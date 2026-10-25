package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Store reads and writes the daemons' env files with atomic tmp+rename
// semantics and a .bak rollback copy. The env files are the single source of
// truth for daemon configuration.
type Store struct {
	dir string // e.g. /etc/viber or ./config
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// FileName returns the env file path for a group ("viberayd" | "viberoxy").
func (s *Store) FileName(group string) string {
	return filepath.Join(s.dir, group+".env")
}

// Values returns the effective values of the editable fields for a group:
// file values win, missing keys fall back to the schema default. This mirrors
// what the daemon itself runs with (its env file, else its built-in
// defaults), so the UI always shows the running config and Apply never
// clobbers an unset value with an empty string.
func (s *Store) Values(group string) (map[string]string, error) {
	values := map[string]string{}

	// Schema defaults first (the daemon's fallback when the env file lacks
	// a key — do NOT use the console's own process env, it is unrelated).
	for _, f := range Fields {
		if f.Group == group && f.Default != "" {
			values[f.Key] = f.Default
		}
	}

	file := s.FileName(group)

	// File values win over defaults.
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return values, nil // no file yet: defaults only
		}
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return values, nil
}

// Validate checks a proposed update map against the schema. Returns a
// per-key error map (nil = all good).
func Validate(group string, updates map[string]string) map[string]string {
	errs := map[string]string{}
	for _, f := range Fields {
		if f.Group != group {
			continue
		}
		v, ok := updates[f.Key]
		if !ok {
			continue // unchanged
		}
		if f.Required && strings.TrimSpace(v) == "" {
			errs[f.Key] = "required"
			continue
		}
		switch f.Type {
		case TypeInt:
			n, err := strconv.Atoi(v)
			if err != nil {
				errs[f.Key] = "must be an integer"
				continue
			}
			if f.Min != nil && n < *f.Min {
				errs[f.Key] = fmt.Sprintf("min %d", *f.Min)
			}
			if f.Max != nil && n > *f.Max {
				errs[f.Key] = fmt.Sprintf("max %d", *f.Max)
			}
		case TypeBool:
			if _, err := strconv.ParseBool(v); err != nil {
				errs[f.Key] = "must be true or false"
			}
		case TypeEnum:
			ok := false
			for _, e := range f.Enum {
				if v == e {
					ok = true
					break
				}
			}
			if !ok {
				errs[f.Key] = "must be one of " + strings.Join(f.Enum, ", ")
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// Update writes the new values into the group's env file atomically. It
// preserves existing keys not in the update, and writes all known schema
// fields for that group (so the file is a complete representation). The
// previous file is kept as <file>.bak. Returns the path written.
func (s *Store) Update(group string, updates map[string]string) (string, error) {
	if errs := Validate(group, updates); len(errs) > 0 {
		return "", fmt.Errorf("invalid values: %v", errs)
	}

	file := s.FileName(group)
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", s.dir, err)
	}

	current, err := s.Values(group)
	if err != nil {
		return "", err
	}
	for k, v := range updates {
		current[k] = v
	}

	var sb strings.Builder
	sb.WriteString("# Managed by viber-console — changes via the WebUI.\n")
	for _, f := range Fields {
		if f.Group != group {
			continue
		}
		if v, ok := current[f.Key]; ok {
			sb.WriteString(f.Key + "=" + v + "\n")
		}
	}

	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return "", fmt.Errorf("write tmp: %w", err)
	}
	// Keep a backup of the previous file if it exists.
	if _, err := os.Stat(file); err == nil {
		_ = os.Rename(file, file+".bak")
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Rename(file+".bak", file) // rollback rename
		os.Remove(tmp)
		return "", fmt.Errorf("rename: %w", err)
	}
	return file, nil
}

// ReadFileLines is a tiny helper for tests / diagnostics.
func ReadFileLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}
