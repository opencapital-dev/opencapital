package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// GHCRDeleter deletes container package versions via the GitHub Packages REST
// API. It replaces the zot OCI manifest-DELETE path (GHCR does not support that).
type GHCRDeleter struct {
	apiBase string
	token   string
	httpc   *http.Client
}

func NewGHCRDeleter(token string) *GHCRDeleter {
	return &GHCRDeleter{apiBase: "https://api.github.com", token: token, httpc: &http.Client{Timeout: 30 * time.Second}}
}

// ResolveVersionID returns the package version id whose container tags include tag.
// ok=false (nil error) when no version carries that tag — the normal "already pruned"
// state. packageName is the GHCR package name, e.g. "plugins-staging/core-datasource".
func (d *GHCRDeleter) ResolveVersionID(ctx context.Context, packageName, tag string) (string, bool, error) {
	esc := url.PathEscape(packageName)
	for page := 1; page <= maxEnumeratePages; page++ {
		u := fmt.Sprintf("%s/user/packages/container/%s/versions?per_page=100&page=%d", d.apiBase, esc, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", false, err
		}
		req.Header.Set("Authorization", "Bearer "+d.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := d.httpc.Do(req)
		if err != nil {
			return "", false, err
		}
		var versions []struct {
			ID       json.Number `json:"id"`
			Metadata struct {
				Container struct {
					Tags []string `json:"tags"`
				} `json:"container"`
			} `json:"metadata"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&versions)
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusNotFound {
			return "", false, nil
		}
		if status != http.StatusOK {
			return "", false, fmt.Errorf("list versions %s page %d: status %d", packageName, page, status)
		}
		if derr != nil {
			return "", false, fmt.Errorf("decode versions %s page %d: %w", packageName, page, derr)
		}
		if len(versions) == 0 {
			return "", false, nil
		}
		for _, v := range versions {
			for _, t := range v.Metadata.Container.Tags {
				if t == tag {
					return v.ID.String(), true, nil
				}
			}
		}
	}
	return "", false, fmt.Errorf("resolve version %s:%s exceeded %d pages", packageName, tag, maxEnumeratePages)
}

// DeletePackageVersion deletes one container package version. 204 and 404 are both
// treated as success (404 = already gone).
func (d *GHCRDeleter) DeletePackageVersion(ctx context.Context, packageName, versionID string) error {
	esc := url.PathEscape(packageName)
	u := fmt.Sprintf("%s/user/packages/container/%s/versions/%s", d.apiBase, esc, versionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := d.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete version %s/%s: status %d", packageName, versionID, resp.StatusCode)
	}
	return nil
}
