// Package grafana wraps the Grafana HTTP admin API. Phase 7 landing-flow
// uses it to create per-tenant Grafana orgs at signup time and to
// validate browser sessions against the in-cluster Grafana service.
//
// Auth: a Grafana admin service-account token, mounted in via the SOPS
// secret `grafana_admin_token`. The client does NOT issue or rotate
// that token; operator provisions it in Grafana's UI.
package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client speaks to one Grafana installation. Concurrency-safe; reuse it.
//
// Auth model (v6 Phase 9): HTTP Basic against Grafana's built-in
// server-admin user (configured via GF_SECURITY_ADMIN_USER +
// GF_SECURITY_ADMIN_PASSWORD on the grafana service). Service-account
// tokens were considered first but Grafana service accounts are
// strictly org-scoped — adding them to a new org via /api/orgs/N/users
// fails with "User not found" because SAs aren't in the user table.
// Basic auth against the server-admin user works across every Grafana
// org via X-Grafana-Org-Id.
type Client struct {
	baseURL       string // e.g. "http://grafana:3000"
	adminUser     string
	adminPassword string
	http          *http.Client
}

// New returns a Client. baseURL should NOT include a trailing slash.
// adminUser + adminPassword must match GF_SECURITY_ADMIN_USER and
// GF_SECURITY_ADMIN_PASSWORD on the grafana service. Empty
// adminPassword disables admin calls (CreateOrg / AddOrgUser /
// SetAppPluginConfig return ErrNoAdminToken) but still allows
// session-cookie validation against /api/user.
func New(baseURL, adminUser, adminPassword string) *Client {
	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		adminUser:     adminUser,
		adminPassword: adminPassword,
		http:          &http.Client{Timeout: 10 * time.Second},
	}
}

// ErrNoAdminToken is returned by admin-scoped methods when the client
// was built without admin credentials. The session-validate path does
// NOT return this error.
var ErrNoAdminToken = errors.New("grafana: admin credentials not configured")

// ErrUnauthorized is returned by GetUserBySession when the supplied
// session cookie is invalid or expired. Distinct from a transport
// error so callers can map it to HTTP 401 cleanly.
var ErrUnauthorized = errors.New("grafana: session cookie rejected")

// User is the subset of Grafana's /api/user payload that the
// onboarding flow cares about. Fields beyond these are ignored.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// GetUserBySession resolves a Grafana session cookie to the user it
// belongs to by calling Grafana's /api/user with the cookie forwarded.
// Returns ErrUnauthorized for 401/403; transport errors otherwise.
func (c *Client) GetUserBySession(ctx context.Context, cookie *http.Cookie) (User, error) {
	if cookie == nil || cookie.Value == "" {
		return User{}, ErrUnauthorized
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/user", nil)
	if err != nil {
		return User{}, err
	}
	req.AddCookie(cookie)
	resp, err := c.http.Do(req)
	if err != nil {
		return User{}, fmt.Errorf("grafana /api/user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return User{}, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return User{}, fmt.Errorf("grafana /api/user: %d %s", resp.StatusCode, body)
	}
	var u User
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return User{}, fmt.Errorf("decode /api/user: %w", err)
	}
	if u.ID == 0 {
		return User{}, fmt.Errorf("grafana /api/user: empty id")
	}
	return u, nil
}

// CreateOrg creates a new Grafana org and returns its integer id.
// Refuses to handle the "already exists" case — the onboarding flow
// generates unique org names per signup, so a collision is an error
// the caller should surface.
func (c *Client) CreateOrg(ctx context.Context, name string) (int64, error) {
	if c.adminPassword == "" {
		return 0, ErrNoAdminToken
	}
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, err := c.adminPost(ctx, "/api/orgs", body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("grafana POST /api/orgs: %d %s", resp.StatusCode, respBody)
	}
	var out struct {
		OrgID int64 `json:"orgId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode /api/orgs: %w", err)
	}
	if out.OrgID == 0 {
		return 0, fmt.Errorf("grafana POST /api/orgs: empty orgId")
	}
	return out.OrgID, nil
}

// AddOrgUser adds an existing Grafana user to a Grafana org with the
// given role ("Admin", "Editor", or "Viewer"). Idempotent: a
// "user is already member" 409 is treated as success.
func (c *Client) AddOrgUser(ctx context.Context, orgID int64, loginOrEmail, role string) error {
	if c.adminPassword == "" {
		return ErrNoAdminToken
	}
	body, _ := json.Marshal(map[string]string{
		"loginOrEmail": loginOrEmail,
		"role":         role,
	})
	resp, err := c.adminPost(ctx, fmt.Sprintf("/api/orgs/%d/users", orgID), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusConflict {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("grafana POST /api/orgs/%d/users: %d %s", orgID, resp.StatusCode, respBody)
}

// RemoveOrgUser drops a user from a Grafana org. Idempotent: 404
// ("user is not a member") + 400 (last admin) are tolerated as
// no-op. Used by the wizard to evict the caller from Main Org
// once they're added to their own tenant org.
func (c *Client) RemoveOrgUser(ctx context.Context, orgID, userID int64) error {
	if c.adminPassword == "" {
		return ErrNoAdminToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/api/orgs/%d/users/%d", c.baseURL, orgID, userID), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.adminUser, c.adminPassword)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("grafana DELETE org user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("grafana DELETE /api/orgs/%d/users/%d: %d %s", orgID, userID, resp.StatusCode, body)
}

// SetAppPluginConfig writes per-(grafana_org) AppPlugin SecureJsonData +
// JsonData. Used at onboarding time to inject each org's
// platform_token into the plugin Grafana installed at boot.
//
// Grafana's API for this is org-scoped via X-Grafana-Org-Id. Basic
// Auth as the server-admin user (admin) traverses every Grafana org
// without per-org membership; v6 Phase 9 picked this over the
// service-account-token path because SAs are strictly org-scoped and
// can't be added to other orgs cleanly.
func (c *Client) SetAppPluginConfig(ctx context.Context, orgID int64, pluginID string, enabled bool, jsonData, secureJsonData map[string]any) error {
	if c.adminPassword == "" {
		return ErrNoAdminToken
	}
	body, _ := json.Marshal(map[string]any{
		"enabled":        enabled,
		"pinned":         true,
		"jsonData":       jsonData,
		"secureJsonData": secureJsonData,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/plugins/"+pluginID+"/settings", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.adminUser, c.adminPassword)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Grafana-Org-Id", fmt.Sprintf("%d", orgID))
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("grafana plugins settings: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("grafana POST /api/plugins/%s/settings: %d %s", pluginID, resp.StatusCode, respBody)
	}
	return nil
}

// adminPost is a shared helper for Basic-Auth admin POSTs (no
// org-scoped X-Grafana-Org-Id; used for server-level endpoints like
// /api/orgs which create resources across the cluster).
func (c *Client) adminPost(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.adminUser, c.adminPassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grafana POST %s: %w", path, err)
	}
	return resp, nil
}
