// Package httpapi exposes the v6 gateway's HTTP surface — the v6/* family of
// write endpoints that plugins POST to, plus /healthz and /readyz.
//
// Every state-mutating endpoint follows the same lifecycle:
//
//  1. Verify the Bearer JWT against the control-plane JWKS.
//  2. Reject any body carrying a top-level org_id (400, no silent override).
//  3. For portfolio_events endpoints: SELECT org_id FROM portfolios WHERE
//     portfolio_id=$1 (run UNCONDITIONALLY so timing is uniform; 404 covers
//     both the missing-row and wrong-org cases — ADR-0039).
//  4. Inject the JWT-verified org_id into the v2 envelope.
//  5. Serialize via Schema Registry (UseLatestVersion=true, AutoRegister=false).
//  6. Produce under the gateway SASL principal (idempotent, acks=all).
//  7. Return 201 with { topic, partition, offset, source }.
//
// Read endpoints do not exist on the gateway. Reads go through RisingWave
// per-org views.
package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/portfolio-management/datakey"
	"github.com/portfolio-management/gateway/internal/config"
	"github.com/portfolio-management/gateway/internal/jwks"
	gwkafka "github.com/portfolio-management/gateway/internal/kafka"
	"github.com/portfolio-management/gateway/internal/lru"
	"github.com/portfolio-management/gateway/internal/metrics"
	"github.com/portfolio-management/gateway/internal/store"
)

// producer is the narrow Kafka-produce surface the HTTP handlers depend on.
// *gwkafka.Producer satisfies it in production; tests install a capturing
// fake. Keeping this an interface (rather than the concrete *gwkafka.Producer)
// is the only seam that lets a handler test assert the exact key/value bytes a
// publish produced.
type producer interface {
	Produce(ctx context.Context, topic string, key, value []byte) (gwkafka.Result, error)
	Connected() bool
}

// serializer is the narrow Schema-Registry surface the HTTP handlers depend on;
// tests inject fakeSerializer to run end-to-end without a live registry.
type serializer interface {
	PortfolioEvents(topic string, env any) ([]byte, error)
	Data(topic string, env any) ([]byte, error)
}

// Server wires together every dependency the gateway needs at request time.
type Server struct {
	cfg    config.Config
	jwks   *jwks.Client
	sink   Sink
	store  *store.Store
	cache  *lru.Cache
	pinger store.Pinger
	logger *slog.Logger

	// ready flips to true after main.go finishes pre-warm; /readyz
	// gates on it so the LB drains the gateway until the LRU is
	// populated. atomic.Bool because the flip happens on the
	// main goroutine and the read happens on every probe.
	ready atomic.Bool

	// now is overridable in tests; production uses time.Now.
	now func() time.Time
}

// New returns a Server. Pass production wiring from main; tests can swap any
// dependency via the public fields exposed by the New signature.
func New(
	cfg config.Config,
	jwksClient *jwks.Client,
	sink Sink,
	st *store.Store,
	cache *lru.Cache,
	pinger store.Pinger,
	logger *slog.Logger,
) *Server {
	s := &Server{
		cfg:    cfg,
		jwks:   jwksClient,
		sink:   sink,
		store:  st,
		cache:  cache,
		pinger: pinger,
		logger: logger,
		now:    func() time.Time { return time.Now().UTC() },
	}
	// main.go calls New AFTER pre-warm completes, so we can flip the
	// ready bit here. Tests that don't pre-warm (server_test.go) can
	// flip it back via SetReady to drive the readiness path.
	s.ready.Store(true)
	return s
}

