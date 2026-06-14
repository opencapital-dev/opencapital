package httpapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/portfolio-management/datakey"
	"github.com/portfolio-management/gateway/internal/config"
	"github.com/portfolio-management/gateway/internal/jwks"
	gwkafka "github.com/portfolio-management/gateway/internal/kafka"
	"github.com/portfolio-management/gateway/internal/lru"
	"github.com/portfolio-management/gateway/internal/store"
)

// ---- helpers --------------------------------------------------------------

type adapter struct{ pool pgxmock.PgxPoolIface }

func (a adapter) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return a.pool.QueryRow(ctx, sql, args...)
}

// fakeProducer is a capturing test double for the producer interface. Every
// Produce call is recorded verbatim — a tombstone's nil value is kept nil, not
// coerced to an empty slice — so handler tests can assert the exact key/value
// bytes a publish emits. captures is guarded by mu because, although the
// httptest handler runs synchronously, the producer interface is free to be
// called from multiple goroutines in bulk paths.
type fakeProducer struct {
	mu       sync.Mutex
	captures []producedRecord
}

type producedRecord struct {
	Topic string
	Key   []byte
	Value []byte
}

func (f *fakeProducer) Produce(_ context.Context, topic string, key, value []byte) (gwkafka.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captures = append(f.captures, producedRecord{Topic: topic, Key: key, Value: value})
	return gwkafka.Result{Topic: topic, Partition: 0, Offset: int64(len(f.captures) - 1)}, nil
}

func (f *fakeProducer) Connected() bool { return true }

// fakeSerializer returns fixed non-nil bytes so the produced value is non-nil
// while tests assert the produced key.
type fakeSerializer struct{}

func (fakeSerializer) PortfolioEvents(_ string, _ any) ([]byte, error) { return []byte("avro"), nil }
func (fakeSerializer) Data(_ string, _ any) ([]byte, error)            { return []byte("avro"), nil }

// produced returns a copy of the captured records, safe to read after the
// handler has run.
func (f *fakeProducer) produced() []producedRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]producedRecord, len(f.captures))
	copy(out, f.captures)
	return out
}

// signKey is a P-256 key whose public half the test JWKS server returns.
type testSigner struct {
	priv    *ecdsa.PrivateKey
	kid     string
	rsaPriv *rsa.PrivateKey
	rsaKid  string
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	// RSA key for the tombstone capability path (control-plane signs
	// those RS256; handleTombstone only accepts RS256).
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa key: %v", err)
	}
	return &testSigner{priv: k, kid: "test-kid", rsaPriv: rk, rsaKid: "test-rsa-kid"}
}

// jwksJSON returns the JWKS doc the JWKS server publishes: the EC key for
// session JWTs and the RSA key for tombstone capability JWTs.
func (s *testSigner) jwksJSON() string {
	// Use the raw x/y from the public key. Each is 32 bytes for P-256.
	xb := s.priv.PublicKey.X.Bytes()
	yb := s.priv.PublicKey.Y.Bytes()
	pad := func(b []byte) []byte {
		if len(b) == 32 {
			return b
		}
		out := make([]byte, 32)
		copy(out[32-len(b):], b)
		return out
	}
	ecKey := fmt.Sprintf(`{"kty":"EC","crv":"P-256","kid":%q,"use":"sig","alg":"ES256","x":%q,"y":%q}`,
		s.kid,
		b64url(pad(xb)),
		b64url(pad(yb)),
	)
	pub := s.rsaPriv.PublicKey
	nB64 := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	eB64 := base64.RawURLEncoding.EncodeToString(eBytes)
	rsaKey := fmt.Sprintf(`{"kty":"RSA","kid":%q,"use":"sig","alg":"RS256","n":%q,"e":%q}`,
		s.rsaKid, nB64, eB64,
	)
	return fmt.Sprintf(`{"keys":[%s,%s]}`, ecKey, rsaKey)
}

func b64url(b []byte) string {
	// base64.RawURLEncoding inline (avoid extra import line):
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		end := i + 3
		if end > len(b) {
			end = len(b)
		}
		chunk := b[i:end]
		v := uint32(0)
		for _, c := range chunk {
			v = v<<8 | uint32(c)
		}
		v <<= uint32((3 - len(chunk)) * 8)
		out.WriteByte(enc[(v>>18)&0x3f])
		out.WriteByte(enc[(v>>12)&0x3f])
		if len(chunk) >= 2 {
			out.WriteByte(enc[(v>>6)&0x3f])
		}
		if len(chunk) == 3 {
			out.WriteByte(enc[v&0x3f])
		}
	}
	return out.String()
}

