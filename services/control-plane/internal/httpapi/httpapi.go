package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/portfolio-management/control-plane/internal/config"
	grafanaclient "github.com/portfolio-management/control-plane/internal/grafana"
	"github.com/portfolio-management/control-plane/internal/install"
	"github.com/portfolio-management/control-plane/internal/jwks"
	"github.com/portfolio-management/control-plane/internal/registry"
	"github.com/portfolio-management/control-plane/internal/signing"
	"github.com/portfolio-management/control-plane/internal/store"
)

type Server struct {
	cfg              config.Config
	keys             *signing.Store
	store            *store.Store
	grafana          *jwks.Client          // nil when GRAFANA_JWKS_URL is empty (smoke-only mode)
	kinde            *jwks.Client          // nil when KINDE_JWKS_URL is empty (Kinde-authed routes 503)
	grafanaAdmin     *grafanaclient.Client // nil when GRAFANA_BASE_URL is empty (onboarding routes 503)
	installer        *install.Installer
	registry         *registry.Client // plugin catalog/footprints/artifacts from the OCI registry
	logger           *slog.Logger
	adminRL          *adminRateLimiter
	lruPrimeClient   *http.Client
	gatewayTombstone *gatewayTombstoneClient // nil = uninstall worker disabled
}

func New(cfg config.Config, keys *signing.Store, st *store.Store, grafanaJWKS *jwks.Client, kindeJWKS *jwks.Client, installer *install.Installer, reg *registry.Client, grafanaAdmin *grafanaclient.Client, logger *slog.Logger) *Server {
	return &Server{
		cfg:          cfg,
		keys:         keys,
		store:        st,
		grafana:      grafanaJWKS,
		kinde:        kindeJWKS,
		grafanaAdmin: grafanaAdmin,
		installer:    installer,
		registry:     reg,
		logger:       logger,
		adminRL:      newAdminRateLimiter(),
		// Shared HTTP client for /internal/lru-prime fan-out. Idle
		// connections kept warm so the per-create notification cost is
		// one socket reuse, not a TCP/TLS handshake.
		lruPrimeClient: &http.Client{Timeout: 3 * time.Second},
	}
}

