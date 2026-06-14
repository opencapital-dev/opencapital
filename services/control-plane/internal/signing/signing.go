package signing

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Key is one row from control_db.jwt_signing_keys. v6 uses RS256 because
// RisingWave's OAuth user binding (the only consumer of /jwt/jwks beyond
// the gateway) parses JWKs assuming RSA-shape (`n`, `e`). ES256 / EC keys
// are rejected by RW's parser with "missing field `n`".
type Key struct {
	Kid       string
	Alg       string
	Private   *rsa.PrivateKey
	Public    *rsa.PublicKey
	CreatedAt time.Time
	RetiresAt *time.Time
}

// Store loads + caches signing keys from control_db.jwt_signing_keys.
type Store struct {
	pool *pgxpool.Pool

	mu     sync.RWMutex
	active *Key
	all    []*Key
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const rsaBits = 2048

// EnsureBootstrap generates one RS256 keypair if no active RS256 row
// exists, then primes the cache. Idempotent across boots.
func (s *Store) EnsureBootstrap(ctx context.Context) error {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM jwt_signing_keys WHERE alg = 'RS256' AND retires_at IS NULL`,
	).Scan(&n); err != nil {
		return fmt.Errorf("count signing keys: %w", err)
	}
	if n == 0 {
		if err := s.generate(ctx); err != nil {
			return fmt.Errorf("generate first key: %w", err)
		}
	}
	return s.refresh(ctx)
}

func (s *Store) generate(ctx context.Context) error {
	priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return fmt.Errorf("rsa keygen: %w", err)
	}
	privDER := x509.MarshalPKCS1PrivateKey(priv)
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return err
	}
	privPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER}))
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	kid := uuid.NewString()

	_, err = s.pool.Exec(ctx,
		`INSERT INTO jwt_signing_keys (kid, alg, private_pem, public_pem)
		 VALUES ($1, 'RS256', $2, $3)`,
		kid, privPEM, pubPEM,
	)
	return err
}

func (s *Store) refresh(ctx context.Context) error {
	rows, err := s.pool.Query(ctx,
		`SELECT kid, alg, private_pem, public_pem, created_at, retires_at
		   FROM jwt_signing_keys
		  WHERE alg = 'RS256'
		  ORDER BY created_at`,
	)
	if err != nil {
		return fmt.Errorf("select signing keys: %w", err)
	}
	defer rows.Close()

	var all []*Key
	var active *Key
	for rows.Next() {
		var k Key
		var privPEM, pubPEM string
		var retires *time.Time
		if err := rows.Scan(&k.Kid, &k.Alg, &privPEM, &pubPEM, &k.CreatedAt, &retires); err != nil {
			return fmt.Errorf("scan signing key: %w", err)
		}
		k.RetiresAt = retires
		priv, err := parseRSAPrivate(privPEM)
		if err != nil {
			return fmt.Errorf("parse private %s: %w", k.Kid, err)
		}
		k.Private = priv
		k.Public = &priv.PublicKey
		_ = pubPEM // pub PEM stored only for human inspection / debugging
		all = append(all, &k)
		if retires == nil {
			active = &k
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if active == nil {
		return errors.New("no active RS256 signing key (every row has retires_at set)")
	}

	s.mu.Lock()
	s.all = all
	s.active = active
	s.mu.Unlock()
	return nil
}

func (s *Store) Active() *Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

func (s *Store) All() []*Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Key, len(s.all))
	copy(out, s.all)
	return out
}

func parseRSAPrivate(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if priv, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return priv, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("PEM is not an RSA private key")
	}
	return rsaKey, nil
}

// SessionClaims is the verified payload of a control-plane-minted session JWT.
type SessionClaims struct {
	UserID    string
	OrgID     string
	PluginID  string
	ExpiresAt time.Time
}

// VerifySession parses and validates a session JWT signed by this control
// plane. The kid header selects the corresponding key from the cache;
// signature, exp, iss, and aud are all checked.
func (s *Store) VerifySession(tokenStr, expectIssuer, expectAudience string) (SessionClaims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(expectIssuer),
		jwt.WithAudience(expectAudience),
		jwt.WithExpirationRequired(),
	)
	tok, err := parser.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, k := range s.all {
			if k.Kid == kid {
				return k.Public, nil
			}
		}
		return nil, fmt.Errorf("unknown kid %q", kid)
	})
	if err != nil {
		return SessionClaims{}, err
	}
	if !tok.Valid {
		return SessionClaims{}, errors.New("invalid token")
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return SessionClaims{}, errors.New("unexpected claims type")
	}
	out := SessionClaims{
		UserID:   stringClaim(claims, "sub"),
		OrgID:    stringClaim(claims, "org_id"),
		PluginID: stringClaim(claims, "plugin_id"),
	}
	if expFloat, ok := claims["exp"].(float64); ok {
		out.ExpiresAt = time.Unix(int64(expFloat), 0)
	}
	if out.UserID == "" || out.OrgID == "" || out.PluginID == "" {
		return SessionClaims{}, errors.New("missing sub / org_id / plugin_id claim")
	}
	return out, nil
}

func stringClaim(c jwt.MapClaims, key string) string {
	if v, ok := c[key].(string); ok {
		return v
	}
	return ""
}
