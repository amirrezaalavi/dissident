package wan

import (
	"context"
	"errors"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
	"dissident/internal/platform"
)

func TestRouteBeforeAndAfterPromotion(t *testing.T) {
	m, fr, _ := testManager(t, nil)
	if _, err := m.Route(context.Background()); !errors.Is(err, errNoActive) {
		t.Fatalf("Route with no active = %v, want errNoActive", err)
	}

	link := testLink("route-link")
	setReady(m, testReady("route-link", 7))
	if err := m.Replace(context.Background()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	route, err := m.Route(context.Background())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if route.ID != link.Hash {
		t.Fatalf("route ID = %q, want short hash %q", route.ID, link.Hash)
	}
	dialer, ok := route.Dial.(*platform.SOCKS5Dialer)
	if !ok {
		t.Fatalf("dialer type = %T, want *platform.SOCKS5Dialer", route.Dial)
	}
	if len(dialer.Addr) < 10 || dialer.Addr[:10] != "127.0.0.1:" {
		t.Fatalf("dial addr = %q", dialer.Addr)
	}
	tests, promotes, _ := fr.counts()
	if tests != 0 || promotes != 1 {
		t.Fatalf("tests=%d promotes=%d, want 0/1", tests, promotes)
	}
}

func TestReportThresholdInsideWindow(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ProbeWindow = 80 * time.Millisecond
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	old := testLink("old-route")
	setActive(t, m, old, 27000)
	setReady(m, testReady("new-route", 9))
	route, err := m.Route(context.Background())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	m.Report(context.Background(), route, errTest)
	m.Report(context.Background(), route, errTest)
	time.Sleep(25 * time.Millisecond)
	if got := len(fr.promotionLinks()); got != 0 {
		t.Fatalf("two failures triggered %d promotions, want 0", got)
	}

	m.Report(context.Background(), route, errTest)
	eventually(t, 2*time.Second, "passive-failure replacement", func() bool {
		return len(fr.promotionLinks()) == 1 && activeHash(m) == "new-route"
	})
}

func TestReportFailuresOutsideWindowDoNotReplace(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ProbeWindow = 40 * time.Millisecond
	})
	setActive(t, m, testLink("old-route"), 27001)
	setReady(m, testReady("new-route", 9))
	route, err := m.Route(context.Background())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	m.Report(context.Background(), route, errTest)
	m.Report(context.Background(), route, errTest)
	time.Sleep(70 * time.Millisecond)
	m.Report(context.Background(), route, errTest)
	time.Sleep(120 * time.Millisecond)
	if got := len(fr.promotionLinks()); got != 0 {
		t.Fatalf("failures outside window triggered %d promotions, want 0", got)
	}
	if activeHash(m) != "old-route" {
		t.Fatalf("active changed to %q despite isolated failures", activeHash(m))
	}
}

func TestReportSuccessResetsCounter(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ProbeWindow = 200 * time.Millisecond
	})
	setActive(t, m, testLink("old-route"), 27002)
	setReady(m, testReady("new-route", 9))
	route, err := m.Route(context.Background())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	m.Report(context.Background(), route, errTest)
	m.Report(context.Background(), route, errTest)
	m.Report(context.Background(), route, nil)
	m.Report(context.Background(), route, errTest)
	time.Sleep(100 * time.Millisecond)
	if got := len(fr.promotionLinks()); got != 0 {
		t.Fatalf("success did not reset failure counter; promotions=%d", got)
	}
	if activeHash(m) != "old-route" {
		t.Fatalf("active = %q, want unchanged", activeHash(m))
	}
}

func TestReportNeverBlocksAndCollapsesReplace(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	setActive(t, m, testLink("old-route"), 27003)
	setReady(m, testReady("new-route", 9))

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &fakeProcess{port: 27100, pid: 8800}, nil
	})

	route, err := m.Route(context.Background())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	begin := time.Now()
	for i := 0; i < 12; i++ {
		m.Report(context.Background(), route, errTest)
	}
	if elapsed := time.Since(begin); elapsed > 200*time.Millisecond {
		t.Fatalf("Report blocked for %v", elapsed)
	}
	<-started
	for i := 0; i < 12; i++ {
		m.Report(context.Background(), route, errTest)
	}
	close(release)
	eventually(t, 2*time.Second, "single replacement", func() bool {
		return activeHash(m) == "new-route"
	})
	if got := len(fr.promotionLinks()); got != 1 {
		t.Fatalf("replacements = %d, want exactly 1", got)
	}
}
