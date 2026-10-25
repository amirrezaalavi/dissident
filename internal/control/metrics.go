package control

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
)

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if snap.Active == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: no active WAN\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	active := 0
	if snap.Active != nil {
		active = 1
	}
	cycling := 0
	if s.cycling() {
		cycling = 1
	}
	lastCycle := int64(0)
	if t := s.lastCycle(); !t.IsZero() {
		lastCycle = t.Unix()
		if lastCycle < 0 {
			lastCycle = 0
		}
	}

	var b strings.Builder
	writeMetric := func(name, help, kind string, value any) {
		fmt.Fprintf(&b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, kind)
		fmt.Fprintf(&b, "%s %v\n", name, value)
	}

	writeMetric("dissident_up", "Whether the dissident process is serving.", "gauge", 1)
	writeMetric("dissident_config_generation", "Current accepted configuration generation.", "gauge", snap.Generation)
	writeMetric("dissident_active_wan", "Whether an active WAN is installed (0 or 1).", "gauge", active)
	writeMetric("dissident_ready_entries", "Number of standby links ready for promotion.", "gauge", len(snap.Ready))
	writeMetric("dissident_catalog_entries", "Number of catalog entries.", "gauge", snap.CatalogCount)
	writeMetric("dissident_supported_entries", "Number of catalog entries supported by the renderer.", "gauge", snap.SupportedCount)
	writeMetric("dissident_testing_entries", "Number of entries currently under test.", "gauge", snap.Testing)
	writeMetric("dissident_cycle_running", "Whether a qualification cycle is running (0 or 1).", "gauge", cycling)
	writeMetric("dissident_last_cycle_timestamp", "Unix timestamp of the last completed qualification cycle (0 if none).", "gauge", lastCycle)

	version := s.opts.Version
	if version == "" {
		version = "dev"
	}
	fmt.Fprintf(&b, "# HELP dissident_build_info Build and runtime information.\n")
	fmt.Fprintf(&b, "# TYPE dissident_build_info gauge\n")
	fmt.Fprintf(&b, "dissident_build_info{version=\"%s\",goversion=\"%s\"} 1\n",
		promLabel(version), promLabel(runtime.Version()))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

func promLabel(s string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"\n", "\\n",
	).Replace(s)
}
