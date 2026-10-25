// Package logging provides the process-wide slog logger (stderr plus
// log_dir/dissident.log) and the TSV access logger, both with size-based
// gzip rotation and retention cleanup.
package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"dissident/internal/contract"
)

// Options configures the process loggers.
type Options struct {
	// Dir is the log directory. It is created if missing and must be set.
	Dir string
	// Level is debug, info, warn, or error. Empty means info.
	Level string
	// Access enables the TSV access log at Dir/access.log.
	Access bool
	// RetentionDays is the maximum age of a compressed archive. Zero means
	// 30 days.
	RetentionDays int
	// MaxSizeMB is the size at which a log file rotates. Zero means 100 MiB.
	MaxSizeMB int
	// Stderr receives system log lines. Nil means os.Stderr.
	Stderr io.Writer
}

// Logging owns the system slog logger and the access logger.
type Logging struct {
	logger       *slog.Logger
	level        *slog.LevelVar
	levelName    string
	systemFile   *RotatingFile
	accessLogger *AccessLogger
	accessFile   *RotatingFile

	closeOnce sync.Once
	closeErr  error
}

// New creates the log directory, opens dissident.log and (optionally)
// access.log, and returns the pair of loggers.
func New(opts Options) (*Logging, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("logging: log directory is empty")
	}
	if opts.Level == "" {
		opts.Level = "info"
	}
	if opts.RetentionDays <= 0 {
		opts.RetentionDays = 30
	}
	if opts.MaxSizeMB <= 0 {
		opts.MaxSizeMB = 100
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}

	lvl, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	levelVar := new(slog.LevelVar)
	levelVar.Set(lvl)

	maxSize := int64(opts.MaxSizeMB) * 1024 * 1024
	retention := time.Duration(opts.RetentionDays) * 24 * time.Hour

	systemFile, err := OpenRotating(RotationOptions{
		Dir:       opts.Dir,
		BaseName:  "dissident.log",
		MaxSize:   maxSize,
		Retention: retention,
	})
	if err != nil {
		return nil, err
	}

	l := &Logging{
		level:      levelVar,
		levelName:  opts.Level,
		systemFile: systemFile,
	}
	handler := slog.NewTextHandler(io.MultiWriter(opts.Stderr, systemFile), &slog.HandlerOptions{
		Level: levelVar,
	})
	l.logger = slog.New(handler)

	if opts.Access {
		accessFile, err := OpenRotating(RotationOptions{
			Dir:       opts.Dir,
			BaseName:  "access.log",
			MaxSize:   maxSize,
			Retention: retention,
		})
		if err != nil {
			_ = systemFile.Close()
			return nil, err
		}
		l.accessFile = accessFile
		l.accessLogger = newAccessLogger(accessFile, accessFile.Close)
	}
	return l, nil
}

// System returns the system logger. It writes text records to stderr and to
// Dir/dissident.log.
func (l *Logging) System() *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l.logger
}

// Access writes one TSV access record. It is a no-op when access logging is
// disabled.
func (l *Logging) Access(rec contract.AccessRecord) error {
	if l == nil || l.accessLogger == nil {
		return nil
	}
	return l.accessLogger.Log(rec)
}

// SetLevel changes the system logger level at runtime.
func (l *Logging) SetLevel(name string) error {
	if l == nil {
		return errors.New("logging: nil logger")
	}
	lvl, err := ParseLevel(name)
	if err != nil {
		return err
	}
	l.level.Set(lvl)
	l.levelName = name
	return nil
}

// Level returns the current system log level.
func (l *Logging) Level() slog.Level {
	if l == nil {
		return slog.LevelInfo
	}
	return l.level.Level()
}

// Close closes both active log files. It is idempotent.
func (l *Logging) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		var errs []error
		if l.accessLogger != nil {
			if err := l.accessLogger.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if l.systemFile != nil {
			if err := l.systemFile.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		l.closeErr = errors.Join(errs...)
	})
	return l.closeErr
}

// ParseLevel maps a configuration level name to slog.Level.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: unknown level %q (want debug, info, warn, or error)", name)
	}
}
