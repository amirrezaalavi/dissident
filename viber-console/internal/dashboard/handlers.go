package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Handler serves the dashboard API. The store holds the polled snapshots.
type Handler struct {
	store *Store
}

func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

// Routes returns the http mux with all /api routes registered.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", h.handleHealth)
	mux.HandleFunc("/api/overview", h.handleOverview)
	mux.HandleFunc("/api/viberayd/configs", h.handleViberaydConfigs)
	mux.HandleFunc("/api/viberayd/urls", h.handleViberaydURLs)
	mux.HandleFunc("/api/viberoxy/metrics", h.handleViberoxyMetrics)
	mux.HandleFunc("/api/viberoxy/wans", h.handleWANSlots)
	mux.HandleFunc("/api/viberoxy/candidates", h.handleCandidates)
	mux.HandleFunc("/api/viberoxy/cycle/trigger", h.handleTriggerCycle)
	mux.HandleFunc("/api/viberoxy/wans/", h.handleDropWAN)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write json", "error", err)
	}
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	})
}

func (h *Handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	snap := h.store.Get()

	vd := map[string]interface{}{
		"reachable": snap.ViberaydUp,
	}
	if snap.Viberayd != nil {
		vd["stats"] = snap.Viberayd
	}
	if snap.SubURL != "" {
		vd["sub_url"] = snap.SubURL
	}

	vx := map[string]interface{}{
		"reachable": snap.ViberoxyUp,
	}
	if snap.Viberoxy != nil {
		vx["wans"] = map[string]interface{}{
			"active": snap.Viberoxy.WansActive,
			"slots":  snap.Viberoxy.Slots,
		}
		vx["proxy"] = snap.Viberoxy.Proxy
		if snap.Viberoxy.BuildVersion != "" {
			vx["build_info"] = map[string]string{"version": snap.Viberoxy.BuildVersion}
		}
	}
	vx["health"] = map[string]string{
		"healthz": snap.ViberoxyHealth,
		"readyz":  snap.ViberoxyReady,
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"generated_at": snap.GeneratedAt.UTC().Format(time.RFC3339),
		"viberayd":     vd,
		"viberoxy":     vx,
	})
}

func (h *Handler) handleViberaydConfigs(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage < 1 || perPage > 100 {
		perPage = 50
	}
	state := r.URL.Query().Get("state")

	p, err := h.store.FetchConfigs(r.Context(), page, perPage, state)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":   "viberayd unreachable",
			"details": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) handleViberaydURLs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		urls, err := h.store.FetchURLs(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error":   "viberayd unreachable",
				"details": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"urls": urls})

	case http.MethodPut:
		var req struct {
			URLs []string `json:"urls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		valid, invalid := validateURLList(req.URLs)
		if len(invalid) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error":   "invalid url(s)",
				"invalid": invalid,
			})
			return
		}
		urls, err := h.store.ReplaceURLs(r.Context(), valid)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error":   "viberayd unreachable",
				"details": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"urls": urls})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// validateURLList trims lines, drops blanks/comments, and rejects anything
// that is not an http(s) URL. Returns the clean list and the invalid lines.
func validateURLList(urls []string) (valid, invalid []string) {
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if u == "" || strings.HasPrefix(u, "#") {
			continue
		}
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			invalid = append(invalid, u)
			continue
		}
		valid = append(valid, u)
	}
	return valid, invalid
}

func (h *Handler) handleViberoxyMetrics(w http.ResponseWriter, r *http.Request) {
	snap := h.store.Get()
	if !snap.ViberoxyUp || snap.Viberoxy == nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "viberoxy unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, snap.Viberoxy)
}

// handleWANSlots proxies viberoxy's per-slot WAN state endpoint.
func (h *Handler) handleWANSlots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	body, status, err := h.store.FetchWANSlots(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":   "viberoxy unreachable",
			"details": err.Error(),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// handleCandidates proxies viberoxy's candidate pool endpoint.
func (h *Handler) handleCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	body, status, err := h.store.FetchCandidates(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":   "viberoxy unreachable",
			"details": err.Error(),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// handleDropWAN proxies viberoxy's WAN drop endpoint. The URL path must be
// /api/viberoxy/wans/{index}/drop — the index is parsed from the trailing
// segment by stripping the /api/viberoxy/wans/ prefix and /drop suffix.
func (h *Handler) handleDropWAN(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	// Path: /api/viberoxy/wans/{index}/drop
	prefix := "/api/viberoxy/wans/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	rest = strings.TrimSuffix(rest, "/drop")
	index, err := strconv.Atoi(rest)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid WAN index"})
		return
	}
	body, status, err := h.store.DropWAN(r.Context(), index)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":   "viberoxy unreachable",
			"details": err.Error(),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// handleTriggerCycle proxies viberoxy's manual cycle trigger endpoint.
func (h *Handler) handleTriggerCycle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	body, status, err := h.store.TriggerCycle(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":   "viberoxy unreachable",
			"details": err.Error(),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}
