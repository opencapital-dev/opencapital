package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/portfolio-management/control-plane/internal/store"
)

// Source labels attached to audit rows. Keep in sync with the
// actor_source values documented in the 0015_audit_log migration.
const (
	auditSourceBootstrap = "bootstrap"
	auditSourceGrafana   = "grafana"
)

// Actions logged by the admin endpoints. Single closed set so the
// audit_log can be filtered by action without surprises.
const (
	auditActionUsersLink     = "users.link"
	auditActionUsersUnlink   = "users.unlink"
	auditActionPluginInstall = "plugins.install"
)

// Allow-lists for closed enums on the link/unlink endpoints. Anything
// outside these sets is a 400 — clients don't get to pick novel
// providers / roles.
var (
	allowedProviders = map[string]bool{
		"grafana": true,
		"kinde":   true,
	}
	allowedRoles = map[string]bool{
		"admin":    true,
		"operator": true,
		"member":   true,
	}
)

// Per-provider regex for the external_id field. Each value carries an
// explicit shape so a malformed handle never reaches storage.
var providerExternalIDRegex = map[string]*regexp.Regexp{
	"grafana": regexp.MustCompile(`^user:[1-9][0-9]{0,18}$`),
	"kinde":   regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`),
}

// userIDRegex constrains the canonical user_id stored in user_org and
// surfaced in audit logs. Tight enough to stay greppable; loose enough
// for email-style ids and Kinde subs.
var userIDRegex = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,128}$`)

// --- caller resolution ------------------------------------------------------

type adminCallerKind int

const (
	callerNone adminCallerKind = iota
	callerBootstrap
	callerAdmin
)

// adminCaller is the resolved identity an admin endpoint operates as.
// For bootstrap callers UserID and ExternalID are empty — that signals
// "no human caller" and disables the self-edit guard.
type adminCaller struct {
	Kind   adminCallerKind
	UserID string
	// ExternalID is the (provider=grafana) handle the JWT carried for
	// Grafana callers; "" for bootstrap. Used by the self-edit guard.
	ExternalID string
}

// authenticateAdmin resolves the caller of an /admin/* request to one of
// three states: bootstrap (env-gated token match), admin (Grafana JWT
// resolved to a control-plane user via user_external_ids), or none. The
// returned `denyReason` lets the audit row record which precondition
// failed. The bootstrap path is short-circuited when
// AdminBootstrapToken is empty so an unconfigured stack never even
// reaches the comparison.
//
// Under v8 this function no longer derives an "acting-as" org from the
// Grafana JWT's `aud=org:N` claim. The target org is whichever org the
// admin endpoint's body specifies, and the admin-in-org check happens
// at handler time via requireAdminInOrg.
func (s *Server) authenticateAdmin(ctx context.Context, r *http.Request) (adminCaller, string) {
	bearer, ok := extractBearer(r)
	if !ok {
		return adminCaller{}, "no-bearer"
	}

	// 1. Bootstrap path. Constant-time compare against the env-gated
	//    secret; absent secret = path disabled (no compare even runs).
	if s.cfg.AdminBootstrapToken != "" {
		if subtle.ConstantTimeCompare([]byte(bearer), []byte(s.cfg.AdminBootstrapToken)) == 1 {
			return adminCaller{Kind: callerBootstrap}, ""
		}
	}

	// 2. Grafana JWT path. Verify the token against Grafana's JWKS the
	//    same way /jwt/mint does, then resolve the JWT sub to a
	//    canonical user_id via user_external_ids. Org membership is
	//    NOT enforced here — handlers do that against the body's org_id
	//    via requireAdminInOrg.
	if s.grafana == nil {
		return adminCaller{}, "grafana-jwks-not-configured"
	}
	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithExpirationRequired(),
	}
	if s.cfg.GrafanaJWTIssuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(s.cfg.GrafanaJWTIssuer))
	}
	if s.cfg.GrafanaJWTAudience != "" {
		parserOpts = append(parserOpts, jwt.WithAudience(s.cfg.GrafanaJWTAudience))
	}
	parser := jwt.NewParser(parserOpts...)
	keyFunc := s.grafana.KeyFunc(ctx)
	tok, err := parser.Parse(bearer, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		return keyFunc(kid)
	})
	if err != nil {
		return adminCaller{}, "jwt-invalid"
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return adminCaller{}, "jwt-claims"
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return adminCaller{}, "jwt-no-sub"
	}
	userID, mappingFound, err := s.store.UserIDByExternalID(ctx, "grafana", sub)
	if err != nil {
		s.logger.Error("admin auth: external_id lookup", "err", err)
		return adminCaller{}, "store-error"
	}
	if !mappingFound {
		return adminCaller{}, "no-user-mapping"
	}
	return adminCaller{
		Kind:       callerAdmin,
		UserID:     userID,
		ExternalID: sub,
	}, ""
}

