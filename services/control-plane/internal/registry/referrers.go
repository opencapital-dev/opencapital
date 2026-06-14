package registry

import (
	"context"
	"fmt"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
)

// sigstoreBundleMediaType is the OCI artifactType cosign attaches the signature
// referrer with (a DSSE-enveloped sigstore bundle). The staging janitor uses it
// (via StagingTagSigned -> findSignatureReferrer) to detect signed tags.
const sigstoreBundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// findSignatureReferrer locates the cosign sigstore-bundle signature among the OCI
// referrers of subject. It does NOT trust the referrers-index descriptor artifactType:
// GHCR populates that from the referrer's config mediaType (so a bundle signature shows
// up as application/vnd.oci.empty.v1+json), which a naive ArtifactType filter would miss.
// Instead it lists all referrers and fetches each candidate manifest, returning the one
// whose own artifactType or first layer is the sigstore bundle type. ok=false when none.
func findSignatureReferrer(ctx context.Context, repo *remote.Repository, subject ocispec.Descriptor) (ocispec.Descriptor, bool, error) {
	var found *ocispec.Descriptor
	err := repo.Referrers(ctx, subject, "", func(refs []ocispec.Descriptor) error {
		for i := range refs {
			if found != nil {
				return nil
			}
			if refs[i].ArtifactType == sigstoreBundleMediaType {
				d := refs[i]
				found = &d
				return nil
			}
			man, err := fetchManifest(ctx, repo, refs[i].Digest.String())
			if err != nil {
				return fmt.Errorf("fetch referrer manifest %s: %w", refs[i].Digest, err)
			}
			if man.ArtifactType == sigstoreBundleMediaType || hasBundleLayer(man) {
				d := refs[i]
				d.ArtifactType = sigstoreBundleMediaType // normalize for callers
				found = &d
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return ocispec.Descriptor{}, false, fmt.Errorf("list referrers: %w", err)
	}
	if found == nil {
		return ocispec.Descriptor{}, false, nil
	}
	return *found, true, nil
}

func hasBundleLayer(man ocispec.Manifest) bool {
	for _, l := range man.Layers {
		if l.MediaType == sigstoreBundleMediaType {
			return true
		}
	}
	return false
}
