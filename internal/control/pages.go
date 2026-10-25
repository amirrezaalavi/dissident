package control

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"dissident/internal/contract"
)

type overviewPage struct {
	baseData
	Snapshot      contract.Snapshot
	Active        *activeView
	ReadyCount    int
	ReadyTarget   int
	ReadyMin      int
	ReadyBelowMin bool
	Ready         []readyRow
	Recent        []testRow
	CycleState    string
	LastCycle     string
	NextCycle     string
	LastError     string
	HasError      bool
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	p := overviewPage{
		baseData:    s.newBase(r, "Overview", "overview"),
		Snapshot:    snap,
		Active:      activeViewFrom(snap.Active),
		ReadyCount:  len(snap.Ready),
		ReadyTarget: s.readyTarget(),
		ReadyMin:    s.readyMin(),
		Ready:       readyRows(snap.Ready, 8, false),
	}
	p.Refresh = true
	p.ReadyBelowMin = p.ReadyCount < p.ReadyMin
	if s.opts.Data != nil {
		p.CycleState = "idle"
		if s.cycling() {
			p.CycleState = "running"
		}
		p.LastCycle = fmtTime(s.lastCycle())
		p.NextCycle = fmtTime(s.nextCycle())
		p.LastError = sanitizeReason(s.lastError())
		p.HasError = p.LastError != ""
		p.Recent = recentRows(s.entries(), 10)
	} else {
		p.Notice = "No runtime data provider is attached; this page is in read-only Snapshot mode."
		p.CycleState = "not available"
		p.LastCycle = "unavailable"
		p.NextCycle = "unavailable"
	}
	s.renderPage(w, r, pages.overview, p)
}

type wanPage struct {
	baseData
	Active      *activeView
	Ready       []readyRow
	ReadyCount  int
	ReadyTarget int
	ReadyMin    int
	CanReplace  bool
}

func (s *Server) handleWAN(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	hasData := s.opts.Data != nil
	p := wanPage{
		baseData:    s.newBase(r, "WAN", "wan"),
		Active:      activeViewFrom(snap.Active),
		Ready:       readyRows(snap.Ready, 0, hasData),
		ReadyCount:  len(snap.Ready),
		ReadyTarget: s.readyTarget(),
		ReadyMin:    s.readyMin(),
		CanReplace:  hasData && len(snap.Ready) > 0,
	}
	if !hasData {
		p.Notice = "No runtime data provider is attached; WAN actions are disabled in read-only Snapshot mode."
	}
	s.renderPage(w, r, pages.wan, p)
}

type testsPage struct {
	baseData
	Snapshot   contract.Snapshot
	Rows       []testRow
	CycleState string
	LastCycle  string
	NextCycle  string
	LastError  string
	HasError   bool
	CanRun     bool
	CanCancel  bool
}

func (s *Server) handleTests(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	hasData := s.opts.Data != nil
	p := testsPage{
		baseData: s.newBase(r, "Tests", "tests"),
		Snapshot: snap,
	}
	if hasData {
		p.CycleState = "idle"
		if s.cycling() {
			p.CycleState = "running"
		}
		p.Rows = testRows(s.entries(), 200)
		p.LastCycle = fmtTime(s.lastCycle())
		p.NextCycle = fmtTime(s.nextCycle())
		p.LastError = sanitizeReason(s.lastError())
		p.HasError = p.LastError != ""
		p.CanRun = !s.cycling()
		p.CanCancel = true
	} else {
		p.Notice = "No runtime data provider is attached; catalog details and cycle actions are unavailable in read-only Snapshot mode."
		p.CycleState = "not available"
		p.LastCycle = "unavailable"
		p.NextCycle = "unavailable"
	}
	s.renderPage(w, r, pages.tests, p)
}

type subRow struct {
	Display string
	Key     string
}

type subscriptionsPage struct {
	baseData
	URLs       []subRow
	LocalCount int
	Total      int
}

func (s *Server) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	p := subscriptionsPage{baseData: s.newBase(r, "Subscriptions", "subscriptions")}
	if s.opts.Data == nil {
		p.Notice = "No runtime data provider is attached; subscription sources are unavailable in read-only Snapshot mode."
		s.renderPage(w, r, pages.subscriptions, p)
		return
	}
	subs := s.subscriptions()
	p.Total = len(subs)
	for _, raw := range subs {
		if isHTTPSubscription(raw) {
			p.URLs = append(p.URLs, subRow{Display: subscriptionDisplay(raw), Key: subKey(raw)})
			continue
		}
		// Local share links are never printed, not even a prefix.
		p.LocalCount++
	}
	s.renderPage(w, r, pages.subscriptions, p)
}

type kv struct {
	Key   string
	Value string
}

type kvGroup struct {
	Title string
	Rows  []kv
}

