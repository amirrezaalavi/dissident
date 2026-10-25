package catalog

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dissident/internal/contract"
	"dissident/internal/uri"
)

const (
	linkOne   = "vless://11111111-1111-1111-1111-111111111111@one.example:443?security=tls&type=tcp#one"
	linkTwo   = "ss://YWVzLTI1Ni1nY206c2VjcmV0@two.example:8388#two"
	linkThree = "socks5://127.0.0.1:1080#three"
)

func parseLink(t *testing.T, raw string) contract.Link {
	t.Helper()
	link, err := uri.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return link
}

func TestStoreMergeStatsAndMark(t *testing.T) {
	var zero Store
	if added := zero.Merge([]contract.Link{parseLink(t, linkOne)}); added != 1 {
		t.Fatalf("zero-value merge added %d", added)
	}

	store := New()
	one := parseLink(t, linkOne)
	two := parseLink(t, linkTwo)
	three := parseLink(t, linkThree)

	if added := store.Merge([]contract.Link{one, two}); added != 2 {
		t.Fatalf("first merge added %d, want 2", added)
	}
	if added := store.Merge([]contract.Link{one, three}); added != 1 {
		t.Fatalf("second merge added %d, want 1", added)
	}
	total, supported, testing := store.Stats()
	if total != 3 || supported != 3 || testing != 0 {
		t.Fatalf("stats = %d,%d,%d", total, supported, testing)
	}

	store.Mark(one.Hash, contract.StateTesting, contract.Result{Hash: one.Hash})
	if _, _, testing := store.Stats(); testing != 1 {
		t.Fatalf("testing = %d", testing)
	}

	result := contract.Result{
		Hash:       one.Hash,
		Stage:      contract.StageStability,
		OK:         true,
		Reason:     "ok",
		At:         time.Now().UTC(),
		TCPLatency: 20 * time.Millisecond,
		TTFB:       100 * time.Millisecond,
		SpeedMbps:  25,
		HTTPStatus: 204,
		Bytes:      4096,
	}
	store.Mark(one.Hash, contract.StateReady, result)
	entry := findEntry(t, store, one.Hash)
	if entry.State != contract.StateReady {
		t.Fatalf("state = %s", entry.State)
	}
	if entry.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", entry.Attempts)
	}
	if entry.Score <= 0 {
		t.Fatalf("score = %f", entry.Score)
	}
	if entry.Last.Reason != "ok" || entry.Last.SpeedMbps != 25 || entry.Last.HTTPStatus != 204 {
		t.Fatalf("last result = %+v", entry.Last)
	}
	if entry.Updated.IsZero() {
		t.Fatal("updated time not set")
	}

	store.Mark(two.Hash, contract.StateUnsupported, contract.Result{OK: false, Reason: "parse: nope"})
	if _, supported, _ := store.Stats(); supported != 2 {
		t.Fatalf("supported = %d, want 2", supported)
	}
	unsupported := findEntry(t, store, two.Hash)
	if unsupported.Score != 0 {
		t.Fatalf("failed score = %f", unsupported.Score)
	}

	before := len(store.Entries())
	store.Mark("missing", contract.StateReady, contract.Result{OK: true})
	if after := len(store.Entries()); after != before {
		t.Fatalf("unknown mark changed entries: %d -> %d", before, after)
	}
}

func findEntry(t *testing.T, store *Store, hash string) contract.Entry {
	t.Helper()
	for _, entry := range store.Entries() {
		if entry.Link.Hash == hash {
			return entry
		}
	}
	t.Fatalf("entry %s not found", hash)
	return contract.Entry{}
}

func TestStoreEntriesOrderingAndCopy(t *testing.T) {
	store := New()
	one := parseLink(t, linkOne)
	two := parseLink(t, linkTwo)
	three := parseLink(t, linkThree)
	store.Merge([]contract.Link{one, two, three})

	store.Mark(one.Hash, contract.StateActive, contract.Result{OK: true, SpeedMbps: 1})
	store.Mark(two.Hash, contract.StateReady, contract.Result{OK: true, SpeedMbps: 50})
	store.Mark(three.Hash, contract.StateFailed, contract.Result{OK: false})

	entries := store.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].State != contract.StateActive || entries[1].State != contract.StateReady || entries[2].State != contract.StateFailed {
		t.Fatalf("states = %s,%s,%s", entries[0].State, entries[1].State, entries[2].State)
	}

	// Entries must be a deep copy.
	entries[0].Link.Params["injected"] = "yes"
	stored := findEntry(t, store, one.Hash)
	if stored.Link.Params["injected"] != "" {
		t.Fatal("Entries exposed the stored params map")
	}

	// Same-state ordering is by score descending.
	other := New()
	other.Merge([]contract.Link{one, two})
	other.Mark(one.Hash, contract.StateReady, contract.Result{OK: true, SpeedMbps: 1})
	other.Mark(two.Hash, contract.StateReady, contract.Result{OK: true, SpeedMbps: 100})
	ordered := other.Entries()
	if ordered[0].Link.Hash != two.Hash {
		t.Fatalf("higher score did not sort first: %+v", ordered)
	}
}

