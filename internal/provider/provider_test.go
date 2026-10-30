package provider

import (
	"testing"

	"dissident/internal/contract"
)

func TestClassifyServer(t *testing.T) {
	cases := []struct {
		server string
		want   Kind
	}{
		{"104.16.0.1", Cloudflare},
		{"172.64.150.28", Cloudflare},
		{"173.245.48.5", Cloudflare},
		{"8.8.8.8", NonCloudflare},
		{"11.22.33.44", NonCloudflare},
		{"1.2.3.4", NonCloudflare},
		{"10.0.0.1", Unknown},
		{"127.0.0.1", Unknown},
		{"192.0.2.10", Unknown},
		{"100.64.0.1", Unknown},
		{"2001:db8::1", Unknown},
		{"2606:4700::1111", Unknown},
		{"example.com", Unknown},
		{"", Unknown},
	}
	for _, tc := range cases {
		if got := ClassifyServer(tc.server); got != tc.want {
			t.Errorf("ClassifyServer(%q) = %q, want %q", tc.server, got, tc.want)
		}
	}
}

func TestEndpointAndPrefixKeys(t *testing.T) {
	if got := EndpointKeyHost("Example.COM.", 443); got != "example.com:443" {
		t.Errorf("endpoint hostname = %q", got)
	}
	if got := EndpointKeyHost("104.16.0.1", 8443); got != "104.16.0.1:8443" {
		t.Errorf("endpoint ip = %q", got)
	}
	if got := EndpointKeyHost("104.16.0.1", 0); got != "" {
		t.Errorf("endpoint invalid port = %q, want empty", got)
	}
	if got := PrefixKeyHost("104.16.0.1"); got != "104.16.0.0/24" {
		t.Errorf("prefix ip = %q", got)
	}
	if got := PrefixKeyHost("Example.COM."); got != "host:example.com" {
		t.Errorf("prefix hostname = %q", got)
	}
	if got := PrefixKeyHost("2001:db8::1"); got != "" {
		t.Errorf("prefix ipv6 = %q, want empty", got)
	}
	if got := PrefixKeyHost(""); got != "" {
		t.Errorf("prefix empty = %q, want empty", got)
	}
}

func TestSummarize(t *testing.T) {
	link := func(hash, server string, port int) contract.Link {
		return contract.Link{Hash: hash, Server: server, Port: port}
	}
	ready := []contract.Ready{
		{Link: link("cf-a", "104.16.0.1", 443)},
		{Link: link("cf-b", "104.16.0.2", 443)},
		{Link: link("other", "8.8.8.8", 443)},
		{Link: link("host", "example.com", 443)},
		{Link: link("private", "10.0.0.1", 443)},
	}
	got := Summarize(ready)
	want := Counts{Cloudflare: 2, NonCloudflare: 1, Unknown: 2, Domains: 4}
	if got != want {
		t.Fatalf("Summarize = %+v, want %+v", got, want)
	}

	single := Summarize(ready[:2])
	if !single.SingleProvider || single.Cloudflare != 2 {
		t.Fatalf("single provider = %+v", single)
	}
	mixed := Summarize(ready[:3])
	if mixed.SingleProvider {
		t.Fatalf("mixed pool reported single provider: %+v", mixed)
	}
	if empty := Summarize(nil); empty.SingleProvider || empty.Domains != 0 {
		t.Fatalf("empty summary = %+v", empty)
	}
}