type settingsPage struct {
	baseData
	ConfigAvailable bool
	Generation      uint64
	Groups          []kvGroup
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	p := settingsPage{baseData: s.newBase(r, "Settings", "settings")}
	if s.opts.Config == nil {
		p.Notice = "No configuration manager is attached; settings are unavailable."
		s.renderPage(w, r, pages.settings, p)
		return
	}
	cur := s.opts.Config.Current()
	if cur == nil {
		p.Notice = "No configuration generation is available."
		s.renderPage(w, r, pages.settings, p)
		return
	}
	cfg := cur.Config
	p.ConfigAvailable = true
	p.Generation = cur.Generation
	p.Groups = []kvGroup{
		{
			Title: "Listeners",
			Rows: []kv{
				{"server.panel_listen", sanitizeLabel(cfg.Server.PanelListen, "(not set)")},
				{"server.proxy_listen", sanitizeLabel(cfg.Server.ProxyListen, "(not set)")},
			},
		},
		{
			Title: "Paths",
			Rows: []kv{
				{"paths.urls_file", sanitizePath(cfg.Paths.URLsFile)},
				{"paths.state_file", sanitizePath(cfg.Paths.StateFile)},
				{"paths.log_dir", sanitizePath(cfg.Paths.LogDir)},
				{"paths.xray_bin", sanitizePath(cfg.Paths.XrayBin)},
			},
		},
		{
			Title: "Cycle",
			Rows: []kv{
				{"cycle.interval", durationText(cfg.Cycle.Interval)},
				{"cycle.active_liveness", durationText(cfg.Cycle.ActiveLiveness)},
				{"cycle.active_idle", durationText(cfg.Cycle.ActiveIdle)},
				{"cycle.ready_liveness", durationText(cfg.Cycle.ReadyLiveness)},
				{"cycle.probe_count", strconv.Itoa(cfg.Cycle.ProbeCount)},
				{"cycle.probe_window", durationText(cfg.Cycle.ProbeWindow)},
				{"cycle.probe_budget_per_min", strconv.Itoa(cfg.Cycle.ProbeBudgetPerMin)},
				{"cycle.ready_min", strconv.Itoa(cfg.Cycle.ReadyMin)},
				{"cycle.ready_target", strconv.Itoa(cfg.Cycle.ReadyTarget)},
			},
		},
		{
			Title: "Swap",
			Rows: []kv{
				{"swap.drain_timeout", durationText(cfg.Swap.DrainTimeout)},
				{"swap.promotion_timeout", durationText(cfg.Swap.PromotionTimeout)},
			},
		},
		{
			Title: "Tests",
			Rows: []kv{
				{"test.cheap_workers", strconv.Itoa(cfg.Test.CheapWorkers)},
				{"test.real_workers", strconv.Itoa(cfg.Test.RealWorkers)},
				{"test.real_test_cap", strconv.Itoa(cfg.Test.RealTestCap)},
				{"test.tcp_timeout", durationText(cfg.Test.TCPTimeout)},
				{"test.tls_timeout", durationText(cfg.Test.TLSTimeout)},
				{"test.protocol_timeout", durationText(cfg.Test.ProtocolTimeout)},
				{"test.xray_timeout", durationText(cfg.Test.XrayTimeout)},
				{"test.http_timeout", durationText(cfg.Test.HTTPTimeout)},
				{"test.speed_size", strconv.FormatInt(cfg.Test.SpeedSize, 10)},
				{"test.min_speed_mbps", formatScore(cfg.Test.MinSpeedMbps)},
				{"test.max_ttfb_ms", strconv.Itoa(cfg.Test.MaxTTFBMs)},
				{"test.stability_passes", strconv.Itoa(cfg.Test.StabilityPasses)},
				{"test.content_quorum", strconv.Itoa(cfg.Test.ContentQuorum)},
				{"test.content_min_bytes", strconv.Itoa(cfg.Test.ContentMinBytes)},
			},
		},
		{
			Title: "Domains",
			Rows: []kv{
				{"urls.probe", configURLList(cfg.URLs.Probe)},
				{"urls.verify", configURLList(cfg.URLs.Verify)},
				{"urls.content", configURLList(cfg.URLs.Content)},
				{"urls.speed", configURL(cfg.URLs.Speed)},
				{"urls.speed_fallback", configURL(cfg.URLs.SpeedFallback)},
				{"urls.exit_ip", configURL(cfg.URLs.ExitIP)},
			},
		},
		{
			Title: "Logging",
			Rows: []kv{
				{"log.level", sanitizeLabel(cfg.Log.Level, "(not set)")},
				{"log.access", fmt.Sprintf("%t", cfg.Log.Access)},
				{"log.retention_days", strconv.Itoa(cfg.Log.RetentionDays)},
				{"log.max_size_mb", strconv.Itoa(cfg.Log.MaxSizeMB)},
			},
		},
	}
	s.renderPage(w, r, pages.settings, p)
}

func configURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "(not set)"
	}
	return sanitizeHTTPURL(raw)
}

func configURLList(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(values))
	for _, raw := range values {
		out = append(out, sanitizeHTTPURL(strings.TrimSpace(raw)))
	}
	return strings.Join(out, "\n")
}