// makeToken signs a session JWT with the supplied claims.
func (s *testSigner) makeToken(t *testing.T, orgID uuid.UUID, userID, pluginID string, lifetime time.Duration) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":       "control-plane",
		"aud":       "gateway",
		"sub":       userID,
		"org_id":    orgID.String(),
		"plugin_id": pluginID,
		"iat":       time.Now().Unix(),
		"exp":       time.Now().Add(lifetime).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// makeTombstoneToken mints the RS256 capability JWT the uninstall purge path
// expects: aud=TombstoneJWTAudience plus the scope_* claims control-plane's
// signTombstoneScope emits. Pass empty scopeOrgID/scopePluginID to exercise
// the missing-scope rejection.
func (s *testSigner) makeTombstoneToken(t *testing.T, scopeOrgID, scopePluginID string, topics []string, lifetime time.Duration) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":          "control-plane",
		"aud":          "gateway-tombstone",
		"sub":          "uninstall:" + scopeOrgID + ":" + scopePluginID,
		"scope_topics": topics,
		"iat":          time.Now().Unix(),
		"exp":          time.Now().Add(lifetime).Unix(),
	}
	if scopeOrgID != "" {
		claims["scope_org_id"] = scopeOrgID
	}
	if scopePluginID != "" {
		claims["scope_plugin_id"] = scopePluginID
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = s.rsaKid
	signed, err := tok.SignedString(s.rsaPriv)
	if err != nil {
		t.Fatalf("sign tombstone: %v", err)
	}
	return signed
}

// newServer wires up a Server with an in-memory JWKS endpoint, pgxmock store,
// fakeProducer, and fakeSerializer. The returned *fakeProducer lets callers
// inspect produced records after the handler runs.
func newServer(t *testing.T) (*Server, *testSigner, pgxmock.PgxPoolIface, *fakeProducer, func()) {
	t.Helper()
	signer := newTestSigner(t)
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, signer.jwksJSON())
	}))

	cfg := config.Config{
		ListenAddr:           ":0",
		ControlPlaneURL:      jwksSrv.URL,
		ControlDBReplicaDSN:  "stub",
		LRUPrimeToken:        "test-lru-token",
		KafkaBrokers:         "stub",
		KafkaSASLUsername:    "gateway",
		KafkaSASLPassword:    "stub",
		KafkaSASLMechanism:   "SCRAM-SHA-256",
		SchemaRegistryURL:    "stub",
		SRBasicAuthUser:      "sr-gateway",
		SRBasicAuthPass:      "stub",
		PortfolioEventsTopic: "portfolio_events.v2",
		DataTopic:            "data.v2",
		JWTIssuer:            "control-plane",
		JWTAudience:          "gateway",
		TombstoneJWTAudience: "gateway-tombstone",
		JWKSRefresh:          5 * time.Minute,
		SourceID:             "gateway@test",
	}

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	st := store.New(adapter{pool: mock})
	cache := lru.New()

	// JWKS client pointed at the in-memory server. Warm before use.
	jc := jwks.New(jwksSrv.URL)
	if err := jc.Refresh(context.Background()); err != nil {
		t.Fatalf("jwks refresh: %v", err)
	}

	fp := &fakeProducer{}
	sink := NewKafkaSink(fakeSerializer{}, fp, cfg.PortfolioEventsTopic, cfg.DataTopic)
	s := New(cfg, jc, sink, st, cache, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cleanup := func() {
		jwksSrv.Close()
		mock.Close()
	}
	return s, signer, mock, fp, cleanup
}

// ---- tests ---------------------------------------------------------------