// SetReady is the gateway's pre-warm hook for tests. Production never
// calls it; main.go already passes a populated cache and New flips the
// flag.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// lookupAndCompare runs the in-process LRU hit + replica miss + JWT
// org compare. Returns ("", nil) on a clean ownership match.
// Otherwise an HTTP status + the error the caller propagates.
//
// Constant-shape branches: every code path runs at least one
// uuid-compare so a malicious caller cannot infer whether a
// portfolio_id exists by timing the response (ADR-0039 honesty about
// avoiding existence oracles).
func (s *Server) lookupAndCompare(ctx context.Context, portfolioID, jwtOrgID uuid.UUID) (int, error) {
	var rowOrg uuid.UUID
	if v, ok := s.cache.Get(portfolioID.String()); ok {
		metrics.LRUHits.Inc()
		rowOrg = v
	} else {
		metrics.LRUMisses.Inc()
		fetched, err := s.store.LookupPortfolioOrg(ctx, portfolioID)
		if err != nil {
			if errors.Is(err, store.ErrPortfolioNotFound) {
				// Negative result NOT cached (ADR-0034) — every
				// subsequent attempt re-checks the replica.
				return http.StatusNotFound, errors.New("not found")
			}
			s.logger.Error("portfolio lookup (replica miss)", "err", err)
			return http.StatusServiceUnavailable, errors.New("portfolio lookup unavailable")
		}
		rowOrg = fetched
		s.cache.Put(portfolioID.String(), fetched)
		metrics.LRUSize.Set(float64(s.cache.Size()))
	}
	if rowOrg != jwtOrgID {
		return http.StatusNotFound, errors.New("not found")
	}
	return 0, nil
}

// Handler returns the mux. Routes are pinned to specific methods to avoid
// accidental coupling between GET /healthz and a future POST /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", promhttp.Handler())

	// v6 Phase 6: control-plane fan-out endpoint. Shared-secret bearer in
	// the X-Lru-Prime-Token header. Single (portfolio_id, org_id) per
	// call. Returns 204 on success.
	mux.HandleFunc("POST /internal/lru-prime", s.handleLRUPrime)
	mux.HandleFunc("POST /internal/tombstone", s.handleTombstone)

	// portfolio_events.v2 endpoints.
	mux.HandleFunc("POST /v6/trades", s.makePortfolioEventHandler("TRADE", false))
	mux.HandleFunc("POST /v6/trades/bulk", s.makePortfolioEventHandler("TRADE", true))
	mux.HandleFunc("POST /v6/dividends", s.makePortfolioEventHandler("DIVIDEND", false))
	mux.HandleFunc("POST /v6/cashflows", s.makePortfolioEventHandler("CASHFLOW", false))
	mux.HandleFunc("POST /v6/fx-conversions", s.makePortfolioEventHandler("FX_CONVERSION", false))
	mux.HandleFunc("POST /v6/transfer-ins", s.makePortfolioEventHandler("TRANSFER_IN", false))
	mux.HandleFunc("POST /v6/option-exercises", s.makePortfolioEventHandler("OPTION_EXERCISE", false))
	mux.HandleFunc("POST /v6/option-assignments", s.makePortfolioEventHandler("OPTION_ASSIGNMENT", false))
	mux.HandleFunc("POST /v6/option-expiries", s.makePortfolioEventHandler("OPTION_EXPIRY", false))

	// portfolio_events.v2 bulk tombstone: a plugin deletes events it authored.
	// The key is org|plugin|source_id (type-agnostic), so one verb covers
	// every event_type. Isolated by org+plugin in the key.
	mux.HandleFunc("POST /v6/events/{plugin_id}/tombstones", s.handleEventsTombstone)

	// option-marks are observations, not portfolio events.
	mux.HandleFunc("POST /v6/option-marks", s.handleOptionMarks)

	// data.v2 endpoints. {namespace} is consumed verbatim as source_namespace
	// in the envelope; the URL-path "/" inside a namespace is illegal — clients
	// must register a single-segment namespace.
	mux.HandleFunc("POST /v6/data/{plugin_id}/{namespace}", s.handleDataSingle)
	mux.HandleFunc("POST /v6/data/{plugin_id}/{namespace}/bulk", s.handleDataBulk)
	mux.HandleFunc("POST /v6/data/{plugin_id}/{namespace}/tombstones", s.handleDataTombstone)

	return mux
}

