package registry

import "testing"

func TestLatestSemver(t *testing.T) {
	got := latestSemver([]string{"v0.2.0", "v0.10.0", "v0.1.0"})
	if got != "v0.10.0" {
		t.Fatalf("got %q want v0.10.0", got)
	}
}

func TestLatestSemverIgnoresNonSemver(t *testing.T) {
	got := latestSemver([]string{"latest", "v1.0.0", "garbage"})
	if got != "v1.0.0" {
		t.Fatalf("got %q want v1.0.0", got)
	}
}

func TestSortSemverDescending(t *testing.T) {
	got := sortSemverDesc([]string{"v1.0.0", "v1.2.0", "v1.1.0"})
	want := []string{"v1.2.0", "v1.1.0", "v1.0.0"}
	if len(got) != len(want) {
		t.Fatalf("len got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d got %q want %q", i, got[i], want[i])
		}
	}
}

func TestLatestSemverEmpty(t *testing.T) {
	if got := latestSemver(nil); got != "" {
		t.Fatalf("empty: got %q want \"\"", got)
	}
}

// Legacy plugins published before tags carried a leading "v" have bare tags
// (e.g. "0.1.0"). They must stay visible, and the ORIGINAL tag is returned so
// the real OCI tag is resolvable.
func TestLatestSemverBareLegacyTag(t *testing.T) {
	if got := latestSemver([]string{"0.1.0"}); got != "0.1.0" {
		t.Fatalf("bare-only: got %q want 0.1.0", got)
	}
	if got := latestSemver([]string{"0.1.0", "0.1.1"}); got != "0.1.1" {
		t.Fatalf("bare pair: got %q want 0.1.1", got)
	}
}

func TestLatestSemverMixedBareAndV(t *testing.T) {
	// v1.0.4 > 0.1.1; the original (v-prefixed) tag is returned verbatim.
	got := latestSemver([]string{"0.1.0", "0.1.1", "v1.0.1", "v1.0.4"})
	if got != "v1.0.4" {
		t.Fatalf("mixed: got %q want v1.0.4", got)
	}
	// And a bare tag wins when it's the greatest, returned bare.
	got = latestSemver([]string{"v0.1.0", "0.2.0"})
	if got != "0.2.0" {
		t.Fatalf("bare-wins: got %q want 0.2.0", got)
	}
}

func TestSortSemverDescMixed(t *testing.T) {
	got := sortSemverDesc([]string{"0.1.0", "v1.0.4", "0.1.1", "v1.0.1"})
	want := []string{"v1.0.4", "v1.0.1", "0.1.1", "0.1.0"}
	if len(got) != len(want) {
		t.Fatalf("len got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d got %q want %q (%v)", i, got[i], want[i], got)
		}
	}
}