// WithGatewayTombstone is called by main.go after construction to
// install the tombstone client. Kept separate from New so the client
// can hold a closure over the Server's signTombstoneScope method
// (the Server doesn't exist yet at New-time).
func (s *Server) WithGatewayTombstone() {
	if s.cfg.GatewayBaseURL == "" {
		return
	}
	s.gatewayTombstone = newGatewayTombstoneClient(s.cfg.GatewayBaseURL, s.signTombstoneScope)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/jwt/jwks", s.handleJWKS)
	mux.HandleFunc("/jwt/mint", s.handleMint)

	// Phase 2 — canonical reference data. Writes (POST/PATCH) use operator-
	// token auth with org_id from the request body. Reads (GET) use session-
	// JWT auth with org_id from the verified claim (Track 1: portfolio reads
	// move to the canonical store so they are read-your-write consistent).
	mux.HandleFunc("POST /portfolios", s.handleUpsertPortfolio)
	mux.HandleFunc("GET /portfolios", s.handleListPortfolios)
	mux.HandleFunc("GET /portfolios/{portfolio_id}", s.handleGetPortfolio)
	mux.HandleFunc("PATCH /portfolios/{portfolio_id}", s.handlePatchPortfolio)
	// v6 Phase 3 — operator-only plugin install. Creates RW schema, role,
	// per-tenant views; generates and persists platform_token. Same admin
	// auth model as /admin/users/link (bootstrap token or Grafana JWT
	// resolving to user_org.role='admin' for the path's org_id).
	mux.HandleFunc("POST /orgs/{org_id}/plugins/{plugin_id}", s.handlePluginInstall)

	// v6 Phase 3 — admin endpoints. Grant or revoke a (provider,
	// external_id) -> user_id mapping that resolves upstream IdP JWTs
	// to control-plane membership. Authenticated via the env-gated
	// bootstrap token OR a Grafana JWT whose subject already has
	// user_org.role='admin' for the target org. See ADR-00xx.
	mux.HandleFunc("POST /admin/users/link", s.handleAdminUsersLink)
	mux.HandleFunc("DELETE /admin/users/link", s.handleAdminUsersUnlink)

	// v6 Phase 7 — landing-flow public surface. Authenticated by the
	// Grafana session cookie (server-side validated via grafana
	// /api/user). When GRAFANA_BASE_URL is empty these routes return
	// 503 because the middleware refuses to dial an unconfigured
	// Grafana.
	mux.HandleFunc("GET /api/me", s.requireGrafanaSession(s.handleMe))
	mux.HandleFunc("POST /api/onboarding/orgs", s.requireGrafanaSession(s.handleCreateOrg))

	// v6 Phase 8 — self-service plugin catalog + install/uninstall
	// (ADR-0050). Grafana-session-auth + admin-role check.
	mux.HandleFunc("GET /api/orgs/{org_id}/plugins", s.requireOrgAdmin(s.handleListPlugins))
	mux.HandleFunc("POST /api/orgs/{org_id}/plugins/{plugin_id}", s.requireOrgAdmin(s.handleInstallPlugin))
	mux.HandleFunc("DELETE /api/orgs/{org_id}/plugins/{plugin_id}", s.requireOrgAdmin(s.handleUninstallPlugin))
	mux.HandleFunc("GET /api/orgs/{org_id}/plugins/{plugin_id}/uninstall-status", s.requireOrgAdmin(s.handleUninstallStatus))

	// v8 — instance-bootstrap read endpoint. Each Grafana process (desktop
	// or cloud container) calls this on startup to fetch its org's
	// installed plugin set + platform_tokens, then renders Grafana
	// provisioning YAML before launching grafana-server. Bootstrap-token
	// authenticated.
	mux.HandleFunc("GET /v1/internal/orgs/{org_id}/plugins", s.handleInstanceListPlugins)
	mux.HandleFunc("GET /v1/internal/plugins/{id}/versions", s.handleInstanceListVersions)
	mux.HandleFunc("GET /v1/internal/plugins/{id}/versions/{version}/artifact", s.handleInstanceArtifact)

	// v8 — shell-facing endpoints. Kinde-access-token authenticated. The
	// desktop Tauri shell and the cloud portal call these before either
	// boots a Grafana process.
	mux.HandleFunc("GET /v1/me/orgs", s.requireKindeSession(s.handleV1MeOrgs))
	mux.HandleFunc("GET /v1/marketplace/catalog", s.requireKindeSession(s.handleV1MarketplaceCatalog))
	mux.HandleFunc("GET /v1/marketplace/plugins/{id}/versions", s.requireKindeSession(s.handleListPluginVersions))
	mux.HandleFunc("POST /v1/instance/token", s.requireKindeSession(s.handleV1InstanceToken))
	// Onboarding: a Kinde-authed user with no org creates one (becomes
	// admin; required plugins installed). Reuses handleCreateOrg, now
	// driven by the Kinde session instead of the retired Grafana session.
	mux.HandleFunc("POST /v1/orgs", s.requireKindeSession(s.handleCreateOrg))
	// Marketplace install/uninstall (desktop shell). Membership-gated;
	// delegates to the shared install + async-uninstall flow.
	mux.HandleFunc("POST /v1/orgs/{org_id}/plugins/{plugin_id}", s.requireKindeSession(s.handleV1InstallPlugin))
	mux.HandleFunc("DELETE /v1/orgs/{org_id}/plugins/{plugin_id}", s.requireKindeSession(s.handleV1UninstallPlugin))

	// Federated plugin sources (desktop shell). Kinde-authenticated.
	mux.HandleFunc("GET /v1/sources", s.requireKindeSession(s.handleListSources))
	mux.HandleFunc("POST /v1/sources", s.requireKindeSession(s.handleAddSource))
	mux.HandleFunc("DELETE /v1/sources", s.requireKindeSession(s.handleDeleteSource))

	return mux
}

// --- /healthz ---------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	active := s.keys.Active()
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"kid":    active.Kid,
	})
}

// --- /jwt/jwks --------------------------------------------------------------

