package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// notifyGatewayLRUPrime fires a POST /internal/lru-prime against
// every gateway URL configured in GATEWAY_LRU_PRIME_URLS. It is
// strictly fire-and-log per ADR-0034: a missed notification is
// benign because the gateway's miss path falls through to the read
// replica and self-heals.
//
// Called from upsertPortfolio AFTER the row is durable in
// control_db. Calls run sequentially because the v6 dev stack has at
// most one gateway URL; the cost of a goroutine fan-out is not
// justified here.
func (s *Server) notifyGatewayLRUPrime(ctx context.Context, portfolioID, orgID uuid.UUID) {
	if len(s.cfg.GatewayLRUPrimeURLs) == 0 || s.cfg.LRUPrimeToken == "" {
		return
	}
	payload, err := json.Marshal(map[string]string{
		"portfolio_id": portfolioID.String(),
		"org_id":       orgID.String(),
	})
	if err != nil {
		s.logger.Error("lru prime: marshal", "err", err)
		return
	}
	for _, target := range s.cfg.GatewayLRUPrimeURLs {
		if err := s.postLRUPrime(ctx, target, payload); err != nil {
			// Log + continue. Don't block the user-visible
			// POST /portfolios response on notification health.
			s.logger.Warn("lru prime: notify failed", "url", target, "err", err)
		}
	}
}

func (s *Server) postLRUPrime(parent context.Context, url string, body []byte) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lru-Prime-Token", s.cfg.LRUPrimeToken)
	resp, err := s.lruPrimeClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