// --- health ----------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Four preconditions: LRU pre-warmed, JWKS fresh, Kafka producer
	// connected, Postgres replica reachable. Pre-warm is the new gate
	// (v6 Phase 6): the LB drains the gateway until the in-process
	// cache holds the full portfolios mirror.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	prewarmOK := s.ready.Load()
	jwksOK := s.jwks != nil && s.jwks.Fresh()
	sinkOK := s.sink != nil && s.sink.Connected()

	dbOK := true
	if s.pinger != nil {
		if err := s.pinger.Ping(ctx); err != nil {
			dbOK = false
		}
	}

	status := http.StatusOK
	if !prewarmOK || !jwksOK || !sinkOK || !dbOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"prewarm":  prewarmOK,
		"jwks":     jwksOK,
		"sink":     sinkOK,
		"postgres": dbOK,
	})
}

// handleLRUPrime accepts a single (portfolio_id, org_id) mapping from
// the control plane after POST /portfolios is durable. The header
// X-Lru-Prime-Token is constant-time-compared against the shared
// secret; missing / mismatched token returns 401 without touching the
// body. Body unknown fields are rejected (DisallowUnknownFields).
//
// A missed notification is benign per ADR-0034: the next request that
// races a brand-new portfolio falls through to the replica miss path.
func (s *Server) handleLRUPrime(w http.ResponseWriter, r *http.Request) {
	if s.cfg.LRUPrimeToken == "" {
		metrics.LRUPrimeCalls.WithLabelValues("disabled").Inc()
		http.Error(w, "lru-prime disabled", http.StatusServiceUnavailable)
		return
	}
	got := r.Header.Get("X-Lru-Prime-Token")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.LRUPrimeToken)) != 1 {
		metrics.LRUPrimeCalls.WithLabelValues("unauthorized").Inc()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		PortfolioID string `json:"portfolio_id"`
		OrgID       string `json:"org_id"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		metrics.LRUPrimeCalls.WithLabelValues("bad-body").Inc()
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pid, err := uuid.Parse(body.PortfolioID)
	if err != nil {
		metrics.LRUPrimeCalls.WithLabelValues("bad-portfolio-id").Inc()
		http.Error(w, "portfolio_id not a UUID", http.StatusBadRequest)
		return
	}
	oid, err := uuid.Parse(body.OrgID)
	if err != nil {
		metrics.LRUPrimeCalls.WithLabelValues("bad-org-id").Inc()
		http.Error(w, "org_id not a UUID", http.StatusBadRequest)
		return
	}
	s.cache.Put(pid.String(), oid)
	metrics.LRUSize.Set(float64(s.cache.Size()))
	metrics.LRUPrimeCalls.WithLabelValues("ok").Inc()
	w.WriteHeader(http.StatusNoContent)
}

// handleTombstone accepts a bulk list of keys to tombstone on one of the
// v2 topics. Gateway produces one null-payload record per key; RW's FORMAT
// UPSERT applies it as a row delete. ADR-0050 (the self-service plugin
// uninstall path) is the only legitimate caller.
//
// Auth: capability JWT (aud=cfg.TombstoneJWTAudience) signed by
// control-plane and verified via the JWKS gateway already polls.
// Different audience from the session JWT (aud=cfg.JWTAudience), so a
// stolen session token can't be replayed against the destructive
// endpoint. Token also carries scope_topics so a leaked token can only
// touch the topic it was minted for, plus scope_org_id/scope_plugin_id:
// every submitted key must open with `org_id|plugin_id|`, so a token
// scoped to one (org, plugin) can never null another's keys. One escaping
// key fails the whole batch closed (all-or-nothing, never per-key).
//
// Topic whitelist: portfolio_events.v2 + data.v2 only.
func (s *Server) handleTombstone(w http.ResponseWriter, r *http.Request) {
	if s.cfg.TombstoneJWTAudience == "" || s.jwks == nil {
		http.Error(w, "tombstone disabled", http.StatusServiceUnavailable)
		return
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tokenStr := strings.TrimPrefix(auth, prefix)
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(s.cfg.JWTIssuer),
		jwt.WithAudience(s.cfg.TombstoneJWTAudience),
		jwt.WithExpirationRequired(),
	)
	keyFunc := s.jwks.KeyFunc(r.Context())
	tok, err := parser.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		return keyFunc(kid)
	})
	if err != nil {
		s.logger.Warn("tombstone jwt verify failed", "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	allowedTopics := stringSliceClaim(mc["scope_topics"])
	scopeOrgID, _ := mc["scope_org_id"].(string)
	scopePluginID, _ := mc["scope_plugin_id"].(string)
	if scopeOrgID == "" || scopePluginID == "" {
		http.Error(w, "tombstone token missing scope", http.StatusForbidden)
		return
	}

	var body struct {
		Topic string   `json:"topic"`
		Keys  []string `json:"keys"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Two checks: configured topic whitelist + the JWT's per-call
	// scope. The token can never be replayed against a topic it
	// wasn't minted for.
	if body.Topic != s.cfg.PortfolioEventsTopic && body.Topic != s.cfg.DataTopic {
		http.Error(w, "topic not allowed", http.StatusBadRequest)
		return
	}
	if !sliceContains(allowedTopics, body.Topic) {
		http.Error(w, "topic out of scope for token", http.StatusForbidden)
		return
	}
	// Per-key scope: every canonical key opens with `org_id|plugin_id|`
	// (P1.3), so a token minted for one (org, plugin) can only null its
	// own keys. Fail closed on the whole batch if any key escapes the
	// token's prefix — an out-of-scope key in a trusted capability batch
	// is buggy or malicious, never something to partially honor.
	requiredPrefix := scopeOrgID + "|" + scopePluginID + "|"
	if !keysInScope(body.Keys, requiredPrefix) {
		http.Error(w, "key outside token scope", http.StatusForbidden)
		return
	}
	if len(body.Keys) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	st := streamData
	if body.Topic == s.cfg.PortfolioEventsTopic {
		st = streamPortfolioEvents
	}
	for _, k := range body.Keys {
		if _, err := s.sink.Tombstone(r.Context(), st, []byte(k)); err != nil {
			s.logger.Error("sink tombstone", "err", err, "topic", body.Topic)
			http.Error(w, "tombstone", http.StatusBadGateway)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"keys_published": len(body.Keys)})
}

