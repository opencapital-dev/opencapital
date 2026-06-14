//go:build live

package registry

import (
	"context"
	"os"
	"testing"
)

// GHCR_LIVE=1 GHCR_OWNER=<owner> GHCR_TOKEN=<pat> go test -tags live -run TestGHCRLive ./internal/registry/
func TestGHCRLive_EnumerateStaging(t *testing.T) {
	if os.Getenv("GHCR_LIVE") != "1" {
		t.Skip("set GHCR_LIVE=1 (with GHCR_OWNER, GHCR_TOKEN) to run")
	}
	owner, token := os.Getenv("GHCR_OWNER"), os.Getenv("GHCR_TOKEN")
	if owner == "" || token == "" {
		t.Skip("set GHCR_OWNER and GHCR_TOKEN to run")
	}
	s := NewStaging("https://ghcr.io", "plugins", "plugins-staging", owner, token).
		WithEnumerator(NewGHCREnumerator(token))
	ids, err := s.ListStagingPluginIDs(context.Background())
	if err != nil {
		t.Fatalf("enumerate staging on GHCR: %v", err)
	}
	t.Logf("staging plugin ids on GHCR: %v", ids)
}
