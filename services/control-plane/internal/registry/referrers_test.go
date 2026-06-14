package registry

import (
	"context"
	"testing"
)

func TestFindSignatureReferrer_GHCRMislabeledIndex(t *testing.T) {
	c := newFakeClient(t,
		map[string][]string{},
		map[string][]string{"core-datasource": {"v1.0.0"}},
		map[string]bool{"plugins-staging/core-datasource": true},
	)
	repo, err := c.stagingRepo("core-datasource")
	if err != nil {
		t.Fatal(err)
	}
	subj, err := repo.Resolve(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	desc, ok, err := findSignatureReferrer(context.Background(), repo, subj)
	if err != nil {
		t.Fatalf("findSignatureReferrer: %v", err)
	}
	if !ok {
		t.Fatal("expected to find the bundle signature referrer despite mislabeled index artifactType")
	}
	if desc.ArtifactType != sigstoreBundleMediaType {
		t.Fatalf("located referrer artifactType = %q, want %q", desc.ArtifactType, sigstoreBundleMediaType)
	}
}
