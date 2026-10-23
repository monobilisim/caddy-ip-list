package caddy_ip_list

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestFileSourceNoNetwork: a file source is read without any HTTP traffic and
// its contents are picked up by the background refresh.
func TestFileSourceNoNetwork(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte("198.51.100.0/24\n"))
	}))
	defer server.Close()

	dir := t.TempDir()
	listPath := filepath.Join(dir, "mono.txt")
	if err := os.WriteFile(listPath, []byte("192.0.2.0/24\n# comment\n\n203.0.113.5\n"), 0o644); err != nil {
		t.Fatalf("write list: %v", err)
	}

	input := `
	list {
	    file ` + listPath + `
	    interval 100ms
	}`

	d := caddyfile.NewTestDispenser(input)
	r := URLIPRange{CacheFile: filepath.Join(dir, "cache.json")}
	if err := r.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(r.Paths) != 1 || r.Paths[0] != listPath {
		t.Fatalf("file directive not parsed: %v", r.Paths)
	}

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	start := time.Now()
	if err := r.Provision(ctx); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer r.Cleanup()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Provision took %v; a file source must not wait on anything", elapsed)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(r.GetIPRanges(nil)) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	got := r.GetIPRanges(nil)
	if len(got) != 2 {
		t.Fatalf("expected 2 prefixes from file, got %v", got)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("file source produced %d HTTP requests, want 0", n)
	}

	// Rewriting the file must be visible on the next tick, with no reload.
	if err := os.WriteFile(listPath, []byte("192.0.2.0/24\n"), 0o644); err != nil {
		t.Fatalf("rewrite list: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(r.GetIPRanges(nil)) != 1 {
		time.Sleep(50 * time.Millisecond)
	}
	if got := r.GetIPRanges(nil); len(got) != 1 {
		t.Errorf("file change not picked up: got %v, want 1 prefix", got)
	}
}

// TestEmptyRefreshKeepsPreviousList: an emptied source must not wipe a working
// list, because an empty list makes `not dynamic_client_ip` deny everyone.
func TestEmptyRefreshKeepsPreviousList(t *testing.T) {
	dir := t.TempDir()
	listPath := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(listPath, []byte("192.0.2.0/24\n"), 0o644); err != nil {
		t.Fatalf("write list: %v", err)
	}

	d := caddyfile.NewTestDispenser(`
	list {
	    file ` + listPath + `
	    interval 100ms
	}`)
	r := URLIPRange{CacheFile: filepath.Join(dir, "cache.json")}
	if err := r.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := r.Provision(ctx); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer r.Cleanup()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(r.GetIPRanges(nil)) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if got := r.GetIPRanges(nil); len(got) != 1 {
		t.Fatalf("setup: expected 1 prefix, got %v", got)
	}

	// truncate the source
	if err := os.WriteFile(listPath, []byte("\n\n# only comments\n"), 0o644); err != nil {
		t.Fatalf("truncate list: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	if got := r.GetIPRanges(nil); len(got) != 1 {
		t.Errorf("empty refresh wiped the list: got %v, want the previous prefix kept", got)
	}

	// with allow_empty the empty list is accepted
	d2 := caddyfile.NewTestDispenser(`
	list {
	    file ` + listPath + `
	    interval 12h
	    allow_empty
	}`)
	r2 := URLIPRange{CacheFile: filepath.Join(dir, "cache2.json")}
	if err := r2.UnmarshalCaddyfile(d2); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !r2.AllowEmpty {
		t.Fatal("allow_empty not parsed")
	}
	ctx2, cancel2 := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel2()
	if err := r2.Provision(ctx2); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	// cold start: the initial refresh runs in the background, wait for it
	time.Sleep(300 * time.Millisecond)
	if got := r2.GetIPRanges(nil); len(got) != 0 {
		t.Errorf("allow_empty should accept the empty list, got %v", got)
	}
	if err := r2.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// let any in-flight write finish before the temp dir is removed
	time.Sleep(100 * time.Millisecond)
}

// TestCachePathIncludesFiles: two blocks with different file sources must not
// share a cache file, otherwise one overwrites the other's list.
func TestCachePathIncludesFiles(t *testing.T) {
	a := URLIPRange{Paths: []string{"/a/list.txt"}}
	b := URLIPRange{Paths: []string{"/b/list.txt"}}
	pa, err := a.cachePath()
	if err != nil {
		t.Fatalf("cachePath a: %v", err)
	}
	pb, err := b.cachePath()
	if err != nil {
		t.Fatalf("cachePath b: %v", err)
	}
	if pa == pb {
		t.Errorf("different file sources share a cache file: %s", pa)
	}

	// a URL source must not collide with a file source of the same text
	c := URLIPRange{URLs: []string{"/a/list.txt"}}
	pc, err := c.cachePath()
	if err != nil {
		t.Fatalf("cachePath c: %v", err)
	}
	if pc == pa {
		t.Errorf("URL and file source share a cache file: %s", pa)
	}
}

// TestConcurrentSaveToCache: several instances share one cache file when they
// share sources, so concurrent writes must never leave invalid JSON behind.
func TestConcurrentSaveToCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	prefix, err := caddyhttp.CIDRExpressionToPrefix("192.0.2.0/24")
	if err != nil {
		t.Fatalf("prefix: %v", err)
	}

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := URLIPRange{CacheFile: path}
			r.lock = new(sync.RWMutex)
			for j := 0; j < 25; j++ {
				if err := r.saveToCache([]netip.Prefix{prefix}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("saveToCache: %v", err)
	}

	r := URLIPRange{CacheFile: path}
	r.lock = new(sync.RWMutex)
	got, err := r.loadFromCache()
	if err != nil {
		t.Fatalf("cache left invalid after concurrent writes: %v", err)
	}
	if len(got) != 1 || got[0] != prefix {
		t.Errorf("unexpected cache contents: %v", got)
	}

	// no temp files may be left behind
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "cache.json" {
			t.Errorf("leftover file in cache dir: %s", e.Name())
		}
	}
}

