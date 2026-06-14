// Package janitor prunes the plugin staging namespace (plugins-staging/*). A
// freshly-published artifact lands in staging and is either promoted into the
// trusted namespace (by the git-PR-driven reconcile workflow) or it stays there
// forever as the signed archive. This package sweeps staging on a ticker and
// removes only the cruft:
//
//   - UNSIGNED artifacts are pruned immediately — they can never be promoted
//     (promotion requires a verifiable cosign signature), so they are pure
//     garbage taking up registry space.
//   - SIGNED-but-unpromoted artifacts are KEPT FOREVER — staging is the
//     permanent signed archive; a signed build may be re-promoted at any time
//     or installed as a preview without re-publishing.
//   - PROMOTED artifacts (the same tag already exists in trusted) are NEVER
//     pruned from staging here.
//
// The janitor ONLY ever touches the staging namespace. It can never delete from
// the trusted namespace: the delete token it mints is scoped to
// plugins-staging/<id> and the broker refuses to sign a delete grant for any
// repository outside that prefix.
package janitor

// stagedTag is the janitor's view of one staged <id>:<tag>: whether it carries
// a cosign signature and whether the same tag is already promoted into trusted.
type stagedTag struct {
	Signed   bool
	Promoted bool
}

// shouldPrune decides whether a staged tag should be reclaimed:
//   - never prune a promoted tag (provenance referrer for a trusted artifact);
//   - prune an unsigned tag immediately (can never be promoted — garbage);
//   - keep a signed-but-unpromoted tag forever (staging is the archive).
func shouldPrune(t stagedTag) bool {
	if t.Promoted {
		return false
	}
	return !t.Signed
}