// JWK shape: consumers (e.g. read-gateway) poll our JWKS to verify the
// session/capability JWTs control-plane mints at /jwt/mint. Keys are
// RSA-shape (`n`, `e`); control-plane signs RS256.
type jwk struct {
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	all := s.keys.All()
	out := jwksDoc{Keys: make([]jwk, 0, len(all))}
	for _, k := range all {
		out.Keys = append(out.Keys, rsaToJWK(k))
	}
	writeJSON(w, http.StatusOK, out)
}

// rsaToJWK serializes an RSA public key to a JWK with the RW-compatible
// {n, e} shape. n is the modulus as base64url-encoded big-endian bytes;
// e is the exponent encoded the same way.
func rsaToJWK(k *signing.Key) jwk {
	pub := k.Public
	nBytes := pub.N.Bytes()
	eInt := big.NewInt(int64(pub.E))
	eBytes := eInt.Bytes()
	return jwk{
		Kty: "RSA",
		N:   base64.RawURLEncoding.EncodeToString(nBytes),
		E:   base64.RawURLEncoding.EncodeToString(eBytes),
		Kid: k.Kid,
		Use: "sig",
		Alg: "RS256",
	}
}

// --- /jwt/mint --------------------------------------------------------------

// mintRequest accepts both v8 (Grafana-JWT bearer + platform_token body +
// explicit org_id) and Phase 1 static-IdP (static bearer + user_id body)
// shapes. The non-empty fields select the path. org_id is required on both
// paths; under v8 the control plane no longer derives org_id from the
// Grafana JWT's `aud=org:N` claim because every Grafana process is now
// single-org and the aud claim is uninformative.
type mintRequest struct {
	// Common — required on both paths.
	OrgID    string `json:"org_id"`
	PluginID string `json:"plugin_id"`

	// Phase 1 static-IdP fallback (smoke flow): caller supplies user_id and
	// the Bearer is matched against IDP_STATIC_USERS.
	UserID string `json:"user_id,omitempty"`

	// v8 production path: caller's Bearer is a Grafana-issued ID JWT
	// (X-Grafana-Id); platform_token authenticates the plugin process.
	PlatformToken string `json:"platform_token,omitempty"`
}

type mintResponse struct {
	JWT    string `json:"jwt"`
	Exp    int64  `json:"exp"`
	Kid    string `json:"kid"`
	UserID string `json:"user_id"`
	OrgID  string `json:"org_id"`
}

