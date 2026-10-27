package wan

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dissident/internal/config"
)

func TestSubscriptionsMissingFileAndFiltering(t *testing.T) {
	m, _, _ := testManager(t, nil)
	if err := os.Remove(m.currentConfig().Paths.URLsFile); err != nil {
		t.Fatalf("remove urls file: %v", err)
	}
	if got := m.Subscriptions(); len(got) != 0 {
		t.Fatalf("missing file subscriptions = %v, want empty", got)
	}

	path := m.currentConfig().Paths.URLsFile
	content := strings.Join([]string{
		"# comment",
		testLink("share").Raw,
		"https://sub.example.com/path?token=secret",
		"http://127.0.0.1:1/plain",
		"ftp://example.com/file",
		"not a url",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := m.Subscriptions()
	want := []string{
		"https://sub.example.com/path?token=secret",
		"http://127.0.0.1:1/plain",
	}
	if len(got) != len(want) {
		t.Fatalf("subscriptions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("subscriptions = %v, want %v", got, want)
		}
	}
}

func TestAddAndRemoveSubscription(t *testing.T) {
	root := t.TempDir()
	m, _, _ := testManager(t, func(c *config.Config) {
		c.Paths.URLsFile = filepath.Join(root, "nested", "deep", "urls.txt")
		c.Test.HTTPTimeout = 20 * time.Millisecond
	})
	path := m.currentConfig().Paths.URLsFile

	// Preserve non-subscription lines already in the file.
	share := testLink("share").Raw
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create urls parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(share+"\n"), 0o600); err != nil {
		t.Fatalf("prime urls file: %v", err)
	}

	for _, bad := range []string{"", "not-a-url", "ftp://example.com/x", "://bad"} {
		if err := m.AddSubscription(bad); err == nil {
			t.Fatalf("AddSubscription(%q) accepted an invalid URL", bad)
		}
	}
	first := "http://127.0.0.1:1/one"
	second := "http://127.0.0.1:1/two"
	if err := m.AddSubscription(first); err != nil {
		t.Fatalf("AddSubscription: %v", err)
	}
	if err := m.AddSubscription(first); err != nil {
		t.Fatalf("AddSubscription duplicate: %v", err)
	}
	if err := m.AddSubscription(second); err != nil {
		t.Fatalf("AddSubscription second: %v", err)
	}
	if got := m.Subscriptions(); len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("subscriptions = %v", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat urls file: %v", err)
	}
	if mode := info.Mode().Perm(); runtime.GOOS != "windows" && mode != 0o600 {
		t.Fatalf("urls file mode = %o, want 600", mode)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat urls dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); runtime.GOOS != "windows" && mode != 0o755 {
		t.Fatalf("urls dir mode = %o, want 755", mode)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), share) {
		t.Fatalf("share link was lost while rewriting urls file: %q", string(data))
	}

	if err := m.RemoveSubscription(first); err != nil {
		t.Fatalf("RemoveSubscription: %v", err)
	}
	if got := m.Subscriptions(); len(got) != 1 || got[0] != second {
		t.Fatalf("subscriptions after remove = %v", got)
	}
	if err := m.RemoveSubscription(first); err != nil {
		t.Fatalf("RemoveSubscription absent = %v, want nil", err)
	}
	if err := m.RemoveSubscription(""); err == nil {
		t.Fatal("RemoveSubscription with empty URL should error")
	}

	// AddSubscription triggers an asynchronous cycle; with a local refused
	// URL it finishes quickly and records timestamps.
	eventually(t, 2*time.Second, "subscription-triggered cycle", func() bool {
		return !m.LastCycle().IsZero()
	})
}

func TestAddSubscriptionCreatesFileWhenMissing(t *testing.T) {
	m, _, _ := testManager(t, nil)
	path := m.currentConfig().Paths.URLsFile
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove empty urls file: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: urls file should be missing")
	}
	if err := m.AddSubscription("http://127.0.0.1:1/sub"); err != nil {
		t.Fatalf("AddSubscription: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read created urls file: %v", err)
	}
	if !strings.Contains(string(data), "http://127.0.0.1:1/sub") {
		t.Fatalf("urls file = %q", string(data))
	}
}