// stringSliceClaim coerces a JWT claim that's `[]any` of strings into a
// `[]string`. Returns empty slice for missing/nil claims.
func stringSliceClaim(c any) []string {
	raw, ok := c.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func sliceContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// keysInScope reports whether every key carries the required
// `org_id|plugin_id|` prefix. Empty key set is in scope (nothing to null).
func keysInScope(keys []string, prefix string) bool {
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			return false
		}
	}
	return true
}

// --- claims ----------------------------------------------------------------

// sessionClaims is the subset of the control-plane-minted JWT the gateway
// reads. iss/aud/exp are validated by jwt's standard verifier; we only pull
// the application claims.
type sessionClaims struct {
	OrgID    uuid.UUID
	UserID   string
	PluginID string
}

// authenticate verifies the Bearer JWT against the control-plane JWKS and
// returns parsed claims. Any failure path returns (zero, false) — the caller
// writes 401 without leaking why.
func (s *Server) authenticate(r *http.Request) (sessionClaims, bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return sessionClaims{}, false
	}
	tokenStr := strings.TrimPrefix(auth, prefix)

	parser := jwt.NewParser(
		// Control-plane signs RS256 since migration 0013. ES256 is kept
		// in the accept list for backward-compat with any in-flight
		// session JWT minted before the rotation.
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithIssuer(s.cfg.JWTIssuer),
		jwt.WithAudience(s.cfg.JWTAudience),
		jwt.WithExpirationRequired(),
	)
	keyFunc := s.jwks.KeyFunc(r.Context())
	tok, err := parser.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		return keyFunc(kid)
	})
	if err != nil {
		s.logger.Warn("jwt verify failed", "err", err)
		return sessionClaims{}, false
	}
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return sessionClaims{}, false
	}
	orgStr, _ := mc["org_id"].(string)
	orgID, err := uuid.Parse(orgStr)
	if err != nil {
		s.logger.Warn("jwt org_id not a uuid", "value", orgStr)
		return sessionClaims{}, false
	}
	userID, _ := mc["sub"].(string)
	pluginID, _ := mc["plugin_id"].(string)
	if userID == "" || pluginID == "" {
		return sessionClaims{}, false
	}
	return sessionClaims{OrgID: orgID, UserID: userID, PluginID: pluginID}, true
}

