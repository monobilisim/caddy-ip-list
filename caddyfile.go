package caddy_ip_list

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(URLIPRange{})
}

// URLIPRange provides a range of IP address prefixes (CIDRs) retrieved from url.
type URLIPRange struct {
	// List of URLs to fetch the IP ranges from.
	URLs []string `json:"url"`
	// Local files to read the IP ranges from, in the same format as a URL source.
	// A file source needs neither TLS nor DNS, so it can be refreshed often
	// without any network cost.
	Paths []string `json:"file"`
	// refresh Interval
	Interval caddy.Duration `json:"interval,omitempty"`
	// request Timeout
	Timeout caddy.Duration `json:"timeout,omitempty"`
	// Number of retries for fetching the IP list.
	// Default is 2 when unspecified. Set explicitly to 0 to disable retries.
	Retries *int `json:"retries,omitempty"`

	// Optional path to a cache file. If not set, a file under Caddy's data
	// directory will be used, derived from the URLs.
	CacheFile string `json:"cache_file,omitempty"`

	// AllowEmpty lets a refresh that yields no prefixes replace a non-empty
	// list. Leave it off: an empty list makes `not dynamic_client_ip` match
	// everything, so a broken or half-written source would lock every client out.
	AllowEmpty bool `json:"allow_empty,omitempty"`

	// Holds the parsed CIDR ranges from Ranges.
	ranges []netip.Prefix

	ctx  caddy.Context
	lock *sync.RWMutex
	log  *zap.Logger

	// Closed by Cleanup to stop the background refresh loop.
	stop chan struct{}
	// Guards a second Cleanup, which would close an already closed channel.
	cleanupOnce sync.Once
	// Fingerprint of the file sources as of the last refresh. A low interval is
	// cheap only if an unchanged file costs a stat and nothing else.
	lastFiles string
}

// CaddyModule returns the Caddy module information.
func (URLIPRange) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.ip_sources.list",
		New: func() caddy.Module { return new(URLIPRange) },
	}
}

// getContext returns a cancelable context, with a timeout if configured.
func (s *URLIPRange) getContext() (context.Context, context.CancelFunc) {
	if s.Timeout > 0 {
		return context.WithTimeout(s.ctx, time.Duration(s.Timeout))
	}
	return context.WithCancel(s.ctx)
}

// parsePrefixes reads a list of CIDRs, one per line. Everything after '#' is a
// comment, matching the format served by the whitelist files.
func parsePrefixes(r io.Reader) ([]netip.Prefix, error) {
	scanner := bufio.NewScanner(r)
	var prefixes []netip.Prefix
	for scanner.Scan() {
		line := scanner.Text()

		// Remove comments from the line
		if idx := strings.Index(line, "#"); idx != -1 {
			line = line[:idx]
		}

		// Trim spaces
		line = strings.TrimSpace(line)

		// Skip empty lines
		if line == "" {
			continue
		}

		// Convert to prefix
		prefix, err := caddyhttp.CIDRExpressionToPrefix(line)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return prefixes, nil
}

func (s *URLIPRange) fetch(api string) ([]netip.Prefix, error) {
	retries := 2
	if s.Retries != nil {
		retries = *s.Retries
		if retries < 0 {
			retries = 0
		}
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		ctx, cancel := s.getContext()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
		if err != nil {
			lastErr = err
			cancel()
			break
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			cancel()
		} else {
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				// drain and close body before next attempt
				_ = resp.Body.Close()
				lastErr = fmt.Errorf("fetch %s returned HTTP %d", api, resp.StatusCode)
				cancel()
			} else {
				prefixes, parseErr := parsePrefixes(resp.Body)
				_ = resp.Body.Close()
				cancel()
				if parseErr != nil {
					return nil, parseErr
				}
				return prefixes, nil
			}
		}

		// If not last attempt, delay before retrying
		if attempt < retries {
			time.Sleep(1 * time.Second)
		}
	}
	// After all attempts
	return nil, fmt.Errorf("after %d retries: %w", retries, lastErr)
}