func TestAuth_MissingBearer(t *testing.T) {
	s, _, _, _, cleanup := newServer(t)
	defer cleanup()

	req := httptest.NewRequest("POST", "/v6/trades", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuth_BadSignature(t *testing.T) {
	s, _, _, _, cleanup := newServer(t)
	defer cleanup()

	// A token signed by a *different* signer — not in our test JWKS.
	other := newTestSigner(t)
	tok := other.makeToken(t, uuid.New(), "u", "plugin-a", time.Hour)
	req := httptest.NewRequest("POST", "/v6/trades", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAuth_Expired(t *testing.T) {
	s, signer, _, _, cleanup := newServer(t)
	defer cleanup()

	tok := signer.makeToken(t, uuid.New(), "u", "plugin-a", -1*time.Minute)
	req := httptest.NewRequest("POST", "/v6/trades", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestClientSuppliedOrgID_Rejected(t *testing.T) {
	s, signer, _, _, cleanup := newServer(t)
	defer cleanup()

	tok := signer.makeToken(t, uuid.New(), "u", "plugin-a", time.Hour)
	body := `{"org_id":"00000000-0000-0000-0000-000000000000","portfolio_id":"00000000-0000-0000-0000-000000000001","source_id":"s","business_ts":1,"payload":"{}"}`
	req := httptest.NewRequest("POST", "/v6/trades", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if !strings.Contains(resp["error"], "org_id") {
		t.Fatalf("expected org_id error message, got %q", resp["error"])
	}
}

// TestClientSuppliedOrgID_NestedAllowed proves the rejection is a top-level
// JSON key check, not a substring scan: a payload that *embeds* "org_id" as a
// string inside another field must NOT be flagged.
func TestClientSuppliedOrgID_NestedAllowed(t *testing.T) {
	s, signer, mock, _, cleanup := newServer(t)
	defer cleanup()

	org := uuid.New()
	portfolio := uuid.New()
	tok := signer.makeToken(t, org, "u", "plugin-a", time.Hour)

	_ = org // we want to make sure the nested-org_id string is NOT flagged at
	// the body-validation layer, so set up the store to return "not found".
	// The handler then exits at the ownership check with 404, NOT at the
	// 400 client-supplied-org_id check.
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(portfolio).
		WillReturnError(pgx.ErrNoRows)

	body := fmt.Sprintf(`{"source_id":"s","portfolio_id":%q,"business_ts":1,"payload":"{\"note\":\"includes org_id key in payload string\"}"}`, portfolio.String())
	req := httptest.NewRequest("POST", "/v6/trades", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusBadRequest && strings.Contains(rr.Body.String(), "org_id is forbidden") {
		t.Fatalf("nested org_id substring incorrectly rejected: %s", rr.Body.String())
	}
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 from ownership check, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOwnershipCheck_404_Missing(t *testing.T) {
	s, signer, mock, _, cleanup := newServer(t)
	defer cleanup()

	org := uuid.New()
	portfolio := uuid.New()
	tok := signer.makeToken(t, org, "u", "plugin-a", time.Hour)
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(portfolio).
		WillReturnError(pgx.ErrNoRows)

	body := fmt.Sprintf(`{"source_id":"s","portfolio_id":%q,"business_ts":1,"payload":"{}"}`, portfolio.String())
	req := httptest.NewRequest("POST", "/v6/trades", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOwnershipCheck_404_WrongOrg(t *testing.T) {
	s, signer, mock, _, cleanup := newServer(t)
	defer cleanup()

	jwtOrg := uuid.New()
	rowOrg := uuid.New()
	portfolio := uuid.New()
	tok := signer.makeToken(t, jwtOrg, "u", "plugin-a", time.Hour)
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(portfolio).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(rowOrg))

	body := fmt.Sprintf(`{"source_id":"s","portfolio_id":%q,"business_ts":1,"payload":"{}"}`, portfolio.String())
	req := httptest.NewRequest("POST", "/v6/trades", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	s, _, _, _, cleanup := newServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// TestProduceSeam_CapturesDataTombstone is a throwaway proof that the capturing
// fake observes a real produce. The data-tombstone path is chosen because it
// reaches s.producer.Produce WITHOUT first calling s.ser.Data (tombstones omit
// the value), so the nil serializer in newServer is fine. It also exercises the
// nil-value contract the fake must preserve faithfully.
//
// Later tasks (canonical org_id|plugin_id key) will assert produced()[0].Key.
func TestProduceSeam_CapturesDataTombstone(t *testing.T) {
	s, signer, _, fake, cleanup := newServer(t)
	defer cleanup()

	const pluginID = "plugin-a"
	tok := signer.makeToken(t, uuid.New(), "u", pluginID, time.Hour)

	// portfolio_id omitted => org-scoped data, no ownership DB query.
	// The tombstone endpoint takes an array; a single-element array exercises
	// the produce seam.
	body := `[{"source_id":"s","observed_at":1,"payload":"{}"}]`
	req := httptest.NewRequest("POST", "/v6/data/"+pluginID+"/prices.spot/tombstones", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	got := fake.produced()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 produce, got %d", len(got))
	}
	if got[0].Topic != s.cfg.DataTopic {
		t.Fatalf("expected topic %q, got %q", s.cfg.DataTopic, got[0].Topic)
	}
	if got[0].Value != nil {
		t.Fatalf("tombstone must produce a nil value, got %q", got[0].Value)
	}
}

// TestDataKey_OrgScoped asserts the canonical org_id|plugin_id-prefixed data key
// for org-scoped data (portfolio_id omitted => empty portfolio segment).
func TestDataKey_OrgScoped(t *testing.T) {
	s, signer, _, fake, cleanup := newServer(t)
	defer cleanup()

	org := uuid.New()
	const pluginID = "plugin-a"
	tok := signer.makeToken(t, org, "u", pluginID, time.Hour)

	body := `[{"source_id":"GKP","observed_at":1700,"payload":"{}"}]`
	req := httptest.NewRequest("POST", "/v6/data/"+pluginID+"/prices.ohlcv/tombstones", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	got := fake.produced()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 produce, got %d", len(got))
	}
	want := datakey.DataKey(org.String(), pluginID, "prices.ohlcv", "", "GKP", 1700)
	if !bytes.Equal(got[0].Key, want) {
		t.Fatalf("data key = %q, want %q", got[0].Key, want)
	}
}

// TestDataKey_PortfolioScoped asserts the canonical key carries the portfolio
// UUID segment when portfolio_id is supplied and owned by the caller's org.
func TestDataKey_PortfolioScoped(t *testing.T) {
	s, signer, mock, fake, cleanup := newServer(t)
	defer cleanup()

	org := uuid.New()
	portfolio := uuid.New()
	const pluginID = "plugin-a"
	tok := signer.makeToken(t, org, "u", pluginID, time.Hour)

	// Ownership check: portfolio belongs to the JWT's org.
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(portfolio).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))

	body := fmt.Sprintf(`[{"source_id":"GKP","observed_at":1700,"portfolio_id":%q,"payload":"{}"}]`, portfolio.String())
	req := httptest.NewRequest("POST", "/v6/data/"+pluginID+"/prices.ohlcv/tombstones", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	got := fake.produced()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 produce, got %d", len(got))
	}
	want := datakey.DataKey(org.String(), pluginID, "prices.ohlcv", portfolio.String(), "GKP", 1700)
	if !bytes.Equal(got[0].Key, want) {
		t.Fatalf("data key = %q, want %q", got[0].Key, want)
	}
}

// TestEventKey_PortfolioEvent drives a portfolio-event publish (POST /v6/trades)
// through fakeSerializer and asserts the produced key is the canonical EventKey.
// The data-key tests use the tombstone path (the tombstone path skips serialization);
// this test covers the full serialize-then-produce lifecycle.
func TestEventKey_PortfolioEvent(t *testing.T) {
	s, signer, mock, fake, cleanup := newServer(t)
	defer cleanup()

	org := uuid.New()
	portfolio := uuid.New()
	const pluginID = "plugin-a"
	const sourceID = "trade-42" // == EventKey source segment
	tok := signer.makeToken(t, org, "u", pluginID, time.Hour)

	// Ownership check approves: portfolio belongs to the JWT's org.
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(portfolio).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))

	body := fmt.Sprintf(`{"source_id":%q,"portfolio_id":%q,"business_ts":1,"payload":"{}"}`, sourceID, portfolio.String())
	req := httptest.NewRequest("POST", "/v6/trades", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	got := fake.produced()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 produce, got %d", len(got))
	}
	if got[0].Topic != s.cfg.PortfolioEventsTopic {
		t.Fatalf("expected topic %q, got %q", s.cfg.PortfolioEventsTopic, got[0].Topic)
	}
	want := datakey.EventKey(org.String(), pluginID, sourceID)
	if !bytes.Equal(got[0].Key, want) {
		t.Fatalf("event key = %q, want %q", got[0].Key, want)
	}
	if got[0].Value == nil {
		t.Fatalf("portfolio-event publish must carry a non-nil serialized value")
	}
}

// bulkResp decodes the succeeded/failed split a bulk handler writes.
type bulkResp struct {
	Succeeded []struct {
		Index  int `json:"index"`
		Result *struct {
			Topic     string `json:"topic"`
			Partition int32  `json:"partition"`
			Offset    int64  `json:"offset"`
			Source    string `json:"source"`
		} `json:"result,omitempty"`
	} `json:"succeeded"`
	Failed []struct {
		Index int    `json:"index"`
		Error string `json:"error"`
	} `json:"failed"`
}

// TestDataTombstoneBulk_AllSucceed posts an array of N portfolio-scoped
// tombstones whose portfolios are all owned by the caller's org. It asserts the
// fakeProducer captured exactly N nil-value produces with the canonical per-
// entry key, and the response is 200 with N succeeded / 0 failed.
func TestDataTombstoneBulk_AllSucceed(t *testing.T) {
	s, signer, mock, fake, cleanup := newServer(t)
	defer cleanup()

	org := uuid.New()
	const pluginID = "plugin-a"
	tok := signer.makeToken(t, org, "u", pluginID, time.Hour)

	pA := uuid.New()
	pB := uuid.New()
	// Two ownership checks, both approve (portfolio belongs to the JWT org).
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(pA).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(pB).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))

	body := fmt.Sprintf(`[
		{"source_id":"AAPL","observed_at":100,"portfolio_id":%q,"payload":"{}"},
		{"source_id":"MSFT","observed_at":200,"portfolio_id":%q,"payload":"{}"}
	]`, pA.String(), pB.String())
	req := httptest.NewRequest("POST", "/v6/data/"+pluginID+"/prices.ohlcv/tombstones", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp bulkResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if len(resp.Succeeded) != 2 || len(resp.Failed) != 0 {
		t.Fatalf("expected 2 succeeded / 0 failed, got %d / %d (%s)", len(resp.Succeeded), len(resp.Failed), rr.Body.String())
	}

	got := fake.produced()
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 produces, got %d", len(got))
	}
	wantKeys := [][]byte{
		datakey.DataKey(org.String(), pluginID, "prices.ohlcv", pA.String(), "AAPL", 100),
		datakey.DataKey(org.String(), pluginID, "prices.ohlcv", pB.String(), "MSFT", 200),
	}
	for i, rec := range got {
		if rec.Value != nil {
			t.Fatalf("entry %d: tombstone must produce a nil value, got %q", i, rec.Value)
		}
		if !bytes.Equal(rec.Key, wantKeys[i]) {
			t.Fatalf("entry %d key = %q, want %q", i, rec.Key, wantKeys[i])
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestDataTombstoneBulk_PartialFailure mixes an owned-portfolio entry, an entry
// whose ownership check returns a different org (404), and another owned entry.
// The middle entry must land in the failed list and produce NO record, while the
// two owned entries still succeed. Asserts the 207 multi-status split and that
// the produce count excludes the failed entry.
func TestDataTombstoneBulk_PartialFailure(t *testing.T) {
	s, signer, mock, fake, cleanup := newServer(t)
	defer cleanup()

	org := uuid.New()
	otherOrg := uuid.New()
	const pluginID = "plugin-a"
	tok := signer.makeToken(t, org, "u", pluginID, time.Hour)

	pOwned1 := uuid.New()
	pForeign := uuid.New()
	pOwned2 := uuid.New()
	// Entry 0 approves, entry 1 returns a foreign org (collapses to 404),
	// entry 2 approves. The batch must not abort on entry 1.
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(pOwned1).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(pForeign).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(otherOrg))
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(pOwned2).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))

	body := fmt.Sprintf(`[
		{"source_id":"AAPL","observed_at":100,"portfolio_id":%q,"payload":"{}"},
		{"source_id":"MSFT","observed_at":200,"portfolio_id":%q,"payload":"{}"},
		{"source_id":"GOOG","observed_at":300,"portfolio_id":%q,"payload":"{}"}
	]`, pOwned1.String(), pForeign.String(), pOwned2.String())
	req := httptest.NewRequest("POST", "/v6/data/"+pluginID+"/prices.ohlcv/tombstones", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("expected 207, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp bulkResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if len(resp.Succeeded) != 2 || len(resp.Failed) != 1 {
		t.Fatalf("expected 2 succeeded / 1 failed, got %d / %d (%s)", len(resp.Succeeded), len(resp.Failed), rr.Body.String())
	}
	if resp.Failed[0].Index != 1 {
		t.Fatalf("expected failed index 1, got %d", resp.Failed[0].Index)
	}
	gotSucceeded := []int{resp.Succeeded[0].Index, resp.Succeeded[1].Index}
	if gotSucceeded[0] != 0 || gotSucceeded[1] != 2 {
		t.Fatalf("expected succeeded indices [0 2], got %v", gotSucceeded)
	}

	got := fake.produced()
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 produces (failed entry excluded), got %d", len(got))
	}
	// The two produces are the owned entries' keys; the foreign one is absent.
	wantKeys := [][]byte{
		datakey.DataKey(org.String(), pluginID, "prices.ohlcv", pOwned1.String(), "AAPL", 100),
		datakey.DataKey(org.String(), pluginID, "prices.ohlcv", pOwned2.String(), "GOOG", 300),
	}
	for i, rec := range got {
		if rec.Value != nil {
			t.Fatalf("entry %d: tombstone must produce a nil value, got %q", i, rec.Value)
		}
		if !bytes.Equal(rec.Key, wantKeys[i]) {
			t.Fatalf("entry %d key = %q, want %q", i, rec.Key, wantKeys[i])
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestDataTombstoneBulk_PluginMismatch keeps the handler-level 403 guard: a path
// plugin_id that differs from the JWT plugin_id is rejected before any produce.
func TestDataTombstoneBulk_PluginMismatch(t *testing.T) {
	s, signer, _, fake, cleanup := newServer(t)
	defer cleanup()

	tok := signer.makeToken(t, uuid.New(), "u", "plugin-a", time.Hour)
	body := `[{"source_id":"AAPL","observed_at":100,"payload":"{}"}]`
	req := httptest.NewRequest("POST", "/v6/data/plugin-b/prices.ohlcv/tombstones", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(fake.produced()) != 0 {
		t.Fatalf("plugin mismatch must produce nothing, got %d", len(fake.produced()))
	}
}

// TestEventsTombstoneBulk_AllSucceed posts an array of N event tombstones whose
// portfolios are all owned by the caller's org. Asserts N nil-value produces on
// the portfolio-events topic, each under the canonical EventKey(org,plugin,
// source), and a 200 with N succeeded / 0 failed.
func TestEventsTombstoneBulk_AllSucceed(t *testing.T) {
	s, signer, mock, fake, cleanup := newServer(t)
	defer cleanup()

	org := uuid.New()
	const pluginID = "plugin-a"
	tok := signer.makeToken(t, org, "u", pluginID, time.Hour)

	pA := uuid.New()
	pB := uuid.New()
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(pA).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(pB).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))

	body := fmt.Sprintf(`[
		{"source_id":"trade-1","portfolio_id":%q},
		{"source_id":"trade-2","portfolio_id":%q}
	]`, pA.String(), pB.String())
	req := httptest.NewRequest("POST", "/v6/events/"+pluginID+"/tombstones", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp bulkResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if len(resp.Succeeded) != 2 || len(resp.Failed) != 0 {
		t.Fatalf("expected 2 succeeded / 0 failed, got %d / %d (%s)", len(resp.Succeeded), len(resp.Failed), rr.Body.String())
	}

	got := fake.produced()
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 produces, got %d", len(got))
	}
	wantKeys := [][]byte{
		datakey.EventKey(org.String(), pluginID, "trade-1"),
		datakey.EventKey(org.String(), pluginID, "trade-2"),
	}
	for i, rec := range got {
		if rec.Topic != s.cfg.PortfolioEventsTopic {
			t.Fatalf("entry %d: expected topic %q, got %q", i, s.cfg.PortfolioEventsTopic, rec.Topic)
		}
		if rec.Value != nil {
			t.Fatalf("entry %d: tombstone must produce a nil value, got %q", i, rec.Value)
		}
		if !bytes.Equal(rec.Key, wantKeys[i]) {
			t.Fatalf("entry %d key = %q, want %q", i, rec.Key, wantKeys[i])
		}
	}
	// Verify the response body's Result carries the real topic (not zero value).
	for i, entry := range resp.Succeeded {
		if entry.Result == nil {
			t.Fatalf("succeeded[%d].Result is nil", i)
		}
		if entry.Result.Topic != s.cfg.PortfolioEventsTopic {
			t.Fatalf("succeeded[%d].Result.Topic = %q, want %q", i, entry.Result.Topic, s.cfg.PortfolioEventsTopic)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestEventsTombstoneBulk_PartialFailure mixes an owned-portfolio entry, an
// entry whose ownership returns a foreign org (404), and another owned entry.
// The foreign entry lands in the failed list and produces nothing; the two
// owned entries still succeed. Asserts the 207 split and excluded produce.
func TestEventsTombstoneBulk_PartialFailure(t *testing.T) {
	s, signer, mock, fake, cleanup := newServer(t)
	defer cleanup()

	org := uuid.New()
	otherOrg := uuid.New()
	const pluginID = "plugin-a"
	tok := signer.makeToken(t, org, "u", pluginID, time.Hour)

	pOwned1 := uuid.New()
	pForeign := uuid.New()
	pOwned2 := uuid.New()
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(pOwned1).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(pForeign).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(otherOrg))
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(pOwned2).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))

	body := fmt.Sprintf(`[
		{"source_id":"trade-1","portfolio_id":%q},
		{"source_id":"trade-2","portfolio_id":%q},
		{"source_id":"trade-3","portfolio_id":%q}
	]`, pOwned1.String(), pForeign.String(), pOwned2.String())
	req := httptest.NewRequest("POST", "/v6/events/"+pluginID+"/tombstones", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("expected 207, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp bulkResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if len(resp.Succeeded) != 2 || len(resp.Failed) != 1 {
		t.Fatalf("expected 2 succeeded / 1 failed, got %d / %d (%s)", len(resp.Succeeded), len(resp.Failed), rr.Body.String())
	}
	if resp.Failed[0].Index != 1 {
		t.Fatalf("expected failed index 1, got %d", resp.Failed[0].Index)
	}

	got := fake.produced()
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 produces (failed entry excluded), got %d", len(got))
	}
	wantKeys := [][]byte{
		datakey.EventKey(org.String(), pluginID, "trade-1"),
		datakey.EventKey(org.String(), pluginID, "trade-3"),
	}
	for i, rec := range got {
		if rec.Value != nil {
			t.Fatalf("entry %d: tombstone must produce a nil value, got %q", i, rec.Value)
		}
		if !bytes.Equal(rec.Key, wantKeys[i]) {
			t.Fatalf("entry %d key = %q, want %q", i, rec.Key, wantKeys[i])
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestEventsTombstoneBulk_PluginMismatch keeps the handler-level 403 guard: a
// path plugin_id that differs from the JWT plugin_id is rejected before any
// produce.
func TestEventsTombstoneBulk_PluginMismatch(t *testing.T) {
	s, signer, _, fake, cleanup := newServer(t)
	defer cleanup()

	tok := signer.makeToken(t, uuid.New(), "u", "plugin-a", time.Hour)
	body := fmt.Sprintf(`[{"source_id":"trade-1","portfolio_id":%q}]`, uuid.New().String())
	req := httptest.NewRequest("POST", "/v6/events/plugin-b/tombstones", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(fake.produced()) != 0 {
		t.Fatalf("plugin mismatch must produce nothing, got %d", len(fake.produced()))
	}
}

// TestEventsTombstoneBulk_EmptyPortfolioID proves an entry with an empty
// portfolio_id is a per-entry failure with no produce — portfolio events are
// always portfolio-scoped (unlike data.v2's org-scoped null).
func TestEventsTombstoneBulk_EmptyPortfolioID(t *testing.T) {
	s, signer, _, fake, cleanup := newServer(t)
	defer cleanup()

	org := uuid.New()
	const pluginID = "plugin-a"
	tok := signer.makeToken(t, org, "u", pluginID, time.Hour)

	body := `[{"source_id":"trade-1","portfolio_id":""}]`
	req := httptest.NewRequest("POST", "/v6/events/"+pluginID+"/tombstones", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("expected 207, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp bulkResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if len(resp.Succeeded) != 0 || len(resp.Failed) != 1 {
		t.Fatalf("expected 0 succeeded / 1 failed, got %d / %d (%s)", len(resp.Succeeded), len(resp.Failed), rr.Body.String())
	}
	if len(fake.produced()) != 0 {
		t.Fatalf("empty portfolio_id must produce nothing, got %d", len(fake.produced()))
	}
}

// ---- uninstall purge (/internal/tombstone) -------------------------------

// TestUninstallTombstone_InScope_AllSucceed posts a batch of keys all prefixed
// by the token's scope_org_id|scope_plugin_id. The handler must produce exactly
// one nil-value record per key and return 200 keys_published=N.
func TestUninstallTombstone_InScope_AllSucceed(t *testing.T) {
	s, signer, _, fake, cleanup := newServer(t)
	defer cleanup()

	const orgA = "org-a"
	const pluginA = "plugin-a"
	tok := signer.makeTombstoneToken(t, orgA, pluginA, []string{"data.v2"}, time.Hour)

	keys := []string{
		orgA + "|" + pluginA + "|prices.ohlcv|pf1|AAPL|100",
		orgA + "|" + pluginA + "|prices.ohlcv|pf2|MSFT|200",
	}
	body, _ := json.Marshal(map[string]any{"topic": "data.v2", "keys": keys})
	req := httptest.NewRequest("POST", "/internal/tombstone", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]int
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if resp["keys_published"] != len(keys) {
		t.Fatalf("keys_published = %d, want %d", resp["keys_published"], len(keys))
	}
	got := fake.produced()
	if len(got) != len(keys) {
		t.Fatalf("expected %d produces, got %d", len(keys), len(got))
	}
	for i, rec := range got {
		if rec.Value != nil {
			t.Fatalf("entry %d: tombstone must produce a nil value, got %q", i, rec.Value)
		}
		if string(rec.Key) != keys[i] {
			t.Fatalf("entry %d key = %q, want %q", i, rec.Key, keys[i])
		}
		if rec.Topic != "data.v2" {
			t.Fatalf("entry %d topic = %q, want data.v2", i, rec.Topic)
		}
	}
}

// TestUninstallTombstone_OutOfScopeKey_RejectsWholeBatch proves the all-or-
// nothing fail-closed: a single key from another org (or plugin) in an otherwise
// in-scope batch rejects the entire request with 403 and produces ZERO records.
func TestUninstallTombstone_OutOfScopeKey_RejectsWholeBatch(t *testing.T) {
	s, signer, _, fake, cleanup := newServer(t)
	defer cleanup()

	const orgA = "org-a"
	const pluginA = "plugin-a"
	tok := signer.makeTombstoneToken(t, orgA, pluginA, []string{"data.v2"}, time.Hour)

	// First two are in scope; the third escapes to org-b.
	keys := []string{
		orgA + "|" + pluginA + "|prices.ohlcv|pf1|AAPL|100",
		orgA + "|" + pluginA + "|prices.ohlcv|pf2|MSFT|200",
		"org-b|" + pluginA + "|prices.ohlcv|pf3|GOOG|300",
	}
	body, _ := json.Marshal(map[string]any{"topic": "data.v2", "keys": keys})
	req := httptest.NewRequest("POST", "/internal/tombstone", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if n := len(fake.produced()); n != 0 {
		t.Fatalf("out-of-scope batch must produce nothing, got %d produces", n)
	}
}

// TestUninstallTombstone_OutOfScopePlugin_RejectsWholeBatch is the plugin-axis
// twin of the org-axis test: a key for another plugin under the same org is
// still outside the token's prefix and fails the whole batch closed.
func TestUninstallTombstone_OutOfScopePlugin_RejectsWholeBatch(t *testing.T) {
	s, signer, _, fake, cleanup := newServer(t)
	defer cleanup()

	const orgA = "org-a"
	const pluginA = "plugin-a"
	tok := signer.makeTombstoneToken(t, orgA, pluginA, []string{"data.v2"}, time.Hour)

	keys := []string{
		orgA + "|" + pluginA + "|prices.ohlcv|pf1|AAPL|100",
		orgA + "|plugin-b|prices.ohlcv|pf2|MSFT|200",
	}
	body, _ := json.Marshal(map[string]any{"topic": "data.v2", "keys": keys})
	req := httptest.NewRequest("POST", "/internal/tombstone", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if n := len(fake.produced()); n != 0 {
		t.Fatalf("out-of-scope batch must produce nothing, got %d produces", n)
	}
}

// TestUninstallTombstone_MissingScopeClaim_Rejects rejects a token that lacks
// scope_org_id (or scope_plugin_id): without both, no prefix can be enforced,
// so the request fails closed with 403 before any produce.
func TestUninstallTombstone_MissingScopeClaim_Rejects(t *testing.T) {
	s, signer, _, fake, cleanup := newServer(t)
	defer cleanup()

	const pluginA = "plugin-a"
	// Empty scopeOrgID -> claim omitted by makeTombstoneToken.
	tok := signer.makeTombstoneToken(t, "", pluginA, []string{"data.v2"}, time.Hour)

	keys := []string{"org-a|" + pluginA + "|prices.ohlcv|pf1|AAPL|100"}
	body, _ := json.Marshal(map[string]any{"topic": "data.v2", "keys": keys})
	req := httptest.NewRequest("POST", "/internal/tombstone", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if n := len(fake.produced()); n != 0 {
		t.Fatalf("missing-scope token must produce nothing, got %d produces", n)
	}
}

// TestUninstallTombstone_TopicOutOfScope keeps the existing scope_topics guard:
// a key correctly prefixed but on a topic the token wasn't minted for is 403.
func TestUninstallTombstone_TopicOutOfScope(t *testing.T) {
	s, signer, _, fake, cleanup := newServer(t)
	defer cleanup()

	const orgA = "org-a"
	const pluginA = "plugin-a"
	// Token scoped only to data.v2; request targets portfolio_events.v2.
	tok := signer.makeTombstoneToken(t, orgA, pluginA, []string{"data.v2"}, time.Hour)

	keys := []string{orgA + "|" + pluginA + "|pf1"}
	body, _ := json.Marshal(map[string]any{"topic": "portfolio_events.v2", "keys": keys})
	req := httptest.NewRequest("POST", "/internal/tombstone", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if n := len(fake.produced()); n != 0 {
		t.Fatalf("topic-out-of-scope must produce nothing, got %d produces", n)
	}
}

// TestKeysInScope unit-tests the pure prefix predicate the handler relies on.
func TestKeysInScope(t *testing.T) {
	prefix := "org-a|plugin-a|"
	cases := []struct {
		name string
		keys []string
		want bool
	}{
		{"empty set in scope", nil, true},
		{"all in scope", []string{prefix + "a", prefix + "b"}, true},
		{"one wrong org", []string{prefix + "a", "org-b|plugin-a|c"}, false},
		{"one wrong plugin", []string{prefix + "a", "org-a|plugin-b|c"}, false},
		{"prefix-only-no-trailing", []string{"org-a|plugin-a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keysInScope(tc.keys, prefix); got != tc.want {
				t.Fatalf("keysInScope(%v) = %v, want %v", tc.keys, got, tc.want)
			}
		})
	}
}
