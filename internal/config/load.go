package config

import (
	"fmt"
	"os"
)

// Load reads, parses, and validates an INI-style configuration file. Missing
// keys keep their defaults. Any parse or validation error returns an empty
// Config, so a bad file can never become a runtime snapshot.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	entries, err := parseINI(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	cfg := Defaults()
	if err := applyEntries(&cfg, entries); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}
