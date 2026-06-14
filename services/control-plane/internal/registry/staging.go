package registry

import (
	"context"
	"fmt"
	"strings"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// StagingClient is the janitor's view of OpenCapital's own registry: it lists +
// signs-checks + prunes the staging namespace and checks the trusted namespace
// for already-promoted tags. Built from REGISTRY_* config (publish path),
// decoupled from the federated catalog.
type StagingClient struct {
	host             string
	plainHTTP        bool
	namespace        string
	stagingNamespace string
	basicAuth        *auth.Client
	deleter          *GHCRDeleter
	enum             RepoEnumerator
}

// NewStaging builds a StagingClient from the registry's publish coordinates.
// internalURL is how control-plane reaches the registry (e.g. https://ghcr.io);
// namespace is the trusted (promoted) prefix and stagingNamespace the candidate
// prefix. username/password provide static basic-auth credentials (GHCR: owner
// + PAT); empty username = anonymous.
func NewStaging(internalURL, namespace, stagingNamespace, username, password string) *StagingClient {
	host := internalURL
	plainHTTP := false
	if rest, ok := strings.CutPrefix(host, "http://"); ok {
		host, plainHTTP = rest, true
	} else if rest, ok := strings.CutPrefix(host, "https://"); ok {
		host = rest
	}
	host = strings.TrimRight(host, "/")
	s := &StagingClient{host: host, plainHTTP: plainHTTP,
		namespace: strings.Trim(namespace, "/"), stagingNamespace: strings.Trim(stagingNamespace, "/")}
	if username != "" {
		s.basicAuth = &auth.Client{
			Client: retry.DefaultClient, Cache: auth.NewCache(),
			Credential: auth.StaticCredential(host, auth.Credential{Username: username, Password: password}),
		}
	}
	return s
}

// WithEnumerator sets the RepoEnumerator used by ListStagingPluginIDs (the
// staging janitor's enumeration). Required when targeting GHCR (whose
// /v2/_catalog is a global list, not per-owner).
func (s *StagingClient) WithEnumerator(e RepoEnumerator) *StagingClient { s.enum = e; return s }

// WithGHCRDelete wires the GitHub Packages REST deleter used by DeleteStagingTag.
// Required when targeting GHCR (which does not support OCI manifest-DELETE).
func (s *StagingClient) WithGHCRDelete(token string) *StagingClient { s.deleter = NewGHCRDeleter(token); return s }

// CanPruneStaging reports whether the janitor has a delete capability wired. When
// false, the janitor still computes + logs its prune decisions but performs no deletes.
func (s *StagingClient) CanPruneStaging() bool { return s.basicAuth != nil || s.deleter != nil }

func (s *StagingClient) repo(ns, id string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(s.host + "/" + ns + "/" + id)
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = s.plainHTTP
	if s.basicAuth != nil {
		repo.Client = s.basicAuth
	}
	return repo, nil
}

// ListVersions returns the promoted (trusted-namespace) versions of a plugin,
// greatest semver first. A plugin that has NEVER been promoted has no trusted
// repository at all; the registry surfaces that as a 404 NameUnknown, which is
// not an error here — it means the promoted set is empty, so this returns nil
// rather than failing. (The staging janitor reads this set to decide "is this
// staged tag already promoted?", for which "no trusted repo" correctly means
// "no".)
func (s *StagingClient) ListVersions(ctx context.Context, id string) ([]string, error) {
	repo, err := s.repo(s.namespace, id)
	if err != nil {
		return nil, err
	}
	var tags []string
	if err := repo.Tags(ctx, "", func(t []string) error { tags = append(tags, t...); return nil }); err != nil {
		if repoAbsent(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list tags: %w", err)
	}
	return sortSemverDesc(tags), nil
}

// ListStagingVersions returns the staged (staging-namespace) versions of a
// plugin, greatest semver first. Mirrors ListVersions but reads the staging
// repo so the promotion sweep can enumerate candidate tags.
func (s *StagingClient) ListStagingVersions(ctx context.Context, id string) ([]string, error) {
	repo, err := s.repo(s.stagingNamespace, id)
	if err != nil {
		return nil, err
	}
	var tags []string
	if err := repo.Tags(ctx, "", func(t []string) error { tags = append(tags, t...); return nil }); err != nil {
		if repoAbsent(err) {
			return nil, nil // no staging repo = no staged versions, not an error
		}
		return nil, fmt.Errorf("list staging tags: %w", err)
	}
	return sortSemverDesc(tags), nil
}

// ListStagingPluginIDs returns the plugin ids present in the staging namespace.
// Used by the promotion sweep to discover candidates published since boot
// (a freshly-staged plugin may have no trusted repo yet).
func (s *StagingClient) ListStagingPluginIDs(ctx context.Context) ([]string, error) {
	if s.enum == nil {
		return nil, fmt.Errorf("no repo enumerator configured")
	}
	ids, err := s.enum.ReposWithPrefix(ctx, s.stagingNamespace+"/")
	if err != nil {
		return nil, fmt.Errorf("enumerate staging repos: %w", err)
	}
	return ids, nil
}

// StagingTagSigned reports whether <id>:<tag> in the staging namespace has a
// cosign signature referrer (the sigstore-bundle artifactType). This is the
// "Signed" input to the janitor predicate — a cheap referrers lookup, NOT a full
// cryptographic verification (the promotion gate does that). A staged tag with a
// signature referrer is treated as signed for retention purposes.
func (s *StagingClient) StagingTagSigned(ctx context.Context, id, tag string) (bool, error) {
	repo, err := s.repo(s.stagingNamespace, id)
	if err != nil {
		return false, err
	}
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return false, fmt.Errorf("resolve %s:%s: %w", id, tag, err)
	}
	_, found, err := findSignatureReferrer(ctx, repo, desc)
	if err != nil {
		return false, fmt.Errorf("find signature referrer %s:%s: %w", id, tag, err)
	}
	return found, nil
}

// DeleteStagingTag prunes <id>:<tag> from the STAGING namespace via the GitHub
// Packages REST API (resolves the package version carrying the tag, then deletes
// it). Returns an error if no GHCR deleter is wired.
func (s *StagingClient) DeleteStagingTag(ctx context.Context, id, tag string) error {
	if s.deleter == nil {
		return fmt.Errorf("delete %s:%s: no GHCR deleter configured", id, tag)
	}
	pkg := s.stagingNamespace + "/" + id
	vid, ok, err := s.deleter.ResolveVersionID(ctx, pkg, tag)
	if err != nil {
		return fmt.Errorf("resolve %s:%s for delete: %w", id, tag, err)
	}
	if !ok {
		return nil // already gone — nothing to prune
	}
	if err := s.deleter.DeletePackageVersion(ctx, pkg, vid); err != nil {
		return fmt.Errorf("delete %s:%s: %w", id, tag, err)
	}
	return nil
}