// requireAdminInOrg checks the caller has user_org.role='admin' for the
// supplied org. Returns empty string on success or a short deny-reason
// (suitable for the audit row's Result field) on rejection. Bootstrap
// callers bypass the check unconditionally — operator break-glass is
// org-agnostic.
func (s *Server) requireAdminInOrg(ctx context.Context, caller adminCaller, orgID uuid.UUID) string {
	if caller.Kind == callerBootstrap {
		return ""
	}
	role, found, err := s.store.RoleForUserOrg(ctx, caller.UserID, orgID)
	if err != nil {
		s.logger.Error("admin authz: role lookup", "err", err)
		return "store-error"
	}
	if !found {
		return "not-a-member"
	}
	if role != "admin" {
		return "not-admin"
	}
	return ""
}

func extractBearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	tok := strings.TrimPrefix(h, prefix)
	if tok == "" {
		return "", false
	}
	return tok, true
}

// --- rate limit -------------------------------------------------------------

// adminRateLimiter is a per-IP token bucket sized to deter brute-force
// scans without affecting normal operator flow. Two budgets: one for
// authenticated admin calls (10/min), a tighter one for bootstrap
// attempts (1/min) so a stolen-but-unused bootstrap token doesn't get
// brute-forced before the operator notices.
type adminRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rlBucket
}

type rlBucket struct {
	count       int
	windowStart time.Time
}

func newAdminRateLimiter() *adminRateLimiter {
	return &adminRateLimiter{buckets: map[string]*rlBucket{}}
}

const (
	rlAdminPerMin     = 10
	rlBootstrapPerMin = 1
	rlWindow          = time.Minute
)

// take consumes one slot for the given (ip, key) bucket and returns
// false when the call is over budget. Key separates bootstrap attempts
// from authenticated admin calls so an attacker probing for the
// bootstrap token can't be hidden inside a flood of legitimate admin
// calls.
func (l *adminRateLimiter) take(ip, key string, perMin int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	id := ip + "|" + key
	b, ok := l.buckets[id]
	if !ok || now.Sub(b.windowStart) >= rlWindow {
		l.buckets[id] = &rlBucket{count: 1, windowStart: now}
		return true
	}
	if b.count >= perMin {
		return false
	}
	b.count++
	return true
}

// requestIP returns the caller's source IP. Caddy at the edge sets
// X-Forwarded-For; we trust the LAST hop since only Caddy reaches us
// in prod (control-plane is on the internal compose network). Falls
// back to the connection's remote address for dev / localhost.
func requestIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		ip := strings.TrimSpace(parts[len(parts)-1])
		if ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- request bodies ---------------------------------------------------------

type linkRequest struct {
	OrgID      string `json:"org_id"`
	UserID     string `json:"user_id"`
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	Role       string `json:"role"`
}

type unlinkRequest struct {
	OrgID      string `json:"org_id"`
	UserID     string `json:"user_id"`
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
}

type linkResponse struct {
	OrgID              string `json:"org_id"`
	UserID             string `json:"user_id"`
	Provider           string `json:"provider"`
	ExternalID         string `json:"external_id"`
	Role               string `json:"role"`
	ExternalIDInserted bool   `json:"external_id_inserted"`
}