// readFile reads a local list. There is no retry: a missing or unreadable file
// is a deployment mistake, and the previous list stays in use either way.
func (s *URLIPRange) readFile(path string) ([]netip.Prefix, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	prefixes, err := parsePrefixes(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return prefixes, nil
}

type cacheFileContents struct {
	Prefixes  []string  `json:"prefixes"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *URLIPRange) cachePath() (string, error) {
	if s.CacheFile != "" {
		return s.CacheFile, nil
	}
	// Derive from every source, so two blocks with different lists never share
	// a cache file. The "file:" tag keeps paths from colliding with URLs.
	sources := make([]string, 0, len(s.URLs)+len(s.Paths))
	sources = append(sources, s.URLs...)
	for _, p := range s.Paths {
		sources = append(sources, "file:"+p)
	}
	joined := strings.Join(sources, "|")
	sum := sha256.Sum256([]byte(joined))
	name := "ip-list-cache-" + hex.EncodeToString(sum[:]) + ".json"
	dir := caddy.AppDataDir()
	if dir == "" {
		// fallback to current working directory
		dir = "."
	}
	return filepath.Join(dir, name), nil
}

func (s *URLIPRange) loadFromCache() ([]netip.Prefix, error) {
	path, err := s.cachePath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var contents cacheFileContents
	if err := json.NewDecoder(f).Decode(&contents); err != nil {
		return nil, err
	}
	prefixes := make([]netip.Prefix, 0, len(contents.Prefixes))
	for _, p := range contents.Prefixes {
		prefix, err := caddyhttp.CIDRExpressionToPrefix(p)
		if err != nil {
			return nil, fmt.Errorf("invalid prefix in cache %q: %w", p, err)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func (s *URLIPRange) saveToCache(prefixes []netip.Prefix) error {
	path, err := s.cachePath()
	if err != nil {
		return err
	}
	// ensure directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// prepare contents
	contents := cacheFileContents{UpdatedAt: time.Now()}
	contents.Prefixes = make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		contents.Prefixes = append(contents.Prefixes, p.String())
	}
	// write atomically
	// A unique temp name: many instances can share one cache file (same
	// sources), and a shared temp file would interleave their writes into
	// invalid JSON, which a later start would decode as "no cache".
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&contents); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (s *URLIPRange) getPrefixes() ([]netip.Prefix, error) {
	var fullPrefixes []netip.Prefix
	for _, url := range s.URLs {
		// Fetch list
		prefixes, err := s.fetch(url)
		if err != nil {
			return nil, err
		}
		fullPrefixes = append(fullPrefixes, prefixes...)
	}
	for _, path := range s.Paths {
		prefixes, err := s.readFile(path)
		if err != nil {
			return nil, err
		}
		fullPrefixes = append(fullPrefixes, prefixes...)
	}

	return fullPrefixes, nil
}

func (s *URLIPRange) Provision(ctx caddy.Context) error {
	s.ctx = ctx
	s.lock = new(sync.RWMutex)
	s.log = ctx.Logger()
	stop := make(chan struct{})
	s.stop = stop

	// No network I/O here: Caddy re-provisions every module on each config
	// change, so a synchronous fetch directly extends the config load time.
	// The last known good list is served from the persistent cache.
	cached, cacheErr := s.loadFromCache()
	if cacheErr == nil {
		s.ranges = cached
	} else if s.log != nil {
		s.log.Warn("no usable IP range cache; starting with an empty list until the first refresh",
			zap.Error(cacheErr))
	}

	// Refresh in the background: immediately on a cold start (no cache),
	// otherwise at the first tick.
	go s.refreshLoop(stop, cacheErr != nil)
	return nil
}

// Cleanup stops the background refresh loop when this config is unloaded.
// Without it every config load leaks a goroutine and a ticker, because Caddy
// never cancels the module context on unload.
func (s *URLIPRange) Cleanup() error {
	s.cleanupOnce.Do(func() {
		if s.stop != nil {
			close(s.stop)
		}
	})
	return nil
}

// fileSourcesChanged reports whether any file source is newer than the last
// refresh. It costs one stat per file and no parsing, so an interval of a few
// seconds stays cheap even with many instances sharing the same file.
func (s *URLIPRange) fileSourcesChanged() bool {
	if len(s.Paths) == 0 {
		return true // URL sources: always refresh, the fetch is the point
	}
	fp := make([]string, 0, len(s.Paths))
	for _, p := range s.Paths {
		fi, err := os.Stat(p)
		if err != nil {
			// Let the refresh report the error the usual way.
			return true
		}
		fp = append(fp, fmt.Sprintf("%s:%d:%d", p, fi.Size(), fi.ModTime().UnixNano()))
	}
	joined := strings.Join(fp, "|")
	if joined == s.lastFiles {
		return false
	}
	s.lastFiles = joined
	return true
}

// refreshIfChanged skips the work when every source is an unchanged file.
func (s *URLIPRange) refreshIfChanged() {
	if !s.fileSourcesChanged() {
		return
	}
	s.refresh()
}

func (s *URLIPRange) refresh() {
	fullPrefixes, err := s.getPrefixes()
	if err != nil {
		if s.log != nil {
			s.log.Warn("failed to refresh IP ranges; keeping existing cache", zap.Error(err))
		}
		return
	}

	// An empty result is almost always a truncated or emptied source, not an
	// intentional "allow nobody". Keeping the previous list fails open for the
	// listed clients instead of locking everyone out.
	if len(fullPrefixes) == 0 && !s.AllowEmpty {
		s.lock.RLock()
		had := len(s.ranges) > 0
		s.lock.RUnlock()
		if had {
			if s.log != nil {
				s.log.Warn("refresh returned no prefixes; keeping the previous list",
					zap.Int("paths", len(s.Paths)), zap.Int("urls", len(s.URLs)))
			}
			return
		}
	}

	s.lock.Lock()
	s.ranges = fullPrefixes
	s.lock.Unlock()

	if err := s.saveToCache(fullPrefixes); err != nil && s.log != nil {
		s.log.Warn("failed to save IP ranges cache after refresh", zap.Error(err))
	}
}

func (s *URLIPRange) refreshLoop(stop <-chan struct{}, refreshNow bool) {
	interval := time.Hour
	if s.Interval > 0 {
		interval = time.Duration(s.Interval)
	}

	if refreshNow {
		s.refresh()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.refreshIfChanged()
		case <-stop:
			return
		}
	}
}

func (s *URLIPRange) GetIPRanges(_ *http.Request) []netip.Prefix {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.ranges
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
//
//	list {
//	   interval val
//	   timeout val
//	   url string
//	}
func (m *URLIPRange) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // Skip module name.

	// No same-line options are supported
	if d.NextArg() {
		return d.ArgErr()
	}

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "interval":
			if !d.NextArg() {
				return d.ArgErr()
			}
			val, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return err
			}
			m.Interval = caddy.Duration(val)
		case "timeout":
			if !d.NextArg() {
				return d.ArgErr()
			}
			val, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return err
			}
			m.Timeout = caddy.Duration(val)
		case "retries":
			if !d.NextArg() {
				return d.ArgErr()
			}
			var n int
			_, err := fmt.Sscanf(d.Val(), "%d", &n)
			if err != nil || n < 0 {
				return fmt.Errorf("invalid retries value: %s", d.Val())
			}
			m.Retries = &n
		case "cache_file":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.CacheFile = d.Val()
		case "file":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.Paths = append(m.Paths, d.Val())
		case "allow_empty":
			m.AllowEmpty = true
		case "url":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.URLs = append(m.URLs, d.Val())
		default:
			return d.ArgErr()
		}
	}

	return nil
}

// Interface guards
var (
	_ caddy.Module            = (*URLIPRange)(nil)
	_ caddy.Provisioner       = (*URLIPRange)(nil)
	_ caddy.CleanerUpper      = (*URLIPRange)(nil)
	_ caddyfile.Unmarshaler   = (*URLIPRange)(nil)
	_ caddyhttp.IPRangeSource = (*URLIPRange)(nil)
)