func (s *Server) handleMint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req mintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.PluginID == "" {
		http.Error(w, "plugin_id required", http.StatusBadRequest)
		return
	}
	if req.OrgID == "" {
		http.Error(w, "org_id required", http.StatusBadRequest)
		return
	}
	orgID, err := uuid.Parse(req.OrgID)
	if err != nil {
		http.Error(w, "org_id not a UUID", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var userID string
	switch {
	case req.PlatformToken != "":
		// v8 production path (identity model Option A): the plugin
		// presents an instance token (bearer) + platform_token (body).
		// The instance token is a control-plane-signed JWT the shell
		// obtained by exchanging a Kinde token at /v1/instance/token; it
		// proves the human belongs to org_id. control-plane verifies it
		// against its OWN JWKS — always reachable, unlike a desktop
		// Grafana's. This replaces the Grafana ID JWT, whose JWKS lives
		// on the (unreachable) laptop.
		uid, ok := s.authenticateInstance(ctx, r, req.PluginID, req.PlatformToken, orgID)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		userID = uid
	case req.UserID != "":
		// Phase 1 static-IdP fallback for smoke and bootstrap.
		tokenUserID, ok := s.authenticateStaticIdP(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if req.UserID != tokenUserID {
			s.logger.Warn("mint user mismatch", "token_user", tokenUserID, "req_user", req.UserID)
			http.Error(w, "user_id mismatch with bearer token", http.StatusForbidden)
			return
		}
		hasOrg, err := s.store.HasUserOrg(ctx, req.UserID, orgID)
		if err != nil {
			s.logger.Error("user_org lookup failed", "err", err)
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		if !hasOrg {
			http.Error(w, "user not a member of org", http.StatusForbidden)
			return
		}
		userID = req.UserID
	default:
		http.Error(w, "platform_token or user_id required", http.StatusBadRequest)
		return
	}

	hasPlugin, err := s.store.HasPluginInstall(ctx, orgID, req.PluginID)
	if err != nil {
		s.logger.Error("plugin_installs lookup failed", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if !hasPlugin {
		http.Error(w, "plugin not installed for org", http.StatusForbidden)
		return
	}

	tok, exp, err := s.sign(userID, orgID.String(), req.PluginID)
	if err != nil {
		s.logger.Error("sign failed", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, mintResponse{
		JWT:    tok,
		Exp:    exp,
		Kid:    s.keys.Active().Kid,
		UserID: userID,
		OrgID:  orgID.String(),
	})
}

// authenticateStaticIdP matches the bearer against IDP_STATIC_USERS. Phase 1
// fallback for smoke and dev bootstrap.
func (s *Server) authenticateStaticIdP(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}
	tok := strings.TrimPrefix(auth, prefix)
	for _, u := range s.cfg.IDPStaticUsers {
		if u.Token == tok {
			return u.UserID, true
		}
	}
	return "", false
}

// authenticateAny tries session-JWT first (the v6 path — plugin backends
// forward a control-plane-minted session JWT), then falls back to the
// static-IdP bearer used by smoke and operator tooling. Returns the
// resolved user identifier (JWT `sub` claim, or IDP_STATIC_USERS row).
func (s *Server) authenticateAny(r *http.Request) (string, bool) {
	if claims, ok := s.authenticateSession(r); ok {
		return claims.UserID, true
	}
	return s.authenticateStaticIdP(r)
}

func (s *Server) sign(userID, orgID, pluginID string) (string, int64, error) {
	active := s.keys.Active()
	if active == nil {
		return "", 0, errors.New("no active signing key")
	}
	now := time.Now().UTC()
	exp := now.Add(s.cfg.JWTLifetime)

	claims := jwt.MapClaims{
		"iss":       s.cfg.JWTIssuer,
		"aud":       s.cfg.JWTAudience,
		"sub":       userID,
		"org_id":    orgID,
		"plugin_id": pluginID,
		"iat":       now.Unix(),
		"exp":       exp.Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = active.Kid
	signed, err := tok.SignedString(active.Private)
	if err != nil {
		return "", 0, err
	}
	return signed, exp.Unix(), nil
}

// instanceTokenAudience scopes instance tokens so one can never be replayed
// as a session JWT (different aud) and vice-versa.
const instanceTokenAudience = "urn:pm:instance-token"

// instanceTokenTTL bounds an instance token's validity. The shell re-mints
// (via the Kinde exchange at /v1/instance/token) before expiry.
const instanceTokenTTL = time.Hour

// signInstance mints an instance token: a control-plane-signed JWT proving
// a Kinde-authenticated human (sub) belongs to org_id. It is the input
// credential a plugin presents to /jwt/mint, replacing the Grafana ID JWT
// — control-plane verifies it against its OWN JWKS, which is always
// reachable (a desktop Grafana's JWKS is not). v8 identity model Option A.
func (s *Server) signInstance(userID, orgID string) (string, int64, error) {
	active := s.keys.Active()
	if active == nil {
		return "", 0, errors.New("no active signing key")
	}
	now := time.Now().UTC()
	exp := now.Add(instanceTokenTTL)
	claims := jwt.MapClaims{
		"iss":    s.cfg.JWTIssuer,
		"aud":    instanceTokenAudience,
		"sub":    userID,
		"org_id": orgID,
		"typ":    "instance",
		"iat":    now.Unix(),
		"exp":    exp.Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = active.Kid
	signed, err := tok.SignedString(active.Private)
	if err != nil {
		return "", 0, err
	}
	return signed, exp.Unix(), nil
}

// verifyInstanceToken parses + validates an instance token against this
// control plane's own signing keys. Returns (userID, orgID) on success.
func (s *Server) verifyInstanceToken(tokenStr string) (userID, orgID string, ok bool) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(s.cfg.JWTIssuer),
		jwt.WithAudience(instanceTokenAudience),
		jwt.WithExpirationRequired(),
	)
	tok, err := parser.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		for _, k := range s.keys.All() {
			if k.Kid == kid {
				return k.Public, nil
			}
		}
		return nil, fmt.Errorf("unknown kid %q", kid)
	})
	if err != nil {
		s.logger.Warn("instance token verify failed", "err", err)
		return "", "", false
	}
	claims, isMap := tok.Claims.(jwt.MapClaims)
	if !isMap {
		return "", "", false
	}
	if typ, _ := claims["typ"].(string); typ != "instance" {
		return "", "", false
	}
	sub, _ := claims["sub"].(string)
	org, _ := claims["org_id"].(string)
	if sub == "" || org == "" {
		return "", "", false
	}
	return sub, org, true
}

// authenticateInstance is the v8 /jwt/mint production path. The plugin
// presents an instance token (bearer) + its platform_token (body). The
// instance token already proves the human belongs to the org (control
// plane checked membership at mint time), so this verifies the token's
// signature/claims, that the token's org matches the requested org, and
// that platform_token is bound to (org, plugin). Returns the user_id.
func (s *Server) authenticateInstance(ctx context.Context, r *http.Request, pluginID, platformToken string, orgID uuid.UUID) (string, bool) {
	bearer, ok := extractBearer(r)
	if !ok {
		return "", false
	}
	userID, tokenOrg, ok := s.verifyInstanceToken(bearer)
	if !ok {
		return "", false
	}
	if tokenOrg != orgID.String() {
		s.logger.Warn("instance token org mismatch", "token_org", tokenOrg, "req_org", orgID)
		return "", false
	}
	tokenOK, err := s.store.VerifyPluginToken(ctx, orgID, pluginID, platformToken)
	if err != nil {
		s.logger.Error("plugin token verify", "err", err)
		return "", false
	}
	if !tokenOK {
		s.logger.Warn("plugin token mismatch", "plugin", pluginID, "org", orgID)
		return "", false
	}
	return userID, true
}

// --- /portfolios -----------------------------------------------------------

type portfolioBody struct {
	OrgID        string         `json:"org_id"`
	PortfolioID  string         `json:"portfolio_id"`
	BaseCurrency string         `json:"base_currency"`
	Attributes   map[string]any `json:"attributes"`
}

func (s *Server) handleUpsertPortfolio(w http.ResponseWriter, r *http.Request) {
	updatedBy, ok := s.authenticateAny(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body portfolioBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.OrgID == "" || body.PortfolioID == "" || body.BaseCurrency == "" {
		http.Error(w, "org_id, portfolio_id, base_currency required", http.StatusBadRequest)
		return
	}
	in, err := portfolioInputFromBody(body, "", updatedBy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.upsertPortfolio(w, r, in, http.StatusCreated)
}

// handleListPortfolios returns the calling org's portfolios. Org scope comes
// from the verified session JWT (org_id claim), never a request parameter —
// the same security spine the write path and the RW per-org views rely on.
func (s *Server) handleListPortfolios(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authenticateSession(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	orgID, err := uuid.Parse(claims.OrgID)
	if err != nil {
		http.Error(w, "org_id claim not a UUID", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	records, err := s.store.ListPortfoliosForOrg(ctx, orgID)
	if err != nil {
		s.logger.Error("list portfolios", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, records)
}

// handleGetPortfolio returns one portfolio scoped to the caller's org. A
// portfolio_id outside the org (or absent) is a 404 — the org-scoped query
// never discloses another org's rows.
func (s *Server) handleGetPortfolio(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authenticateSession(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	orgID, err := uuid.Parse(claims.OrgID)
	if err != nil {
		http.Error(w, "org_id claim not a UUID", http.StatusUnauthorized)
		return
	}
	portfolioID, err := uuid.Parse(r.PathValue("portfolio_id"))
	if err != nil {
		http.Error(w, "portfolio_id not a UUID", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rec, found, err := s.store.GetPortfolio(ctx, orgID, portfolioID)
	if err != nil {
		s.logger.Error("get portfolio", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handlePatchPortfolio(w http.ResponseWriter, r *http.Request) {
	updatedBy, ok := s.authenticateAny(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	pathID := r.PathValue("portfolio_id")
	var body portfolioBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.OrgID == "" || body.BaseCurrency == "" {
		http.Error(w, "org_id, base_currency required", http.StatusBadRequest)
		return
	}
	in, err := portfolioInputFromBody(body, pathID, updatedBy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.upsertPortfolio(w, r, in, http.StatusOK)
}

func portfolioInputFromBody(body portfolioBody, pathID, updatedBy string) (store.PortfolioInput, error) {
	orgID, err := uuid.Parse(body.OrgID)
	if err != nil {
		return store.PortfolioInput{}, errors.New("org_id not a UUID")
	}
	idStr := body.PortfolioID
	if pathID != "" {
		idStr = pathID
	}
	portfolioID, err := uuid.Parse(idStr)
	if err != nil {
		return store.PortfolioInput{}, errors.New("portfolio_id not a UUID")
	}
	return store.PortfolioInput{
		OrgID:        orgID,
		PortfolioID:  portfolioID,
		BaseCurrency: body.BaseCurrency,
		Attributes:   body.Attributes,
		UpdatedBy:    updatedBy,
	}, nil
}

func (s *Server) upsertPortfolio(w http.ResponseWriter, r *http.Request, in store.PortfolioInput, okStatus int) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.store.UpsertPortfolio(ctx, in); err != nil {
		s.logger.Error("upsert portfolio", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	// v6 Phase 6: write-through to every gateway. After the row is
	// durable in control_db, prime the gateway's in-process LRU so
	// the next /v6/* request for this portfolio_id hits the cache
	// instead of falling through to the replica miss path. ADR-0034:
	// a missed notification self-heals via the replica, so we don't
	// gate the user-visible response on delivery.
	s.notifyGatewayLRUPrime(ctx, in.PortfolioID, in.OrgID)
	writeJSON(w, okStatus, map[string]string{
		"org_id":       in.OrgID.String(),
		"portfolio_id": in.PortfolioID.String(),
	})
}

// --- /orgs/{org}/plugins/{plugin} ------------------------------------------

type installResponse struct {
	OrgID         string `json:"org_id"`
	PluginID      string `json:"plugin_id"`
	ShortID       string `json:"short_id"`
	SQLitePath    string `json:"sqlite_path"`
	PlatformToken string `json:"platform_token"`
}

// handlePluginInstall provisions a plugin install for an org: creates
// the filesystem path and persists a per-install platform_token.
// Idempotent. No per-org RW schema/views/role is created — the
// read-gateway is the sole RW reader.
//
// Authenticated via the same admin path as /admin/users/link:
// bootstrap token OR Grafana JWT resolving to user_org.role='admin'
// for the target org. Static-IdP smoke bearers are no longer accepted
// here; bootstrap is the substitute for first-stack provisioning.
func (s *Server) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	ip := requestIP(r)
	if !s.adminRL.take(ip, "admin", rlAdminPerMin) {
		s.audit(r.Context(), store.AuditEntry{
			Action:    auditActionPluginInstall,
			Result:    "denied:rate-limited",
			RequestIP: ip,
		})
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	caller, denyReason := s.authenticateAdmin(ctx, r)
	if caller.Kind == callerNone {
		if denyReason == "" {
			denyReason = "unauthorized"
		}
		s.audit(ctx, store.AuditEntry{
			Action:    auditActionPluginInstall,
			Result:    "denied:" + denyReason,
			RequestIP: ip,
		})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if s.installer == nil {
		http.Error(w, "installer not configured", http.StatusServiceUnavailable)
		return
	}

	orgIDStr := r.PathValue("org_id")
	pluginID := r.PathValue("plugin_id")
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionPluginInstall,
			Target:      map[string]any{"org_id": orgIDStr, "plugin_id": pluginID},
			Result:      "denied:bad-org-id",
			RequestIP:   ip,
		})
		http.Error(w, "org_id not a UUID", http.StatusBadRequest)
		return
	}

	if deny := s.requireAdminInOrg(ctx, caller, orgID); deny != "" {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionPluginInstall,
			Target:      map[string]any{"org_id": orgIDStr, "plugin_id": pluginID},
			Result:      "denied:" + deny,
			RequestIP:   ip,
		})
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	shortID, err := s.store.GetOrgShortID(ctx, orgID)
	if err != nil {
		s.logger.Error("org short_id lookup", "err", err, "org_id", orgID)
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionPluginInstall,
			Target:      map[string]any{"org_id": orgIDStr, "plugin_id": pluginID},
			Result:      "denied:org-not-found",
			RequestIP:   ip,
		})
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}

	rp, found, err := s.registry.Get(ctx, pluginID)
	if err != nil {
		s.logger.Error("plugin install: registry lookup", "err", err, "plugin_id", pluginID)
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	}
	if !found {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionPluginInstall,
			Target:      map[string]any{"org_id": orgIDStr, "plugin_id": pluginID},
			Result:      "denied:unknown-plugin",
			RequestIP:   ip,
		})
		http.Error(w, "unknown plugin", http.StatusNotFound)
		return
	}

	res, err := s.installer.Install(ctx, orgID, shortID, rp.Footprint)
	if err != nil {
		s.logger.Error("plugin install", "err", err, "org_id", orgID, "plugin_id", pluginID)
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionPluginInstall,
			Target:      map[string]any{"org_id": orgIDStr, "plugin_id": pluginID},
			Result:      "denied:internal",
			RequestIP:   ip,
		})
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	s.audit(ctx, store.AuditEntry{
		Actor:       caller.UserID,
		ActorSource: callerSourceLabel(caller.Kind),
		Action:      auditActionPluginInstall,
		Target:      map[string]any{"org_id": orgIDStr, "plugin_id": pluginID},
		Result:      "ok",
		RequestIP:   ip,
	})
	writeJSON(w, http.StatusOK, installResponse{
		OrgID:         res.OrgID.String(),
		PluginID:      res.PluginID,
		ShortID:       res.ShortID,
		SQLitePath:    res.SQLitePath,
		PlatformToken: res.PlatformToken,
	})
}

// signTombstoneScope mints a short-lived capability JWT for the
// gateway /internal/tombstone endpoint (ADR-0050). Type-B token in
// the README of the design — represents a CAPABILITY to delete data
// in a given scope, not a user identity.
//
// Claims:
//   - aud = cfg.TombstoneJWTAudience (default "gateway-tombstone")
//   - sub = "uninstall:<org_id>:<plugin_id>" (opaque job identifier)
//   - scope_org_id / scope_plugin_id / scope_topics (capability scope)
//   - exp = now + cfg.TombstoneJWTLifetime (default 30s)
//   - jti = random nonce
//
// Gateway verifies the signature against the same JWKS it polls for
// session JWTs; the audience differentiates the two token types.
func (s *Server) signTombstoneScope(orgID, pluginID string, topics []string) (string, error) {
	active := s.keys.Active()
	if active == nil {
		return "", errors.New("no active signing key")
	}
	now := time.Now().UTC()
	exp := now.Add(s.cfg.TombstoneJWTLifetime)

	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}

	claims := jwt.MapClaims{
		"iss":             s.cfg.JWTIssuer,
		"aud":             s.cfg.TombstoneJWTAudience,
		"sub":             "uninstall:" + orgID + ":" + pluginID,
		"scope_org_id":    orgID,
		"scope_plugin_id": pluginID,
		"scope_topics":    topics,
		"iat":             now.Unix(),
		"exp":             exp.Unix(),
		"jti":             base64.RawURLEncoding.EncodeToString(jti),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = active.Kid
	return tok.SignedString(active.Private)
}

// authenticateSession verifies the bearer session JWT against the local
// signing key set, with the same iss/aud the mint endpoint uses.
func (s *Server) authenticateSession(r *http.Request) (signing.SessionClaims, bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return signing.SessionClaims{}, false
	}
	tok := strings.TrimPrefix(auth, prefix)
	claims, err := s.keys.VerifySession(tok, s.cfg.JWTIssuer, s.cfg.JWTAudience)
	if err != nil {
		s.logger.Warn("session verify failed", "err", err)
		return signing.SessionClaims{}, false
	}
	return claims, true
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// splitCSV splits a comma-separated env value into trimmed, non-empty parts.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
