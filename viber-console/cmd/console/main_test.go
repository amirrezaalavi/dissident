package main

import "testing"

func TestLoadConfigUsesSeparateViberoxyURLs(t *testing.T) {
	t.Setenv("VIBEROXY_METRICS_URL", "http://127.0.0.1:2111")
	t.Setenv("VIBEROXY_API_URL", "http://127.0.0.1:1980")

	cfg := loadConfig()
	if cfg.vxMetrics != "http://127.0.0.1:2111" {
		t.Fatalf("vxMetrics = %q", cfg.vxMetrics)
	}
	if cfg.vxAPI != "http://127.0.0.1:1980" {
		t.Fatalf("vxAPI = %q", cfg.vxAPI)
	}
}