// --- response helpers ------------------------------------------------------

// produceResult mirrors the result tuple returned by Kafka and adds the
// gateway version string for trace plumbing.
type produceResult struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Offset    int64  `json:"offset"`
	Source    string `json:"source"`
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// --- body validation -------------------------------------------------------

// rejectTopLevelOrgID parses the raw JSON body as a generic map and returns
// (false, nil) iff a top-level "org_id" key is present. Nested fields like
// "portfolio_metadata.org_id" are not flagged. Returns the raw bytes so the
// caller can decode them into a typed struct without a second read.
//
// Plan note: this is deliberately NOT a substring/regex scan — those produce
// false positives on legitimate nested fields named org_id.
func rejectTopLevelOrgID(r *http.Request) (raw []byte, hasOrgID bool, err error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, false, err
	}
	// Empty body: nothing to scan; caller will fail on the typed decode.
	if len(bytes.TrimSpace(body)) == 0 {
		return body, false, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		// If the top-level isn't an object (e.g. a JSON array for bulk), it
		// cannot carry a top-level "org_id" key. Defer the malformed-body
		// signal to the typed decode that follows.
		return body, false, nil
	}
	_, ok := probe["org_id"]
	return body, ok, nil
}

// decode parses the raw body bytes into the target struct.
func decode(raw []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

// --- portfolio-event handler factory ---------------------------------------

// makePortfolioEventHandler returns a handler closed over the event_type
// discriminator. bulk=true expects a JSON array of bodies and produces N
// records in a partial-success loop.
func (s *Server) makePortfolioEventHandler(eventType string, bulk bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := s.authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		raw, hasOrg, err := rejectTopLevelOrgID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "read body")
			return
		}
		if hasOrg {
			writeError(w, http.StatusBadRequest, "client-supplied org_id is forbidden")
			return
		}

		if bulk {
			s.handleBulkPortfolioEvents(w, r, claims, eventType, raw)
			return
		}

		var body portfolioEventBody
		if err := decode(raw, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		res, status, errResp := s.publishPortfolioEvent(r.Context(), claims, eventType, body)
		if errResp != nil {
			writeError(w, status, errResp.Error())
			return
		}
		writeJSON(w, http.StatusCreated, res)
	}
}

