package caddy_ip_list

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// TestFileOnlyIntervalCapped: a file-only source ignores a long interval,
// because an unchanged file costs one stat and CI updates must land fast.
func TestFileOnlyIntervalCapped(t *testing.T) {
	dir := t.TempDir()
	listPath := filepath.Join(dir, "mono.txt")
	if err := os.WriteFile(listPath, []byte("192.0.2.0/24\n"), 0o644); err != nil {
		t.Fatalf("write list: %v", err)
	}

	input := `
	list {
	    file ` + listPath + `
	    interval 6h
	}`

	d := caddyfile.NewTestDispenser(input)
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

	// A cold start refreshes immediately; with the interval capped to 15s the
	// list must appear well before a 6h ticker would ever fire.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(r.GetIPRanges(nil)) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if len(r.GetIPRanges(nil)) == 0 {
		t.Fatalf("list still empty; 6h interval was not capped for a file-only source")
	}
}