type unlinkResponse struct {
	OrgID                string `json:"org_id"`
	UserID               string `json:"user_id"`
	Provider             string `json:"provider"`
	ExternalID           string `json:"external_id"`
	OrgMembershipRemoved bool   `json:"org_membership_removed"`
}

// decodeStrict parses the request body with unknown-field rejection so
// a typo or injected extra key is a 400, not a silent accept.
func decodeStrict(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Refuse trailing junk after the JSON object — same defense against
	// content smuggling as DisallowUnknownFields.
	if dec.More() {
		return errors.New("unexpected trailing data")
	}
	return nil
}

// --- handlers ---------------------------------------------------------------

// handleAdminUsersLink writes a (provider, external_id) -> user_id
// mapping and ensures the user_id has the requested role in org_id.
// See the plan + 0014/0015 migrations for the trust model.
func (s *Server) handleAdminUsersLink(w http.ResponseWriter, r *http.Request) {
	ip := requestIP(r)
	if !s.adminRL.take(ip, "admin", rlAdminPerMin) {
		s.audit(r.Context(), store.AuditEntry{
			Action:    auditActionUsersLink,
			Result:    "denied:rate-limited",
			RequestIP: ip,
		})
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	caller, denyReason := s.authenticateAdmin(ctx, r)
	if caller.Kind == callerNone {
		if denyReason == "" {
			denyReason = "unauthorized"
		}
		s.audit(ctx, store.AuditEntry{
			Action:    auditActionUsersLink,
			Result:    "denied:" + denyReason,
			RequestIP: ip,
		})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var body linkRequest
	if err := decodeStrict(r, &body); err != nil {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersLink,
			Result:      "denied:bad-body",
			RequestIP:   ip,
		})
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	orgID, valErr := validateLinkRequest(body)
	if valErr != "" {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersLink,
			Target:      linkTarget(body),
			Result:      "denied:" + valErr,
			RequestIP:   ip,
		})
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Org existence + scope checks. Bootstrap callers bypass the same-org
	// check; admins must be operating in the org they admin.
	if _, err := s.store.GetOrgShortID(ctx, orgID); err != nil {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersLink,
			Target:      linkTarget(body),
			Result:      "denied:org-not-found",
			RequestIP:   ip,
		})
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	if deny := s.requireAdminInOrg(ctx, caller, orgID); deny != "" {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersLink,
			Target:      linkTarget(body),
			Result:      "denied:" + deny,
			RequestIP:   ip,
		})
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if caller.Kind == callerAdmin && caller.UserID == body.UserID {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersLink,
			Target:      linkTarget(body),
			Result:      "denied:self-edit",
			RequestIP:   ip,
		})
		http.Error(w, "self-edit forbidden", http.StatusForbidden)
		return
	}

	createdBy := caller.UserID
	if caller.Kind == callerBootstrap {
		createdBy = "bootstrap"
	}
	result, err := s.store.LinkUser(ctx, body.Provider, body.ExternalID, body.UserID, orgID, body.Role, createdBy)
	if err != nil {
		if errors.Is(err, store.ErrExternalIDPointsElsewhere) {
			s.audit(ctx, store.AuditEntry{
				Actor:       caller.UserID,
				ActorSource: callerSourceLabel(caller.Kind),
				Action:      auditActionUsersLink,
				Target:      linkTarget(body),
				Result:      "denied:relink-explicit",
				RequestIP:   ip,
			})
			http.Error(w, "external_id already mapped to a different user_id", http.StatusBadRequest)
			return
		}
		s.logger.Error("link user", "err", err)
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersLink,
			Target:      linkTarget(body),
			Result:      "denied:internal",
			RequestIP:   ip,
		})
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	s.audit(ctx, store.AuditEntry{
		Actor:       caller.UserID,
		ActorSource: callerSourceLabel(caller.Kind),
		Action:      auditActionUsersLink,
		Target:      linkTarget(body),
		Result:      "ok",
		RequestIP:   ip,
	})
	writeJSON(w, http.StatusCreated, linkResponse{
		OrgID:              body.OrgID,
		UserID:             body.UserID,
		Provider:           body.Provider,
		ExternalID:         body.ExternalID,
		Role:               body.Role,
		ExternalIDInserted: result.ExternalIDInserted,
	})
}

