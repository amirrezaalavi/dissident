package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	// valid int within range
	if errs := Validate("viberoxy", map[string]string{"WAN_COUNT": "3"}); errs != nil {
		t.Errorf("WAN_COUNT=3: %v", errs)
	}
	// out of range
	if errs := Validate("viberoxy", map[string]string{"WAN_COUNT": "9"}); errs["WAN_COUNT"] == "" {
		t.Error("WAN_COUNT=9 should fail (max 5)")
	}
	// non-int
	if errs := Validate("viberoxy", map[string]string{"WAN_COUNT": "abc"}); errs["WAN_COUNT"] == "" {
		t.Error("WAN_COUNT=abc should fail")
	}
	// enum
	if errs := Validate("viberoxy", map[string]string{"ROUTE_MODE": "bogus"}); errs["ROUTE_MODE"] == "" {
		t.Error("ROUTE_MODE=bogus should fail")
	}
	if errs := Validate("viberoxy", map[string]string{"ROUTE_MODE": "proxy-default"}); errs != nil {
		t.Errorf("ROUTE_MODE=proxy-default: %v", errs)
	}
	// bool
	if errs := Validate("viberoxy", map[string]string{"XRAY_MUX": "yes"}); errs["XRAY_MUX"] == "" {
		t.Error("XRAY_MUX=yes should fail")
	}
	if errs := Validate("viberoxy", map[string]string{"XRAY_MUX": "false"}); errs != nil {
		t.Errorf("XRAY_MUX=false: %v", errs)
	}
	// required
	if errs := Validate("viberoxy", map[string]string{"SUBSCRIBER_URL": ""}); errs["SUBSCRIBER_URL"] == "" {
		t.Error("empty SUBSCRIBER_URL should fail (required)")
	}
	// unknown keys are ignored
	if errs := Validate("viberoxy", map[string]string{"NOT_A_KEY": "x"}); errs != nil {
		t.Errorf("unknown key: %v", errs)
	}
}

func TestStoreUpdateAndValues(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Update viberoxy env with a couple of values.
	path, err := s.Update("viberoxy", map[string]string{
		"WAN_COUNT":   "3",
		"ROUTE_MODE":  "proxy-default",
		"DIRECT_DOMAINS": ".ir",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if filepath.Base(path) != "viberoxy.env" {
		t.Errorf("path = %q, want viberoxy.env", filepath.Base(path))
	}

	// File must be 0600.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", fi.Mode().Perm())
	}

	vals, err := s.Values("viberoxy")
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if vals["WAN_COUNT"] != "3" {
		t.Errorf("WAN_COUNT = %q, want 3", vals["WAN_COUNT"])
	}
	if vals["ROUTE_MODE"] != "proxy-default" {
		t.Errorf("ROUTE_MODE = %q", vals["ROUTE_MODE"])
	}
	if vals["DIRECT_DOMAINS"] != ".ir" {
		t.Errorf("DIRECT_DOMAINS = %q", vals["DIRECT_DOMAINS"])
	}
	// Defaults not present in file but in schema: absent from values unless
	// in process env. We only assert the ones we set.
}

func TestStoreUpdateInvalidRejected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	_, err := s.Update("viberoxy", map[string]string{"WAN_COUNT": "99"})
	if err == nil {
		t.Fatal("expected error for WAN_COUNT=99")
	}
	// No file should have been created.
	if _, statErr := os.Stat(s.FileName("viberoxy")); !os.IsNotExist(statErr) {
		t.Error("file exists after rejected update")
	}
}

func TestStoreUpdateBackup(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if _, err := s.Update("viberoxy", map[string]string{"WAN_COUNT": "3"}); err != nil {
		t.Fatal(err)
	}
	// Second update must create a .bak of the first file.
	if _, err := s.Update("viberoxy", map[string]string{"WAN_COUNT": "5"}); err != nil {
		t.Fatal(err)
	}

	bak := s.FileName("viberoxy") + ".bak"
	lines, err := ReadFileLines(bak)
	if err != nil {
		t.Fatalf("read bak: %v", err)
	}
	found := false
	for _, l := range lines {
		if l == "WAN_COUNT=3" {
			found = true
		}
	}
	if !found {
		t.Errorf("backup does not contain WAN_COUNT=3: %v", lines)
	}
}

func TestStoreValuesMissingFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	vals, err := s.Values("viberoxy")
	if err != nil {
		t.Fatalf("Values on missing file: %v", err)
	}
	if vals == nil {
		t.Error("expected non-nil map")
	}
	// Missing file → schema defaults, so the form shows the effective config.
	if vals["WAN_COUNT"] != "4" {
		t.Errorf("WAN_COUNT = %q, want default 4", vals["WAN_COUNT"])
	}
	if vals["XRAY_MUX"] != "true" {
		t.Errorf("XRAY_MUX = %q, want default true", vals["XRAY_MUX"])
	}
}

func TestStoreValuesFileWinsOverDefault(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if _, err := s.Update("viberoxy", map[string]string{"WAN_COUNT": "2"}); err != nil {
		t.Fatal(err)
	}
	vals, err := s.Values("viberoxy")
	if err != nil {
		t.Fatal(err)
	}
	if vals["WAN_COUNT"] != "2" {
		t.Errorf("WAN_COUNT = %q, want 2 (file wins)", vals["WAN_COUNT"])
	}
	// Unset keys still fall back to defaults (complete effective config).
	if vals["XRAY_MUX"] != "true" {
		t.Errorf("XRAY_MUX = %q, want default true", vals["XRAY_MUX"])
	}
}

func TestSchemaFieldByKey(t *testing.T) {
	if f := FieldByKey("WAN_COUNT"); f == nil || f.Group != "viberoxy" {
		t.Error("WAN_COUNT not found in schema")
	}
	if f := FieldByKey("DAEMON_PARALLEL"); f == nil || f.Group != "viberayd" {
		t.Error("DAEMON_PARALLEL not found in schema")
	}
	if FieldByKey("NOPE") != nil {
		t.Error("unknown key should return nil")
	}
}
