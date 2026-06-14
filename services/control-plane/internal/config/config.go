package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type StaticUser struct {
	UserID string `json:"user_id"`
	Token  string `json:"token"`
}

type Config struct {
	ListenAddr     string
	ControlDBDSN   string
	JWTLifetime    time.Duration
	JWTIssuer      string
	JWTAudience    string
	IDPMode        string
	IDPStaticUsers []StaticUser

	// Grafana identity forwarding. When GRAFANA_JWKS_URL is set, /jwt/mint
	// accepts Grafana-signed X-Grafana-Id JWTs and verifies them against this
	// JWKS. GrafanaJWTIssuer / GrafanaJWTAudience optionally enforce iss/aud
	// claims; empty means skip those checks.
	GrafanaJWKSURL     string
	GrafanaJWTIssuer   string
	GrafanaJWTAudience string

	// v8 Kinde access-token authentication. When KindeJWKSURL is set, the
	// new /v1/* shell-facing endpoints (/v1/me/orgs, /v1/marketplace/catalog)
	// accept Kinde-issued access tokens and verify them against this JWKS.
	// KindeIssuer / KindeAudience optionally enforce iss/aud claims.
	KindeJWKSURL  string
	KindeIssuer   string
	KindeAudience string

	// v8 plugin registry (GHCR). Control-plane reads plugin artifacts +
	// self-describing footprints from this registry at runtime (no hardcoded
	// manifests). RegistryInternalURL is how control-plane reaches the registry.
	// RegistryPublicURL is the host-reachable base stamped into reconciler blob
	// URLs. RegistryNamespace is the repository prefix plugins are published under.
	RegistryInternalURL string
	RegistryPublicURL   string
	RegistryNamespace   string

	// RegistryStagingNamespace is the repository prefix freshly-published
	// (unverified) plugin artifacts land under before the promotion gate
	// signature-verifies them and copies them into RegistryNamespace.
	RegistryStagingNamespace string

	// RegistryOwner is the GHCR owner/org; required in GHCR mode — used by the
	// Packages REST enumerator/deleter.
	RegistryOwner string

	// PluginsManifestURL is the PUBLIC plugins manifest (GitHub Pages) the
	// marketplace catalog reads its validated-version set from, replacing the
	// GitHub Packages REST enumeration of the trusted namespace. When empty,
	// main.go falls back to the legacy trusted-namespace enumeration so a
	// misconfigured deploy serves a (stale-shaped) catalog rather than a blank
	// one. Loaded from PLUGINS_MANIFEST_URL.
	PluginsManifestURL string

	// v6 Phase 3 install endpoint targets. The control plane is the only
	// principal that CREATEs RisingWave schemas / roles / views; RisingWaveDSN
	// is therefore a privileged DSN (typically the root user).
	RisingWaveDSN string
	PluginsRoot   string

	// OwnJWKSURL is the address RisingWave (and other consumers) will use to
	// fetch this control plane's JWKS. Substituted into RW `CREATE USER ...
	// WITH oauth(jwks_url=...)` at install time. Defaults to the dev compose
	// hostname; prod must override.
	OwnJWKSURL string

	// AdminBootstrapToken is the one-shot bearer that gates the
	// admin endpoints (POST/DELETE /admin/users/link and the plugin
	// install route) on a fresh stack, before any user has been
	// granted role='admin' in user_org. When empty, the bootstrap
	// auth path is hard-disabled — every request through it is a 401
	// without ever comparing tokens. Loaded from ADMIN_BOOTSTRAP_TOKEN
	// or ADMIN_BOOTSTRAP_TOKEN_FILE (file path); the _FILE variant is
	// preferred so the token doesn't appear in the process
	// environment.
	AdminBootstrapToken string

	// v6 Phase 6: gateway LRU write-through. After POST /portfolios is
	// durable, control-plane POSTs (portfolio_id, org_id) to each URL
	// here so the gateway's in-process cache is primed without waiting
	// for the replica miss path. LRUPrimeToken is the shared secret
	// stamped into the X-Lru-Prime-Token header.
	GatewayLRUPrimeURLs []string
	LRUPrimeToken       string

	// v6 Phase 7: Grafana admin API + browser-session validation. The
	// landing/onboarding flow calls Grafana to create per-tenant orgs
	// and to resolve session cookies to user records. Empty
	// GrafanaBaseURL disables the /api/me + /api/onboarding/orgs
	// routes (they return 503).
	GrafanaBaseURL          string
	GrafanaAdminUser        string
	GrafanaAdminPassword    string
	GrafanaSessionCookie    string // default "grafana_session"
	GrafanaPluginRedirectTo string // post-wizard URL the browser is redirected to; default "/grafana/"

	// Inter-container URLs handed to plugins via per-(grafana_org)
	// AppPluginConfig.jsonData. The control-plane only knows them via
	// env vars set on the compose service.
	PluginControlPlaneURL string
	PluginGatewayURL      string
	PluginOTLPEndpoint    string

	// v6 Phase 8 (ADR-0050) self-service plugin uninstall. control-plane
	// mints a short-lived capability JWT (aud=TombstoneJWTAudience)
	// per page and POSTs gateway /internal/tombstone. Gateway verifies
	// via JWKS. Empty GatewayBaseURL disables the uninstall worker.
	GatewayBaseURL       string
	TombstoneJWTAudience string
	TombstoneJWTLifetime time.Duration

	LogLevel string
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:  envOrDefault("LISTEN_ADDR", ":8080"),
		JWTIssuer:   envOrDefault("JWT_ISSUER", "control-plane"),
		JWTAudience: envOrDefault("JWT_AUDIENCE", "gateway"),
		IDPMode:     envOrDefault("IDP_MODE", "static"),
		LogLevel:    envOrDefault("LOG_LEVEL", "info"),
	}

	cfg.ControlDBDSN = os.Getenv("CONTROL_DB_DSN")
	if cfg.ControlDBDSN == "" {
		return cfg, errors.New("CONTROL_DB_DSN is required")
	}

	lifetime := envOrDefault("JWT_LIFETIME", "10m")
	d, err := time.ParseDuration(lifetime)
	if err != nil {
		return cfg, fmt.Errorf("JWT_LIFETIME %q: %w", lifetime, err)
	}
	cfg.JWTLifetime = d

	cfg.GrafanaJWKSURL = os.Getenv("GRAFANA_JWKS_URL")
	cfg.GrafanaJWTIssuer = os.Getenv("GRAFANA_JWT_ISSUER")
	cfg.GrafanaJWTAudience = os.Getenv("GRAFANA_JWT_AUDIENCE")

	cfg.KindeJWKSURL = os.Getenv("KINDE_JWKS_URL")
	cfg.KindeIssuer = os.Getenv("KINDE_ISSUER")
	cfg.KindeAudience = os.Getenv("KINDE_AUDIENCE")

	cfg.RegistryInternalURL = strings.TrimRight(os.Getenv("REGISTRY_INTERNAL_URL"), "/")
	cfg.RegistryPublicURL = strings.TrimRight(os.Getenv("REGISTRY_PUBLIC_URL"), "/")
	cfg.RegistryNamespace = envOrDefault("REGISTRY_NAMESPACE", "plugins")
	cfg.RegistryStagingNamespace = envOrDefault("REGISTRY_STAGING_NAMESPACE", "plugins-staging")
	cfg.RegistryOwner = os.Getenv("REGISTRY_OWNER")
	cfg.PluginsManifestURL = strings.TrimRight(os.Getenv("PLUGINS_MANIFEST_URL"), "/")

	cfg.RisingWaveDSN = os.Getenv("RISINGWAVE_DSN")
	cfg.PluginsRoot = envOrDefault("PLUGINS_ROOT", "/var/lib/plugins")
	// The JWKS URL RW will reach the control plane on. Default suits the
	// dev compose stack; prod must set this explicitly.
	cfg.OwnJWKSURL = envOrDefault("CONTROL_PLANE_JWKS_URL", "http://control-plane:8080/jwt/jwks")

	tok, err := envOrFile("ADMIN_BOOTSTRAP_TOKEN")
	if err != nil {
		return cfg, fmt.Errorf("ADMIN_BOOTSTRAP_TOKEN: %w", err)
	}
	cfg.AdminBootstrapToken = tok

	// v6 Phase 6: gateway LRU write-through. Comma-separated list of
	// fully-qualified gateway URLs the control plane fires
	// /internal/lru-prime against after a successful POST /portfolios.
	// Empty list = no notifications fired (the LRU still self-heals
	// via the replica miss path; this is the design's fail-open mode).
	if v := os.Getenv("GATEWAY_LRU_PRIME_URLS"); v != "" {
		for _, u := range strings.Split(v, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				cfg.GatewayLRUPrimeURLs = append(cfg.GatewayLRUPrimeURLs, u)
			}
		}
	}
	primeTok, err := envOrFile("LRU_PRIME_TOKEN")
	if err != nil {
		return cfg, fmt.Errorf("LRU_PRIME_TOKEN: %w", err)
	}
	cfg.LRUPrimeToken = primeTok
	if len(cfg.GatewayLRUPrimeURLs) > 0 && cfg.LRUPrimeToken == "" {
		return cfg, errors.New("GATEWAY_LRU_PRIME_URLS set but LRU_PRIME_TOKEN (or _FILE) is empty")
	}

	cfg.GrafanaBaseURL = os.Getenv("GRAFANA_BASE_URL")
	cfg.GrafanaAdminUser = envOrDefault("GRAFANA_ADMIN_USER", "admin")
	gPass, err := envOrFile("GRAFANA_ADMIN_PASSWORD")
	if err != nil {
		return cfg, fmt.Errorf("GRAFANA_ADMIN_PASSWORD: %w", err)
	}
	cfg.GrafanaAdminPassword = gPass
	cfg.GrafanaSessionCookie = envOrDefault("GRAFANA_SESSION_COOKIE", "grafana_session")
	cfg.GrafanaPluginRedirectTo = envOrDefault("GRAFANA_REDIRECT_TO", "/grafana/")
	cfg.PluginControlPlaneURL = envOrDefault("PLUGIN_CONTROL_PLANE_URL", "http://control-plane:8080")
	cfg.PluginGatewayURL = envOrDefault("PLUGIN_GATEWAY_URL", "http://gateway:8090")
	cfg.PluginOTLPEndpoint = envOrDefault("PLUGIN_OTLP_ENDPOINT", "http://alloy:4317")

	cfg.GatewayBaseURL = os.Getenv("GATEWAY_BASE_URL")
	cfg.TombstoneJWTAudience = envOrDefault("TOMBSTONE_JWT_AUDIENCE", "gateway-tombstone")
	tombTTL := envOrDefault("TOMBSTONE_JWT_LIFETIME", "30s")
	dT, err := time.ParseDuration(tombTTL)
	if err != nil {
		return cfg, fmt.Errorf("TOMBSTONE_JWT_LIFETIME %q: %w", tombTTL, err)
	}
	cfg.TombstoneJWTLifetime = dT

	if cfg.IDPMode != "static" {
		return cfg, fmt.Errorf("IDP_MODE %q: only \"static\" supported in Phase 1", cfg.IDPMode)
	}

	raw := os.Getenv("IDP_STATIC_USERS")
	if raw == "" {
		return cfg, errors.New("IDP_STATIC_USERS is required when IDP_MODE=static")
	}
	if err := json.Unmarshal([]byte(raw), &cfg.IDPStaticUsers); err != nil {
		return cfg, fmt.Errorf("IDP_STATIC_USERS: %w", err)
	}
	if len(cfg.IDPStaticUsers) == 0 {
		return cfg, errors.New("IDP_STATIC_USERS is empty")
	}

	return cfg, nil
}

func envOrDefault(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envOrFile reads a secret from either KEY or KEY_FILE. Trailing
// whitespace (including the newline kubectl/docker injects when
// shelling secret content into a file) is trimmed. Returns "" with
// nil err when neither is set; callers decide whether absence is
// fatal.
func envOrFile(key string) (string, error) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v, nil
	}
	pathKey := key + "_FILE"
	if path, ok := os.LookupEnv(pathKey); ok && path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s=%s: %w", pathKey, path, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}