// handleAdminUsersUnlink removes a (provider, external_id) -> user_id
// mapping. If no other mapping resolves to the same user_id, the
// user_org row is also dropped. Refuses to leave an org with zero
// admins (last-admin guard).
func (s *Server) handleAdminUsersUnlink(w http.ResponseWriter, r *http.Request) {
	ip := requestIP(r)
	if !s.adminRL.take(ip, "admin", rlAdminPerMin) {
		s.audit(r.Context(), store.AuditEntry{
			Action:    auditActionUsersUnlink,
			Result:    "denied:rate-limited",
			RequestIP: ip,
		})
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	caller, denyReason := s.authenticateAdmin(ctx, r)
	if caller.Kind == callerNone {
		if denyReason == "" {
			denyReason = "unauthorized"
		}
		s.audit(ctx, store.AuditEntry{
			Action:    auditActionUsersUnlink,
			Result:    "denied:" + denyReason,
			RequestIP: ip,
		})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var body unlinkRequest
	if err := decodeStrict(r, &body); err != nil {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersUnlink,
			Result:      "denied:bad-body",
			RequestIP:   ip,
		})
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	orgID, valErr := validateUnlinkRequest(body)
	if valErr != "" {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersUnlink,
			Target:      unlinkTarget(body),
			Result:      "denied:" + valErr,
			RequestIP:   ip,
		})
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if _, err := s.store.GetOrgShortID(ctx, orgID); err != nil {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersUnlink,
			Target:      unlinkTarget(body),
			Result:      "denied:org-not-found",
			RequestIP:   ip,
		})
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	if deny := s.requireAdminInOrg(ctx, caller, orgID); deny != "" {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersUnlink,
			Target:      unlinkTarget(body),
			Result:      "denied:" + deny,
			RequestIP:   ip,
		})
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if caller.Kind == callerAdmin && caller.UserID == body.UserID {
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersUnlink,
			Target:      unlinkTarget(body),
			Result:      "denied:self-edit",
			RequestIP:   ip,
		})
		http.Error(w, "self-edit forbidden", http.StatusForbidden)
		return
	}

	// Last-admin guard. Refuse to drop the count of admins to zero in
	// the target org — forces the operator to promote a replacement
	// first. Bootstrap bypasses (operator may need to reseed).
	if caller.Kind != callerBootstrap {
		// Resolve target user's CURRENT role in the org to know whether
		// removing this mapping might also drop their org_org row, which
		// could in turn drop the admin count.
		_, targetRole, _, err := s.store.UserOrgByExternalID(ctx, body.Provider, body.ExternalID, orgID)
		if err != nil {
			s.logger.Error("unlink: lookup target role", "err", err)
			s.audit(ctx, store.AuditEntry{
				Actor:       caller.UserID,
				ActorSource: callerSourceLabel(caller.Kind),
				Action:      auditActionUsersUnlink,
				Target:      unlinkTarget(body),
				Result:      "denied:internal",
				RequestIP:   ip,
			})
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		if targetRole == "admin" {
			adminCount, err := s.store.CountOrgAdmins(ctx, orgID)
			if err != nil {
				s.logger.Error("unlink: count admins", "err", err)
				s.audit(ctx, store.AuditEntry{
					Actor:       caller.UserID,
					ActorSource: callerSourceLabel(caller.Kind),
					Action:      auditActionUsersUnlink,
					Target:      unlinkTarget(body),
					Result:      "denied:internal",
					RequestIP:   ip,
				})
				http.Error(w, "internal", http.StatusInternalServerError)
				return
			}
			if adminCount <= 1 {
				s.audit(ctx, store.AuditEntry{
					Actor:       caller.UserID,
					ActorSource: callerSourceLabel(caller.Kind),
					Action:      auditActionUsersUnlink,
					Target:      unlinkTarget(body),
					Result:      "denied:last-admin",
					RequestIP:   ip,
				})
				http.Error(w, "would leave org with zero admins", http.StatusConflict)
				return
			}
		}
	}

	result, err := s.store.UnlinkUser(ctx, body.Provider, body.ExternalID, body.UserID, orgID)
	if err != nil {
		if errors.Is(err, store.ErrUserExternalIDNotFound) {
			s.audit(ctx, store.AuditEntry{
				Actor:       caller.UserID,
				ActorSource: callerSourceLabel(caller.Kind),
				Action:      auditActionUsersUnlink,
				Target:      unlinkTarget(body),
				Result:      "denied:not-found",
				RequestIP:   ip,
			})
			http.Error(w, "mapping not found", http.StatusNotFound)
			return
		}
		s.logger.Error("unlink user", "err", err)
		s.audit(ctx, store.AuditEntry{
			Actor:       caller.UserID,
			ActorSource: callerSourceLabel(caller.Kind),
			Action:      auditActionUsersUnlink,
			Target:      unlinkTarget(body),
			Result:      "denied:internal",
			RequestIP:   ip,
		})
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	s.audit(ctx, store.AuditEntry{
		Actor:       caller.UserID,
		ActorSource: callerSourceLabel(caller.Kind),
		Action:      auditActionUsersUnlink,
		Target:      unlinkTarget(body),
		Result:      "ok",
		RequestIP:   ip,
	})
	writeJSON(w, http.StatusOK, unlinkResponse{
		OrgID:                body.OrgID,
		UserID:               body.UserID,
		Provider:             body.Provider,
		ExternalID:           body.ExternalID,
		OrgMembershipRemoved: result.OrgMembershipRemoved,
	})
}

// --- validation -------------------------------------------------------------

func validateLinkRequest(req linkRequest) (uuid.UUID, string) {
	orgID, reason := validateCommonFields(req.OrgID, req.UserID, req.Provider, req.ExternalID)
	if reason != "" {
		return uuid.Nil, reason
	}
	if !allowedRoles[req.Role] {
		return uuid.Nil, "bad-role"
	}
	return orgID, ""
}

func validateUnlinkRequest(req unlinkRequest) (uuid.UUID, string) {
	return validateCommonFields(req.OrgID, req.UserID, req.Provider, req.ExternalID)
}

func validateCommonFields(orgIDStr, userID, provider, externalID string) (uuid.UUID, string) {
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		return uuid.Nil, "bad-org-id"
	}
	if !userIDRegex.MatchString(userID) {
		return uuid.Nil, "bad-user-id"
	}
	if !allowedProviders[provider] {
		return uuid.Nil, "bad-provider"
	}
	re := providerExternalIDRegex[provider]
	if re == nil || !re.MatchString(externalID) {
		return uuid.Nil, "bad-external-id"
	}
	return orgID, ""
}

