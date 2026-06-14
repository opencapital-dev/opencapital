package janitor

import "testing"

func TestShouldPruneUnsigned(t *testing.T) {
	// Unsigned artifacts can never be promoted — prune immediately.
	if !shouldPrune(stagedTag{Signed: false, Promoted: false}) {
		t.Fatal("unsigned unpromoted artifact should be pruned immediately")
	}
}

func TestKeepSignedUnpromoted(t *testing.T) {
	// Signed but not yet promoted: keep forever (staging is the permanent archive).
	if shouldPrune(stagedTag{Signed: true, Promoted: false}) {
		t.Fatal("signed unpromoted artifact must be kept indefinitely")
	}
}

func TestNeverPrunePromoted(t *testing.T) {
	// Promoted tags are never pruned regardless of signing state.
	if shouldPrune(stagedTag{Signed: true, Promoted: true}) {
		t.Fatal("promoted artifact must never be pruned")
	}
	if shouldPrune(stagedTag{Signed: false, Promoted: true}) {
		t.Fatal("promoted artifact must never be pruned")
	}
}