func TestStorePendingOrdering(t *testing.T) {
	store := New()
	one := parseLink(t, linkOne)
	two := parseLink(t, linkTwo)
	three := parseLink(t, linkThree)
	store.Merge([]contract.Link{one, two, three})

	// one: 20ms, two: zero, three: 5ms.
	store.Mark(one.Hash, contract.StateCatalog, contract.Result{TCPLatency: 20 * time.Millisecond})
	time.Sleep(2 * time.Millisecond)
	store.Mark(two.Hash, contract.StateCatalog, contract.Result{})
	time.Sleep(2 * time.Millisecond)
	store.Mark(three.Hash, contract.StateFailed, contract.Result{TCPLatency: 5 * time.Millisecond})

	pending := store.Pending(0)
	if len(pending) != 3 {
		t.Fatalf("pending = %d", len(pending))
	}
	if pending[0].Link.Hash != three.Hash || pending[1].Link.Hash != one.Hash || pending[2].Link.Hash != two.Hash {
		t.Fatalf("pending order = %s,%s,%s", pending[0].Link.Hash, pending[1].Link.Hash, pending[2].Link.Hash)
	}
	if limited := store.Pending(2); len(limited) != 2 || limited[0].Link.Hash != three.Hash {
		t.Fatalf("limited pending = %+v", limited)
	}

	// Ready entries are not pending.
	store.Mark(three.Hash, contract.StateReady, contract.Result{OK: true})
	for _, entry := range store.Pending(0) {
		if entry.Link.Hash == three.Hash {
			t.Fatal("ready entry returned as pending")
		}
	}

	// Equal latency sorts by oldest Updated first.
	other := New()
	other.Merge([]contract.Link{one, two})
	other.Mark(one.Hash, contract.StateCatalog, contract.Result{TCPLatency: time.Millisecond})
	time.Sleep(3 * time.Millisecond)
	other.Mark(two.Hash, contract.StateCatalog, contract.Result{TCPLatency: time.Millisecond})
	if ordered := other.Pending(0); ordered[0].Link.Hash != one.Hash {
		t.Fatalf("older entry did not sort first: %+v", ordered)
	}
}

func TestParseSources(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		data := []byte("# comment\n" + linkOne + "\nnot-a-link\n")
		links, problems := ParseSources(data)
		if len(links) != 1 || links[0].Hash != parseLink(t, linkOne).Hash {
			t.Fatalf("links = %+v", links)
		}
		if len(problems) != 1 || !strings.Contains(problems[0].Error(), "line 3") {
			t.Fatalf("problems = %v", problems)
		}
	})

	t.Run("base64", func(t *testing.T) {
		payload := base64.StdEncoding.EncodeToString([]byte(linkOne + "\n" + linkTwo + "\n"))
		links, problems := ParseSources([]byte(payload))
		if len(links) != 2 || len(problems) != 0 {
			t.Fatalf("links = %d, problems = %v", len(links), problems)
		}
	})

	t.Run("base64 bad line", func(t *testing.T) {
		payload := base64.StdEncoding.EncodeToString([]byte(linkOne + "\nbroken\n"))
		links, problems := ParseSources([]byte(payload))
		if len(links) != 1 || len(problems) != 1 {
			t.Fatalf("links = %d, problems = %v", len(links), problems)
		}
	})
}

func TestLoadSources(t *testing.T) {
	goodPayload := linkOne + "\n" + linkTwo + "\n"
	good := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(goodPayload))
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	localLink := linkThree
	path := filepath.Join(t.TempDir(), "urls.txt")
	content := strings.Join([]string{
		"# sources",
		good.URL,
		bad.URL,
		localLink,
		linkOne, // duplicate across a subscription and a direct link
		"broken-share-link",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	links, problems, err := LoadSources(ctx, path, 2*time.Second)
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if len(links) != 3 {
		t.Fatalf("links = %d, want 3", len(links))
	}
	hashes := map[string]bool{}
	for _, link := range links {
		hashes[link.Hash] = true
	}
	for _, raw := range []string{linkOne, linkTwo, localLink} {
		if !hashes[parseLink(t, raw).Hash] {
			t.Fatalf("missing link %s in %+v", raw, links)
		}
	}
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want 2", problems)
	}
	joined := ""
	for _, problem := range problems {
		joined += problem.Error() + "\n"
	}
	if !strings.Contains(joined, "HTTP 500") || !strings.Contains(joined, "line 6") {
		t.Fatalf("problems = %v", problems)
	}
}

func TestLoadSourcesRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(linkTwo + "\n"))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	path := filepath.Join(t.TempDir(), "urls.txt")
	if err := os.WriteFile(path, []byte(redirect.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	links, problems, err := LoadSources(context.Background(), path, 2*time.Second)
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if len(links) != 1 || len(problems) != 0 {
		t.Fatalf("links = %d, problems = %v", len(links), problems)
	}
}

func TestLoadSourcesErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, _, err := LoadSources(context.Background(), filepath.Join(t.TempDir(), "absent.txt"), time.Second); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("only bad sources", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.txt")
		if err := os.WriteFile(path, []byte("broken\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		links, problems, err := LoadSources(context.Background(), path, time.Second)
		if err == nil {
			t.Fatal("expected error")
		}
		if len(links) != 0 || len(problems) != 1 {
			t.Fatalf("links = %d, problems = %v", len(links), problems)
		}
	})

	t.Run("comments only", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "comments.txt")
		if err := os.WriteFile(path, []byte("# nothing here\n\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		links, problems, err := LoadSources(context.Background(), path, time.Second)
		if err != nil || links != nil || problems != nil {
			t.Fatalf("links = %v, problems = %v, err = %v", links, problems, err)
		}
	})
}