// TestUnchangedFileSkipsParse: with a file source and a short interval, an
// unchanged file must cost a stat and nothing more, or hundreds of instances
// sharing one file burn CPU for nothing.
func TestUnchangedFileSkipsParse(t *testing.T) {
	dir := t.TempDir()
	listPath := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(listPath, []byte("192.0.2.0/24\n"), 0o644); err != nil {
		t.Fatalf("write list: %v", err)
	}

	d := caddyfile.NewTestDispenser(`
	list {
	    file ` + listPath + `
	    interval 100ms
	}`)
	r := URLIPRange{CacheFile: filepath.Join(dir, "cache.json")}
	if err := r.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	r.lock = new(sync.RWMutex)

	r.refreshIfChanged()
	if got := r.GetIPRanges(nil); len(got) != 1 {
		t.Fatalf("expected 1 prefix, got %v", got)
	}

	// unchanged file: the tick must not touch the list
	if r.fileSourcesChanged() {
		t.Error("unchanged file reported as changed")
	}
	before := r.GetIPRanges(nil)
	r.refreshIfChanged()
	if got := r.GetIPRanges(nil); len(got) != len(before) {
		t.Errorf("unchanged file changed the list: %v -> %v", before, got)
	}

	// touching the file must be noticed
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(listPath, []byte("192.0.2.0/24\n198.51.100.0/24\n"), 0o644); err != nil {
		t.Fatalf("rewrite list: %v", err)
	}
	if !r.fileSourcesChanged() {
		t.Error("modified file not detected")
	}
	r.refresh()
	if got := r.GetIPRanges(nil); len(got) != 2 {
		t.Errorf("changed file not applied: %v", got)
	}
}

// TestURLSourceAlwaysRefreshes: a URL source cannot be stat'ed, so every tick
// still fetches (the fetch interval is the whole point for remote lists).
func TestURLSourceAlwaysRefreshes(t *testing.T) {
	r := URLIPRange{URLs: []string{"https://example.invalid/list.txt"}}
	r.lock = new(sync.RWMutex)
	if !r.fileSourcesChanged() {
		t.Error("URL source must always be considered changed")
	}
}

// TestMissingFileStillReported: a stat failure must fall through to the normal
// path so the error is logged instead of being silently swallowed.
func TestMissingFileStillReported(t *testing.T) {
	r := URLIPRange{Paths: []string{filepath.Join(t.TempDir(), "nope.txt")}}
	r.lock = new(sync.RWMutex)
	if !r.fileSourcesChanged() {
		t.Error("missing file must be treated as changed so the error surfaces")
	}
}

// TestNoFileKeyWithoutFiles: an unused file source must not appear in the
// adapted JSON. A running Caddy from before this field existed rejects the
// unknown key, so `systemctl reload` fails while the binary is being replaced.
func TestNoFileKeyWithoutFiles(t *testing.T) {
	b, err := json.Marshal(URLIPRange{URLs: []string{"https://example.invalid/list.txt"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"file"`) {
		t.Fatalf("config carries an unused file key: %s", b)
	}
}