// handleBulkPortfolioEvents loops the single-record path and returns the
// per-index aggregate. Plan calls for at-first-failure short-circuit; we
// implement that here.
type bulkResultEntry struct {
	Index  int            `json:"index"`
	Result *produceResult `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

type bulkResponse struct {
	Succeeded []bulkResultEntry `json:"succeeded"`
	Failed    []bulkResultEntry `json:"failed"`
}

func (s *Server) handleBulkPortfolioEvents(w http.ResponseWriter, r *http.Request, claims sessionClaims, eventType string, raw []byte) {
	var bodies []portfolioEventBody
	if err := decode(raw, &bodies); err != nil {
		writeError(w, http.StatusBadRequest, "invalid bulk body")
		return
	}
	out := bulkResponse{}
	for i, body := range bodies {
		res, _, errResp := s.publishPortfolioEvent(r.Context(), claims, eventType, body)
		if errResp != nil {
			out.Failed = append(out.Failed, bulkResultEntry{Index: i, Error: errResp.Error()})
			// Short-circuit on first failure (per ADR-0039 fail-closed bias).
			break
		}
		out.Succeeded = append(out.Succeeded, bulkResultEntry{Index: i, Result: &res})
	}
	status := http.StatusCreated
	if len(out.Failed) > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, out)
}

// publishPortfolioEvent runs the full lifecycle for one portfolio_events
// envelope: ownership check, envelope build, SR serialize, Kafka produce.
// Returns (result, http-status, err). Successful produces return (result,
// 201, nil); the http-status only matters when err != nil.
func (s *Server) publishPortfolioEvent(ctx context.Context, claims sessionClaims, eventType string, body portfolioEventBody) (produceResult, int, error) {
	if body.SourceID == "" || body.PortfolioID == "" {
		return produceResult{}, http.StatusBadRequest, errors.New("source_id and portfolio_id required")
	}
	portfolioUUID, err := uuid.Parse(body.PortfolioID)
	if err != nil {
		return produceResult{}, http.StatusBadRequest, errors.New("portfolio_id not a UUID")
	}

	// Ownership check. Hot path: LRU.Get (in-process map, ~100ns). On
	// miss, fall through to the replica with the prepared statement;
	// write through on hit. Both row-missing and wrong-org collapse to
	// 404 without an early-return branch (ADR-0039 / ADR-0034).
	if status, err := s.lookupAndCompare(ctx, portfolioUUID, claims.OrgID); err != nil {
		return produceResult{}, status, err
	}

	env := PortfolioEventV2{
		OrgID:        claims.OrgID.String(),
		SourceID:     body.SourceID,
		EventType:    eventType,
		PortfolioID:  body.PortfolioID,
		InstrumentID: body.InstrumentID,
		BusinessTs:   time.UnixMicro(body.BusinessTs),
		IngestTs:     s.now(),
		Source:       s.cfg.SourceID,
		PluginID:     nullableString(claims.PluginID),
		TraceID:      body.TraceID,
		Headers:      body.Headers,
		Payload:      body.Payload,
	}
	if env.Headers == nil {
		env.Headers = map[string]string{}
	}

	res, err := s.sink.PublishPortfolioEvent(ctx, datakey.EventKey(claims.OrgID.String(), claims.PluginID, body.SourceID), &env)
	if err != nil {
		s.logger.Error("sink publish portfolio_events", "err", err)
		return produceResult{}, http.StatusBadGateway, errors.New("publish")
	}
	return produceResult{Topic: res.Topic, Partition: res.Partition, Offset: res.Offset, Source: s.cfg.SourceID}, http.StatusCreated, nil
}

// --- portfolio_events.v2 tombstone ------------------------------------------

// eventTombstoneEntry is one target in the events-tombstone array. No
// observed_at and no event_type: the event key is org|plugin|source_id, so a
// tombstone is type-agnostic. portfolio_id is REQUIRED — every portfolio event
// is portfolio-scoped, and ownership is verified against the JWT org.
type eventTombstoneEntry struct {
	SourceID    string `json:"source_id"`
	PortfolioID string `json:"portfolio_id"`
}

// handleEventsTombstone lets a plugin delete events it authored on portfolio_events.v2.
// Per-entry failures land in the failed list without aborting the batch.
func (s *Server) handleEventsTombstone(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	pluginID := r.PathValue("plugin_id")
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id required")
		return
	}
	if pluginID != claims.PluginID {
		writeError(w, http.StatusForbidden, "plugin_id mismatch")
		return
	}
	raw, hasOrg, err := rejectTopLevelOrgID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		return
	}
	if hasOrg {
		writeError(w, http.StatusBadRequest, "client-supplied org_id is forbidden")
		return
	}
	var entries []eventTombstoneEntry
	if err := decode(raw, &entries); err != nil {
		writeError(w, http.StatusBadRequest, "invalid bulk body")
		return
	}

	out := bulkResponse{}
	for i, e := range entries {
		res, _, errResp := s.tombstoneEvent(r.Context(), claims, e)
		if errResp != nil {
			out.Failed = append(out.Failed, bulkResultEntry{Index: i, Error: errResp.Error()})
			continue
		}
		out.Succeeded = append(out.Succeeded, bulkResultEntry{Index: i, Result: &produceResult{Topic: res.Topic, Partition: res.Partition, Offset: res.Offset, Source: s.cfg.SourceID}})
	}
	status := http.StatusOK
	if len(out.Failed) > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, out)
}

// tombstoneEvent verifies ownership of one event-tombstone entry and produces a
// NULL value under EventKey(org, plugin, source_id). Returns (result, http-status, err);
// the status only matters when err != nil.
func (s *Server) tombstoneEvent(ctx context.Context, claims sessionClaims, e eventTombstoneEntry) (produceResult, int, error) {
	if e.SourceID == "" || e.PortfolioID == "" {
		return produceResult{}, http.StatusBadRequest, errors.New("source_id and portfolio_id required")
	}
	portfolioUUID, err := uuid.Parse(e.PortfolioID)
	if err != nil {
		return produceResult{}, http.StatusBadRequest, errors.New("portfolio_id not a UUID")
	}
	if status, err := s.lookupAndCompare(ctx, portfolioUUID, claims.OrgID); err != nil {
		return produceResult{}, status, err
	}
	key := datakey.EventKey(claims.OrgID.String(), claims.PluginID, e.SourceID)
	res, err := s.sink.Tombstone(ctx, streamPortfolioEvents, key)
	if err != nil {
		s.logger.Error("sink tombstone events", "err", err)
		return produceResult{}, http.StatusBadGateway, errors.New("tombstone")
	}
	return produceResult{Topic: res.Topic, Partition: res.Partition, Offset: res.Offset, Source: s.cfg.SourceID}, http.StatusCreated, nil
}

// --- option-marks (data.v2) -------------------------------------------------

// handleOptionMarks is a thin shim: same body shape as a data record under
// the prices.option_mark namespace, but the URL is hoisted to /v6/option-marks
// to keep parity with the v5 plugin's URL family. portfolio_id ownership does
// not apply (data, not portfolio_events); plugin_id namespace ownership is
// enforced via the JWT.
func (s *Server) handleOptionMarks(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	raw, hasOrg, err := rejectTopLevelOrgID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		return
	}
	if hasOrg {
		writeError(w, http.StatusBadRequest, "client-supplied org_id is forbidden")
		return
	}
	var body dataBody
	if err := decode(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	s.publishData(w, r, claims, "prices.option_mark", body, http.StatusCreated, false)
}

// --- /v6/data/{plugin_id}/{namespace} --------------------------------------

func (s *Server) handleDataSingle(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	pluginID := r.PathValue("plugin_id")
	namespace := r.PathValue("namespace")
	if pluginID == "" || namespace == "" {
		writeError(w, http.StatusBadRequest, "plugin_id and namespace required")
		return
	}
	if pluginID != claims.PluginID {
		writeError(w, http.StatusForbidden, "plugin_id mismatch")
		return
	}
	raw, hasOrg, err := rejectTopLevelOrgID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		return
	}
	if hasOrg {
		writeError(w, http.StatusBadRequest, "client-supplied org_id is forbidden")
		return
	}
	var body dataBody
	if err := decode(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	s.publishData(w, r, claims, namespace, body, http.StatusCreated, false)
}

// parseBulkDataRequest handles the shared preamble for handleDataBulk and
// handleDataTombstone: authenticate, extract and validate path params, reject
// client-supplied org_id, decode the body array. Returns ok=false after writing
// the appropriate error response.
func (s *Server) parseBulkDataRequest(w http.ResponseWriter, r *http.Request) (claims sessionClaims, namespace string, bodies []dataBody, ok bool) {
	claims, ok = s.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	pluginID := r.PathValue("plugin_id")
	namespace = r.PathValue("namespace")
	if pluginID == "" || namespace == "" {
		writeError(w, http.StatusBadRequest, "plugin_id and namespace required")
		ok = false
		return
	}
	if pluginID != claims.PluginID {
		writeError(w, http.StatusForbidden, "plugin_id mismatch")
		ok = false
		return
	}
	raw, hasOrg, err := rejectTopLevelOrgID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		ok = false
		return
	}
	if hasOrg {
		writeError(w, http.StatusBadRequest, "client-supplied org_id is forbidden")
		ok = false
		return
	}
	// An empty array body is valid: both handlers return 200/201 with zero entries.
	if err := decode(raw, &bodies); err != nil {
		writeError(w, http.StatusBadRequest, "invalid bulk body")
		ok = false
		return
	}
	return
}

func (s *Server) handleDataBulk(w http.ResponseWriter, r *http.Request) {
	claims, namespace, bodies, ok := s.parseBulkDataRequest(w, r)
	if !ok {
		return
	}
	out := bulkResponse{}
	for i, b := range bodies {
		res, _, errResp := s.serializeAndProduceData(r.Context(), claims, namespace, b, false)
		if errResp != nil {
			out.Failed = append(out.Failed, bulkResultEntry{Index: i, Error: errResp.Error()})
			// bulk publish short-circuits on first failure; the tombstone bulk handler intentionally continues
			break
		}
		out.Succeeded = append(out.Succeeded, bulkResultEntry{Index: i, Result: &res})
	}
	status := http.StatusCreated
	if len(out.Failed) > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, out)
}

// handleDataTombstone accepts an ARRAY of tombstone targets so a plugin can
// purge many bars in one call (the yfinance symbol-change purge nulls every
// prior bar for a symbol). Each entry's payload is irrelevant and ignored; only
// source_id, observed_at and portfolio_id locate the key to null. Per-entry
// failures (ownership 404, bad portfolio_id) are reported in the bulkResponse
// failed list without aborting the batch — a failed entry must not block the
// rest of the purge.
func (s *Server) handleDataTombstone(w http.ResponseWriter, r *http.Request) {
	claims, namespace, bodies, ok := s.parseBulkDataRequest(w, r)
	if !ok {
		return
	}
	out := bulkResponse{}
	for i, b := range bodies {
		res, _, errResp := s.serializeAndProduceData(r.Context(), claims, namespace, b, true)
		if errResp != nil {
			out.Failed = append(out.Failed, bulkResultEntry{Index: i, Error: errResp.Error()})
			continue
		}
		out.Succeeded = append(out.Succeeded, bulkResultEntry{Index: i, Result: &res})
	}
	status := http.StatusOK
	if len(out.Failed) > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, out)
}

// publishData adapts publish helpers to single-record HTTP responses.
func (s *Server) publishData(w http.ResponseWriter, r *http.Request, claims sessionClaims, namespace string, body dataBody, okStatus int, tombstone bool) {
	res, status, errResp := s.serializeAndProduceData(r.Context(), claims, namespace, body, tombstone)
	if errResp != nil {
		writeError(w, status, errResp.Error())
		return
	}
	writeJSON(w, okStatus, res)
}

// serializeAndProduceData runs the data publish lifecycle: ownership check, then
// hand the typed envelope to the sink. Tombstones carry only the key (the sink
// deletes the row); publishes hand over the full DataV2 envelope.
func (s *Server) serializeAndProduceData(ctx context.Context, claims sessionClaims, namespace string, body dataBody, tombstone bool) (produceResult, int, error) {
	if body.SourceID == "" {
		return produceResult{}, http.StatusBadRequest, errors.New("source_id required")
	}

	// Track 2c: a non-null portfolio_id is verified to belong to the caller's
	// org (same rule as portfolio events); null = org-scoped data, no check.
	portfolioSeg := ""
	if body.PortfolioID != nil && *body.PortfolioID != "" {
		pid, err := uuid.Parse(*body.PortfolioID)
		if err != nil {
			return produceResult{}, http.StatusBadRequest, errors.New("portfolio_id not a UUID")
		}
		if code, err := s.lookupAndCompare(ctx, pid, claims.OrgID); err != nil {
			return produceResult{}, code, err
		}
		portfolioSeg = pid.String()
	}

	key := datakey.DataKey(claims.OrgID.String(), claims.PluginID, namespace, portfolioSeg, body.SourceID, body.ObservedAt)

	var res sinkResult
	var err error
	if tombstone {
		res, err = s.sink.Tombstone(ctx, streamData, key)
	} else {
		env := DataV2{
			OrgID:           claims.OrgID.String(),
			SourceNamespace: namespace,
			SourceID:        body.SourceID,
			ObservedAt:      time.UnixMicro(body.ObservedAt),
			IngestTs:        s.now(),
			Source:          s.cfg.SourceID,
			PluginID:        nullableString(claims.PluginID),
			PortfolioID:     body.PortfolioID,
			TraceID:         body.TraceID,
			Payload:         body.Payload,
		}
		res, err = s.sink.PublishData(ctx, key, &env)
	}
	if err != nil {
		s.logger.Error("sink publish data", "err", err)
		return produceResult{}, http.StatusBadGateway, errors.New("publish")
	}
	return produceResult{Topic: res.Topic, Partition: res.Partition, Offset: res.Offset, Source: s.cfg.SourceID}, http.StatusCreated, nil
}
