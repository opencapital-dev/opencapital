package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// RepoEnumerator lists the plugin ids published under a repository-name prefix.
// It replaces the OCI /v2/_catalog sweep, which GHCR serves only as a useless
// global list. The id returned is the trailing path segment after the prefix.
type RepoEnumerator interface {
	ReposWithPrefix(ctx context.Context, prefix string) ([]string, error)
}

// GHCREnumerator enumerates an owner's container packages via the GitHub Packages
// REST API (GET /user/packages?package_type=container), paginating and filtering by
// the GHCR package name prefix (e.g. "plugins-staging/").
type GHCREnumerator struct {
	apiBase string
	token   string
	httpc   *http.Client
}

func NewGHCREnumerator(token string) *GHCREnumerator {
	return &GHCREnumerator{
		apiBase: "https://api.github.com",
		token:   token,
		httpc:   &http.Client{Timeout: 30 * time.Second},
	}
}

const maxEnumeratePages = 1000

func (g *GHCREnumerator) ReposWithPrefix(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	for page := 1; page <= maxEnumeratePages; page++ {
		url := fmt.Sprintf("%s/user/packages?package_type=container&per_page=100&page=%d", g.apiBase, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+g.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := g.httpc.Do(req)
		if err != nil {
			return nil, err
		}
		var pkgs []struct {
			Name string `json:"name"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&pkgs)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusOK {
			return nil, fmt.Errorf("list packages page %d: status %d", page, status)
		}
		if derr != nil {
			return nil, fmt.Errorf("decode packages page %d: %w", page, derr)
		}
		if len(pkgs) == 0 {
			sort.Strings(out)
			return out, nil
		}
		for _, p := range pkgs {
			if id, ok := strings.CutPrefix(p.Name, prefix); ok && id != "" {
				out = append(out, id)
			}
		}
	}
	return nil, fmt.Errorf("enumerate exceeded %d pages", maxEnumeratePages)
}
