package caddy_ip_list

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestDefault(t *testing.T) {
	testDefault(t, `url`)
	testDefault(t, `list { }`)
}

func testDefault(t *testing.T, input string) {
	d := caddyfile.NewTestDispenser(input)

	r := URLIPRange{}
	err := r.UnmarshalCaddyfile(d)
	if err != nil {
		t.Errorf("unmarshal error for %q: %v", input, err)
	}

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	err = r.Provision(ctx)
	if err != nil {
		t.Errorf("error provisioning %q: %v", input, err)
	}
	defer r.Cleanup()
}

func TestUnmarshal(t *testing.T) {
	input := `
	list {
	    url https://www.cloudflare.com/ips-v4
		interval 1.5h
		timeout 30s
	}`

	d := caddyfile.NewTestDispenser(input)

	r := URLIPRange{}
	err := r.UnmarshalCaddyfile(d)
	if err != nil {
		t.Errorf("unmarshal error: %v", err)
	}

	expectedInterval := caddy.Duration(90 * time.Minute)
	if expectedInterval != r.Interval {
		t.Errorf("incorrect interval: expected %v, got %v", expectedInterval, r.Interval)
	}

	expectedTimeout := caddy.Duration(30 * time.Second)
	if expectedTimeout != r.Timeout {
		t.Errorf("incorrect timeout: expected %v, got %v", expectedTimeout, r.Timeout)
	}
}

// Simulates being nested in another block.
func TestUnmarshalNested(t *testing.T) {
	input := `{
				list {
				    url https://www.cloudflare.com/ips-v4
					interval 1.5h
					timeout 30s
				}
				other_module 10h
			}`

	d := caddyfile.NewTestDispenser(input)

	// Enter the outer block.
	d.Next()
	d.NextBlock(d.Nesting())

	r := URLIPRange{}
	err := r.UnmarshalCaddyfile(d)
	if err != nil {
		t.Errorf("unmarshal error: %v", err)
	}

	expectedInterval := caddy.Duration(90 * time.Minute)
	if expectedInterval != r.Interval {
		t.Errorf("incorrect interval: expected %v, got %v", expectedInterval, r.Interval)
	}

	expectedTimeout := caddy.Duration(30 * time.Second)
	if expectedTimeout != r.Timeout {
		t.Errorf("incorrect timeout: expected %v, got %v", expectedTimeout, r.Timeout)
	}

	d.Next()
	if d.Val() != "other_module" {
		t.Errorf("cursor at unexpected position, expected 'other_module', got %v", d.Val())
	}
}

// TestProvisionDoesNotFetchWithCache verifies the contract that keeps Caddy
// config loads fast: Provision must serve the persistent cache and never do
// network I/O when a usable cache exists.
func TestProvisionDoesNotFetchWithCache(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte("198.51.100.0/24\n"))
	}))
	defer server.Close()

	input := `
	list {
	    url ` + server.URL + `
	    interval 12h
	}`

	d := caddyfile.NewTestDispenser(input)
	r := URLIPRange{CacheFile: filepath.Join(t.TempDir(), "cache.json")}
	if err := r.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	prefix, err := caddyhttp.CIDRExpressionToPrefix("192.0.2.0/24")
	if err != nil {
		t.Fatalf("prefix: %v", err)
	}
	if err := r.saveToCache([]netip.Prefix{prefix}); err != nil {
		t.Fatalf("saveToCache: %v", err)
	}

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	start := time.Now()
	if err := r.Provision(ctx); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer r.Cleanup()

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Provision took %v; it must not wait on the network", elapsed)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("Provision fetched from the source %d time(s), want 0", n)
	}
	got := r.GetIPRanges(nil)
	if len(got) != 1 || got[0] != prefix {
		t.Errorf("ranges should come from cache: got %v, want %v", got, prefix)
	}
}

// TestColdStartRefreshRetries: with no cache, Provision returns immediately and
// the background refresh retries until the source answers.
func TestColdStartRefreshRetries(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) <= 2 {
			http.Error(w, "fail", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("192.0.2.1/32\n"))
	}))
	defer server.Close()

	input := `
	list {
	    url ` + server.URL + `
	    retries 2
	    interval 12h
	}`

	d := caddyfile.NewTestDispenser(input)
	r := URLIPRange{CacheFile: filepath.Join(t.TempDir(), "cache.json")}
	if err := r.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := r.Provision(ctx); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer r.Cleanup()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(r.GetIPRanges(nil)) == 0 {
		time.Sleep(100 * time.Millisecond)
	}

	if got := r.GetIPRanges(nil); len(got) != 1 {
		t.Fatalf("background refresh did not populate ranges: %v", got)
	}
	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Errorf("expected 3 attempts (2 fails + 1 success), got %d", n)
	}
}

// TestColdStartAllSourcesDown: sources being down must not fail the config load.
func TestColdStartAllSourcesDown(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "fail", http.StatusInternalServerError)
	}))
	defer server.Close()

	input := `
	list {
	    url ` + server.URL + `
	    retries 2
	    interval 12h
	}`

	d := caddyfile.NewTestDispenser(input)
	r := URLIPRange{CacheFile: filepath.Join(t.TempDir(), "cache.json")}
	if err := r.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := r.Provision(ctx); err != nil {
		t.Fatalf("Provision must not fail when sources are down: %v", err)
	}
	defer r.Cleanup()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&hits) < 3 {
		time.Sleep(100 * time.Millisecond)
	}

	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Errorf("expected 3 attempts, got %d", n)
	}
	if got := r.GetIPRanges(nil); len(got) != 0 {
		t.Errorf("expected empty ranges, got %v", got)
	}
}

// TestCleanupStopsRefreshLoop verifies that unloading the module stops the loop.
func TestCleanupStopsRefreshLoop(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte("192.0.2.1/32\n"))
	}))
	defer server.Close()

	input := `
	list {
	    url ` + server.URL + `
	    interval 100ms
	}`

	d := caddyfile.NewTestDispenser(input)
	r := URLIPRange{CacheFile: filepath.Join(t.TempDir(), "cache.json")}
	if err := r.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := r.Provision(ctx); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	// let the loop run, then stop it
	time.Sleep(300 * time.Millisecond)
	if err := r.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	stopped := atomic.LoadInt32(&hits)
	time.Sleep(500 * time.Millisecond)
	if now := atomic.LoadInt32(&hits); now != stopped {
		t.Errorf("refresh loop kept running after Cleanup: %d -> %d requests", stopped, now)
	}
	// Cleanup must be idempotent
	if err := r.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
}
