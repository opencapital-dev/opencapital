package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// v8 Kinde access-token middleware. The desktop Tauri shell and the
// cloud portal both reach the control plane directly using a Kinde
// access token in Authorization: Bearer. This middleware verifies the
// token against the Kinde JWKS, resolves the JWT sub to a control-plane
// user via user_external_ids (lazy create on first sight), and stashes
// a callerSession on the request context that the downstream handler
// reads via sessionFromContext.
//
// Mirrors requireGrafanaSession's contract (same callerSession shape)
// so handlers don't have to care which auth path delivered them.

// requireKindeSession wraps a handler with Kinde-access-token verification.
// 401 if the token is missing or invalid; 503 if Kinde JWKS isn't
// configured on the control plane.
func (s *Server) requireKindeSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.kinde == nil {
			http.Error(w, "kinde not configured", http.StatusServiceUnavailable)
			return
		}
		bearer, ok := extractBearer(r)
		if !ok {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		parserOpts := []jwt.ParserOption{
			jwt.WithValidMethods([]string{"RS256", "ES256"}),
			jwt.WithExpirationRequired(),
		}
		if s.cfg.KindeIssuer != "" {
			parserOpts = append(parserOpts, jwt.WithIssuer(s.cfg.KindeIssuer))
		}
		if s.cfg.KindeAudience != "" {
			parserOpts = append(parserOpts, jwt.WithAudience(s.cfg.KindeAudience))
		}
		parser := jwt.NewParser(parserOpts...)
		keyFunc := s.kinde.KeyFunc(ctx)
		tok, err := parser.Parse(bearer, func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			if kid == "" {
				return nil, errors.New("missing kid")
			}
			return keyFunc(kid)
		})
		if err != nil {
			s.logger.Warn("kinde JWT verify failed", "err", err)
			http.Error(w, "invalid kinde token", http.StatusUnauthorized)
			return
		}
		claims, ok := tok.Claims.(jwt.MapClaims)
		if !ok {
			http.Error(w, "bad kinde claims", http.StatusUnauthorized)
			return
		}
		sub, _ := claims["sub"].(string)
		if sub == "" {
			http.Error(w, "no sub", http.StatusUnauthorized)
			return
		}
		email, _ := claims["email"].(string)
		// Lazy-create the control_db user the first time we see this
		// kinde sub. Canonical user_id defaults to email when present, sub
		// otherwise. Same pattern as the Grafana session middleware uses.
		proposed := email
		if proposed == "" {
			proposed = sub
		}
		userID, err := s.store.EnsureUserExternalID(ctx, "kinde", sub, proposed, "kinde-login")
		if err != nil {
			s.logger.Error("kinde: ensure user_external_ids", "err", err)
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}

		session := callerSession{
			GrafanaLogin: sub,
			GrafanaEmail: email,
			UserID:       userID,
		}
		ctx = context.WithValue(r.Context(), sessionCtxKey{}, session)
		next(w, r.WithContext(ctx))
	}
}
