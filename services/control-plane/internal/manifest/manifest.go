// Package manifest reads the marketplace's plugin/version set from a PUBLIC
// JSON manifest (plugins.json published to the opencapital repo, read
// over raw.githubusercontent.com), replacing the per-namespace GitHub Packages
// REST enumeration the catalog used to do.
//
// The manifest is the source of truth for which plugin versions exist. It has
// two sections:
//
//	{
//	  "plugins": {"core-app":["1.2.0","1.1.0"],"core-datasource":["0.4.1"],"yfinance-app":[]},
//	  "preview": {"core-app":["0.1.1"],"core-datasource":["0.1.6","0.1.5"],"yfinance-app":["0.1.1"]}
//	}
//
// `plugins` = VALIDATED/productive versions (highest-is-productive; an empty
// list means no validated version exists yet). `preview` = PREVIEW/staging
// versions surfaced in the marketplace preview channel. Versions are bare
// semver, highest-first.
package manifest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

// DefaultTTL is how long a successfully-fetched manifest is served before the
// next request triggers a refresh.
const DefaultTTL = 60 * time.Second

type doc struct {
	Plugins map[string][]string `json:"plugins"`
	Preview map[string][]string `json:"preview"`
}

// Client fetches + caches the public plugins manifest. It is concurrency-safe:
// the marketplace handlers read it from multiple requests at once. On a refresh
// failure it serves the last good value (logging at Warn); only a COLD miss
// (never fetched successfully) returns an error.
type Client struct {
	url   string
	httpc *http.Client
	ttl   time.Duration
	now   func() time.Time
	log   *slog.Logger

	mu        sync.Mutex
	cached    *doc // nil until the first successful fetch
	fetchedAt time.Time
}

// New builds a Client for the given manifest URL. ttl<=0 selects DefaultTTL.
// httpc nil selects a client with a sane timeout.
func New(url string, httpc *http.Client, ttl time.Duration, log *slog.Logger) *Client {
	if httpc == nil {
		httpc = &http.Client{Timeout: 15 * time.Second}
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{url: url, httpc: httpc, ttl: ttl, now: time.Now, log: log}
}

// load returns the cached manifest doc, refreshing it if stale. On a refresh
// failure with a warm cache it returns the stale value; on a cold miss it
// returns the error. One fetch serves both the validated (plugins) and preview
// sections.
func (c *Client) load(ctx context.Context) (*doc, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached != nil && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.cached, nil
	}

	fresh, err := c.fetch(ctx)
	if err != nil {
		if c.cached != nil {
			c.log.Warn("plugins manifest refresh failed; serving cached value",
				"err", err, "url", c.url)
			return c.cached, nil
		}
		return nil, fmt.Errorf("fetch plugins manifest %s: %w", c.url, err)
	}
	c.cached = fresh
	c.fetchedAt = c.now()
	return c.cached, nil
}

func (c *Client) fetch(ctx context.Context) (*doc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain a small amount so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var d doc
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if d.Plugins == nil {
		d.Plugins = map[string][]string{}
	}
	if d.Preview == nil {
		d.Preview = map[string][]string{}
	}
	return &d, nil
}

// PluginIDs returns the manifest's validated-section plugin keys, sorted.
// Includes plugins with an empty validated list — callers that need a validated
// version filter those out via ValidatedVersions.
func (c *Client) PluginIDs(ctx context.Context) ([]string, error) {
	d, err := c.load(ctx)
	if err != nil {
		return nil, err
	}
	return sortedKeys(d.Plugins), nil
}

// ValidatedVersions returns the validated versions for one plugin, semver-desc
// (highest first). Unknown id or empty list yields an empty slice.
func (c *Client) ValidatedVersions(ctx context.Context, id string) ([]string, error) {
	d, err := c.load(ctx)
	if err != nil {
		return nil, err
	}
	return sortSemverDesc(d.Plugins[id]), nil
}

// PreviewPluginIDs returns the manifest's preview-section plugin keys, sorted.
// A missing `preview` section yields an empty slice (not an error).
func (c *Client) PreviewPluginIDs(ctx context.Context) ([]string, error) {
	d, err := c.load(ctx)
	if err != nil {
		return nil, err
	}
	return sortedKeys(d.Preview), nil
}

// PreviewVersions returns the preview versions for one plugin, semver-desc
// (highest first). Unknown id or empty list yields an empty slice.
func (c *Client) PreviewVersions(ctx context.Context, id string) ([]string, error) {
	d, err := c.load(ctx)
	if err != nil {
		return nil, err
	}
	return sortSemverDesc(d.Preview[id]), nil
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// normSemver returns version's v-prefixed canonical form for comparison, or ""
// if it isn't semver. Tolerates bare versions (the manifest's form) by trying a
// leading "v".
func normSemver(v string) string {
	if semver.IsValid(v) {
		return v
	}
	if p := "v" + v; semver.IsValid(p) {
		return p
	}
	return ""
}

// sortSemverDesc returns the original version strings, greatest semver first,
// dropping any that aren't valid semver.
func sortSemverDesc(versions []string) []string {
	type tagged struct{ orig, norm string }
	valid := make([]tagged, 0, len(versions))
	for _, v := range versions {
		if n := normSemver(v); n != "" {
			valid = append(valid, tagged{orig: v, norm: n})
		}
	}
	sort.Slice(valid, func(i, j int) bool { return semver.Compare(valid[i].norm, valid[j].norm) > 0 })
	out := make([]string, len(valid))
	for i, v := range valid {
		out[i] = v.orig
	}
	return out
}