// --- audit helpers ----------------------------------------------------------

func callerSourceLabel(kind adminCallerKind) string {
	switch kind {
	case callerBootstrap:
		return auditSourceBootstrap
	case callerAdmin:
		return auditSourceGrafana
	}
	return ""
}

// audit is a log-and-continue wrapper. Audit-write failures must not
// block the caller-facing response — a write that succeeded in the
// real path still gets returned 2xx even if the audit row couldn't be
// persisted (the operational logger gets the error so an operator
// catches the gap).
func (s *Server) audit(ctx context.Context, entry store.AuditEntry) {
	// Run in the request's context for cancellation but with a hard
	// timeout so a slow audit insert can't hang a request that's
	// otherwise ready to return.
	auditCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.store.WriteAuditLog(auditCtx, entry); err != nil {
		s.logger.Error("audit write failed",
			"err", err,
			"action", entry.Action,
			"result", entry.Result)
	}
}

func linkTarget(req linkRequest) map[string]any {
	return map[string]any{
		"org_id":      req.OrgID,
		"user_id":     req.UserID,
		"provider":    req.Provider,
		"external_id": req.ExternalID,
		"role":        req.Role,
	}
}

func unlinkTarget(req unlinkRequest) map[string]any {
	return map[string]any{
		"org_id":      req.OrgID,
		"user_id":     req.UserID,
		"provider":    req.Provider,
		"external_id": req.ExternalID,
	}
}
