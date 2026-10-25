package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	actionTimeout = 30 * time.Second
	cycleTimeout  = 10 * time.Minute
	maxActionForm = 1 << 20
)

func parseActionForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxActionForm)
	return r.ParseForm()
}

func (s *Server) handleWANReplace(w http.ResponseWriter, r *http.Request) {
	if s.rejectCrossSite(w, r) {
		return
	}
	if !s.requireData(w, r, "/wan") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	if err := s.opts.Data.Replace(ctx); err != nil {
		s.opts.Logger.Warn("control replace failed", "err", sanitizeMessage(err.Error()))
		s.redirectFlash(w, r, "/wan", "replace failed: "+err.Error(), true)
		return
	}
	s.redirectFlash(w, r, "/wan", "active WAN replaced", false)
}

func (s *Server) handleWANRemove(w http.ResponseWriter, r *http.Request) {
	if s.rejectCrossSite(w, r) {
		return
	}
	if !s.requireData(w, r, "/wan") {
		return
	}
	if err := parseActionForm(w, r); err != nil {
		s.redirectFlash(w, r, "/wan", "invalid form submission", true)
		return
	}
	full, err := s.resolveReadyHash(r.FormValue("hash"))
	if err != nil {
		s.redirectFlash(w, r, "/wan", err.Error(), true)
		return
	}
	if err := s.opts.Data.RemoveReady(full); err != nil {
		s.opts.Logger.Warn("control remove ready failed", "err", sanitizeMessage(err.Error()))
		s.redirectFlash(w, r, "/wan", "remove failed: "+err.Error(), true)
		return
	}
	s.redirectFlash(w, r, "/wan", "ready entry removed", false)
}

func (s *Server) handleCycleRun(w http.ResponseWriter, r *http.Request) {
	if s.rejectCrossSite(w, r) {
		return
	}
	if !s.requireData(w, r, "/tests") {
		return
	}
	if s.opts.Data.Cycling() {
		s.redirectFlash(w, r, "/tests", "a cycle is already running", false)
		return
	}
	// RunCycle may return after starting the cycle. Do not make the browser
	// wait: start it in a bounded background goroutine and redirect now.
	s.runCycleAsync(s.opts.Data)
	s.redirectFlash(w, r, "/tests", "cycle run requested", false)
}

func (s *Server) runCycleAsync(data Data) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.opts.Logger.Error("control async cycle panic", "panic", panicText(rec))
			}
		}()
		base := s.baseCtx
		if base == nil {
			base = context.Background()
		}
		ctx, cancel := context.WithTimeout(base, cycleTimeout)
		defer cancel()
		if err := data.RunCycle(ctx); err != nil {
			s.opts.Logger.Warn("control cycle failed", "err", sanitizeMessage(err.Error()))
		}
	}()
}

func (s *Server) handleCycleCancel(w http.ResponseWriter, r *http.Request) {
	if s.rejectCrossSite(w, r) {
		return
	}
	if !s.requireData(w, r, "/tests") {
		return
	}
	s.opts.Data.CancelCycle()
	s.redirectFlash(w, r, "/tests", "cycle cancel requested", false)
}

func (s *Server) handleSubscriptionAdd(w http.ResponseWriter, r *http.Request) {
	if s.rejectCrossSite(w, r) {
		return
	}
	if !s.requireData(w, r, "/subscriptions") {
		return
	}
	if err := parseActionForm(w, r); err != nil {
		s.redirectFlash(w, r, "/subscriptions", "invalid form submission", true)
		return
	}
	raw := strings.TrimSpace(r.FormValue("url"))
	if err := validateSubscriptionURL(raw); err != nil {
		s.redirectFlash(w, r, "/subscriptions", err.Error(), true)
		return
	}
	if err := s.opts.Data.AddSubscription(raw); err != nil {
		s.opts.Logger.Warn("control add subscription failed", "err", sanitizeMessage(err.Error()))
		s.redirectFlash(w, r, "/subscriptions", "add failed: "+err.Error(), true)
		return
	}
	s.redirectFlash(w, r, "/subscriptions", "subscription added", false)
}

func (s *Server) handleSubscriptionRemove(w http.ResponseWriter, r *http.Request) {
	if s.rejectCrossSite(w, r) {
		return
	}
	if !s.requireData(w, r, "/subscriptions") {
		return
	}
	if err := parseActionForm(w, r); err != nil {
		s.redirectFlash(w, r, "/subscriptions", "invalid form submission", true)
		return
	}
	raw, err := s.subscriptionForRemoval(r.FormValue("key"), r.FormValue("url"))
	if err != nil {
		s.redirectFlash(w, r, "/subscriptions", err.Error(), true)
		return
	}
	if err := s.opts.Data.RemoveSubscription(raw); err != nil {
		s.opts.Logger.Warn("control remove subscription failed", "err", sanitizeMessage(err.Error()))
		s.redirectFlash(w, r, "/subscriptions", "remove failed: "+err.Error(), true)
		return
	}
	s.redirectFlash(w, r, "/subscriptions", "subscription removed", false)
}

// subscriptionForRemoval resolves an opaque key (preferred, so the URL never
// needs to be embedded in HTML) or an exact URL (legacy form posts).
func (s *Server) subscriptionForRemoval(key, legacyURL string) (string, error) {
	key = strings.TrimSpace(key)
	subs := s.subscriptions()
	if key != "" {
		match := ""
		for _, raw := range subs {
			if subKey(raw) != key {
				continue
			}
			if match != "" && match != raw {
				return "", errors.New("subscription key is ambiguous")
			}
			match = raw
		}
		if match == "" {
			return "", errors.New("subscription not found")
		}
		return match, nil
	}
	legacyURL = strings.TrimSpace(legacyURL)
	if legacyURL != "" {
		for _, raw := range subs {
			if raw == legacyURL {
				return raw, nil
			}
		}
	}
	return "", errors.New("subscription not found")
}
