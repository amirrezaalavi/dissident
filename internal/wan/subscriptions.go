package wan

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const urlsFileMode = 0o600

// Subscriptions lists the http(s) subscription sources in paths.urls_file.
// Share links and comments are filtered out: they are not subscriptions and
// may contain credentials. A missing file is an empty list, not an error.
func (m *Manager) Subscriptions() []string {
	path := m.currentConfig().Paths.URLsFile
	lines, err := readSourceLines(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.logger.Warn("wan: reading subscriptions failed", "path", path, "err", err)
		}
		return nil
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if isHTTPSource(line) {
			out = append(out, line)
		}
	}
	return out
}

// AddSubscription validates an http(s) source URL, appends it as one line, and
// triggers an asynchronous cycle. The file is rewritten atomically with 0600
// permissions; missing parent directories are created 0755.
func (m *Manager) AddSubscription(raw string) error {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	raw = strings.TrimSpace(raw)
	if !isHTTPSource(raw) {
		return fmt.Errorf("wan: subscription %q must be an absolute http or https URL", raw)
	}
	path := m.currentConfig().Paths.URLsFile
	lines, err := readSourceLines(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("wan: read subscriptions: %w", err)
	}
	for _, line := range lines {
		if line == raw {
			return nil
		}
	}
	lines = append(lines, raw)
	if err := writeSourceLines(path, lines); err != nil {
		return err
	}
	m.logger.Info("wan: subscription added", "url", redactURL(raw))
	m.triggerCycle("subscription added")
	return nil
}

// RemoveSubscription removes every line exactly equal to raw. Removing an
// absent URL is a no-op; a missing file has nothing to remove.
func (m *Manager) RemoveSubscription(raw string) error {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("wan: remove subscription: empty URL")
	}
	path := m.currentConfig().Paths.URLsFile
	lines, err := readSourceLines(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("wan: read subscriptions: %w", err)
	}
	out := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		if line == raw {
			removed = true
			continue
		}
		out = append(out, line)
	}
	if !removed {
		return nil
	}
	if err := writeSourceLines(path, out); err != nil {
		return err
	}
	m.logger.Info("wan: subscription removed", "url", redactURL(raw))
	return nil
}

func readSourceLines(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

func writeSourceLines(path string, lines []string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("wan: empty urls_file path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("wan: create urls directory: %w", err)
	}
	var b strings.Builder
	for _, line := range lines {
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(dir, ".dissident-urls-*.tmp")
	if err != nil {
		return fmt.Errorf("wan: create temporary urls file: %w", err)
	}
	name := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(name)
	}
	if err := tmp.Chmod(urlsFileMode); err != nil {
		cleanup()
		return fmt.Errorf("wan: chmod urls file: %w", err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		cleanup()
		return fmt.Errorf("wan: write urls file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("wan: sync urls file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("wan: close urls file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("wan: install urls file: %w", err)
	}
	_ = os.Chmod(path, urlsFileMode)
	return nil
}

func isHTTPSource(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, " \t") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	return true
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid url)"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
