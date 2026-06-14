// Package manifest parses the two federated-catalog file formats:
//   - PluginClient: fetches + caches a per-plugin self-describing manifest
//     (registry coords + own version list), one instance per manifest URL.
//   - ListClient: fetches + caches the curated marketplace list (an array of
//     per-plugin manifest URLs).
package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DefaultTTL is how long a successfully-fetched manifest is served before the
// next request triggers a refresh.
const DefaultTTL = 60 * time.Second

// PluginManifest is the per-plugin self-describing manifest: registry coords
// plus the plugin's own validated/preview version lists.
type PluginManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	PluginID      string       `json:"pluginId"`
	Publisher     string       `json:"publisher"`
	Registry      RegistrySpec `json:"registry"`
	Versions      []string     `json:"versions"`
	Preview       []string     `json:"preview,omitempty"`
}

// RegistrySpec holds the OCI registry coordinates for one plugin.
type RegistrySpec struct {
	Host             string `json:"host"`
	Namespace        string `json:"namespace"`
	StagingNamespace string `json:"stagingNamespace,omitempty"`
	PublicURL        string `json:"publicURL,omitempty"`
}

// PluginClient fetches + caches one per-plugin manifest URL.
type PluginClient struct {
	url   string
	httpc *http.Client
	ttl   time.Duration
	now   func() time.Time
	log   *slog.Logger

	mu        sync.Mutex
	cached    *PluginManifest
	fetchedAt time.Time
}

// NewPluginClient builds a PluginClient for the given manifest URL. ttl<=0
// selects DefaultTTL. httpc nil selects a client with a sane timeout. log nil
// selects slog.Default().
func NewPluginClient(url string, httpc *http.Client, ttl time.Duration, log *slog.Logger) *PluginClient {
	if httpc == nil {
		httpc = &http.Client{Timeout: 15 * time.Second}
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if log == nil {
		log = slog.Default()
	}
	return &PluginClient{url: url, httpc: httpc, ttl: ttl, now: time.Now, log: log}
}

// Fetch returns the cached manifest, refreshing it if stale. On a refresh
// failure with a warm cache it returns the stale value; on a cold miss it
// returns the error.
func (c *PluginClient) Fetch(ctx context.Context) (*PluginManifest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.cached, nil
	}
	body, err := getJSON(ctx, c.httpc, c.url)
	if err != nil {
		if c.cached != nil {
			c.log.Warn("plugin manifest refresh failed; serving cached", "err", err, "url", c.url)
			return c.cached, nil
		}
		return nil, fmt.Errorf("fetch plugin manifest %s: %w", c.url, err)
	}
	var m PluginManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode plugin manifest %s: %w", c.url, err)
	}
	if err := validatePlugin(&m); err != nil {
		return nil, fmt.Errorf("invalid plugin manifest %s: %w", c.url, err)
	}
	c.cached, c.fetchedAt = &m, c.now()
	return c.cached, nil
}

func validatePlugin(m *PluginManifest) error {
	if m.PluginID == "" {
		return errors.New("pluginId required")
	}
	if m.Registry.Host == "" {
		return errors.New("registry.host required")
	}
	if m.Registry.Namespace == "" {
		return errors.New("registry.namespace required")
	}
	if len(m.Preview) > 0 && m.Registry.StagingNamespace == "" {
		return errors.New("preview set but registry.stagingNamespace missing")
	}
	return nil
}

// listDoc is the JSON shape for the curated marketplace list.
type listDoc struct {
	SchemaVersion int      `json:"schemaVersion"`
	Plugins       []string `json:"plugins"`
}

// ListClient fetches + caches the curated marketplace list (array of per-plugin
// manifest URLs).
type ListClient struct {
	url   string
	httpc *http.Client
	ttl   time.Duration
	now   func() time.Time
	log   *slog.Logger

	mu        sync.Mutex
	cached    []string
	fetchedAt time.Time
}

// NewListClient builds a ListClient for the given marketplace list URL. ttl<=0
// selects DefaultTTL. httpc nil selects a client with a sane timeout. log nil
// selects slog.Default().
func NewListClient(url string, httpc *http.Client, ttl time.Duration, log *slog.Logger) *ListClient {
	if httpc == nil {
		httpc = &http.Client{Timeout: 15 * time.Second}
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if log == nil {
		log = slog.Default()
	}
	return &ListClient{url: url, httpc: httpc, ttl: ttl, now: time.Now, log: log}
}

// Fetch returns the cached URL list, refreshing it if stale. On a refresh
// failure with a warm cache it returns the stale value; on a cold miss it
// returns the error. Each entry is validated to be an http(s) URL.
func (c *ListClient) Fetch(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.cached, nil
	}
	body, err := getJSON(ctx, c.httpc, c.url)
	if err != nil {
		if c.cached != nil {
			c.log.Warn("marketplace list refresh failed; serving cached", "err", err, "url", c.url)
			return c.cached, nil
		}
		return nil, fmt.Errorf("fetch marketplace list %s: %w", c.url, err)
	}
	var d listDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("decode marketplace list: %w", err)
	}
	for _, u := range d.Plugins {
		if pu, perr := url.Parse(u); perr != nil || (pu.Scheme != "http" && pu.Scheme != "https") {
			return nil, fmt.Errorf("marketplace list entry %q is not an http(s) URL", u)
		}
	}
	c.cached, c.fetchedAt = d.Plugins, c.now()
	return c.cached, nil
}

// getJSON GETs url and returns the body, erroring on non-200.
func getJSON(ctx context.Context, httpc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
