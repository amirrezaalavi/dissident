package logging

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// defaultMaxBackups bounds the number of retained compressed archives when
// the caller does not specify a limit. Retention-by-age still applies.
const defaultMaxBackups = 50

// RotationOptions configures a size-rotated, gzip-archiving log file.
type RotationOptions struct {
	// Dir is the directory that holds the active file and its archives.
	Dir string
	// BaseName is the active file name, for example "dissident.log".
	BaseName string
	// MaxSize is the rotation threshold in bytes. Values <= 0 disable
	// size-based rotation.
	MaxSize int64
	// Retention is the maximum age of an archive. Values <= 0 disable
	// age-based cleanup.
	Retention time.Duration
	// MaxBackups is the maximum number of archives to keep. Zero selects
	// defaultMaxBackups; a negative value keeps all archives (retention by
	// age still applies).
	MaxBackups int
	// Now is injectable for tests. Nil means time.Now.
	Now func() time.Time
}

// RotatingFile is a concurrency-safe io.WriteCloser that rotates by size,
// gzips the previous file, and prunes old archives. Rotation happens before a
// write that would exceed MaxSize, so a newline-terminated record is not split
// between files unless the record alone is larger than the limit.
type RotatingFile struct {
	mu     sync.Mutex
	opts   RotationOptions
	file   *os.File
	size   int64
	closed bool
}

// OpenRotating creates the directory if needed, opens the active file in
// append mode, and prunes archives left by earlier runs.
func OpenRotating(opts RotationOptions) (*RotatingFile, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("logging: rotation directory is empty")
	}
	if opts.BaseName == "" {
		opts.BaseName = "dissident.log"
	}
	if opts.MaxBackups == 0 {
		opts.MaxBackups = defaultMaxBackups
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("logging: create log directory %s: %w", opts.Dir, err)
	}
	r := &RotatingFile{opts: opts}
	if err := r.openLocked(); err != nil {
		return nil, err
	}
	r.cleanupLocked(opts.Now())
	return r, nil
}

// CurrentPath returns the path of the active log file.
func (r *RotatingFile) CurrentPath() string {
	return filepath.Join(r.opts.Dir, r.opts.BaseName)
}

func (r *RotatingFile) openLocked() error {
	f, err := os.OpenFile(r.CurrentPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("logging: open log file %s: %w", r.CurrentPath(), err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("logging: stat log file %s: %w", r.CurrentPath(), err)
	}
	r.file = f
	r.size = info.Size()
	return nil
}

// Write appends p to the active file, rotating first if needed.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	if r.file == nil {
		if err := r.openLocked(); err != nil {
			return 0, err
		}
	}
	if r.opts.MaxSize > 0 && r.size > 0 && r.size+int64(len(p)) > r.opts.MaxSize {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
		if err := r.openLocked(); err != nil {
			return 0, err
		}
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) rotateLocked() error {
	now := r.opts.Now()
	if err := r.file.Sync(); err != nil {
		return fmt.Errorf("logging: sync log file: %w", err)
	}
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("logging: close log file for rotation: %w", err)
	}
	r.file = nil

	current := r.CurrentPath()
	archive := r.nextArchivePathLocked(now)
	tmp := archive + ".tmp"
	if err := gzipFile(current, tmp, now); err != nil {
		_ = os.Remove(tmp)
		// Put the active file back so no log lines are lost; the next write
		// will retry rotation.
		_ = r.openLocked()
		return err
	}
	if err := os.Rename(tmp, archive); err != nil {
		_ = os.Remove(tmp)
		_ = r.openLocked()
		return fmt.Errorf("logging: publish archive %s: %w", archive, err)
	}
	if err := os.Remove(current); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = r.openLocked()
		return fmt.Errorf("logging: remove rotated log %s: %w", current, err)
	}
	r.size = 0
	r.cleanupLocked(now)
	return nil
}

func (r *RotatingFile) nextArchivePathLocked(now time.Time) string {
	stamp := now.UTC().Format("20060102T150405.000000000Z")
	base := r.opts.BaseName + "." + stamp
	for i := 0; ; i++ {
		name := base + ".gz"
		if i > 0 {
			name = fmt.Sprintf("%s.%d.gz", base, i)
		}
		if _, err := os.Stat(filepath.Join(r.opts.Dir, name)); errors.Is(err, os.ErrNotExist) {
			return filepath.Join(r.opts.Dir, name)
		}
	}
}

func gzipFile(src, dst string, modTime time.Time) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("logging: open log for compression: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("logging: create compressed log %s: %w", dst, err)
	}
	zw := gzip.NewWriter(out)
	zw.Name = filepath.Base(src)
	zw.ModTime = modTime.UTC()
	if _, err := io.Copy(zw, in); err != nil {
		_ = zw.Close()
		_ = out.Close()
		return fmt.Errorf("logging: compress log: %w", err)
	}
	if err := zw.Close(); err != nil {
		_ = out.Close()
		return fmt.Errorf("logging: finish compressed log: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("logging: close compressed log: %w", err)
	}
	return nil
}

// cleanupLocked removes archives older than Retention, then enforces
// MaxBackups. The caller must hold r.mu.
func (r *RotatingFile) cleanupLocked(now time.Time) {
	entries, err := os.ReadDir(r.opts.Dir)
	if err != nil {
		return
	}
	prefix := r.opts.BaseName + "."
	type archive struct {
		path    string
		modTime time.Time
	}
	var archives []archive
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".gz") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if r.opts.Retention > 0 && now.Sub(info.ModTime()) > r.opts.Retention {
			_ = os.Remove(filepath.Join(r.opts.Dir, name))
			continue
		}
		archives = append(archives, archive{path: filepath.Join(r.opts.Dir, name), modTime: info.ModTime()})
	}
	if r.opts.MaxBackups <= 0 {
		return
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].modTime.After(archives[j].modTime) })
	for i := r.opts.MaxBackups; i < len(archives); i++ {
		_ = os.Remove(archives[i].path)
	}
}

// Close closes the active file. It is idempotent.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
