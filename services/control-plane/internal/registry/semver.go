package registry

import (
	"sort"

	"golang.org/x/mod/semver"
)

// normSemver returns a tag's v-prefixed canonical form for comparison, or ""
// if it isn't semver. It tolerates legacy BARE tags (e.g. "0.1.0") published
// before tags carried the leading "v" — they normalize to "v0.1.0" so a
// plugin that was only ever released in the old scheme stays visible.
func normSemver(t string) string {
	if semver.IsValid(t) {
		return t
	}
	if v := "v" + t; semver.IsValid(v) {
		return v
	}
	return ""
}

// latestSemver returns the greatest semver tag (the ORIGINAL tag string, so
// the caller resolves the real OCI tag), or "". Bare and v-prefixed tags are
// compared on equal footing.
func latestSemver(tags []string) string {
	best, bestNorm := "", ""
	for _, t := range tags {
		n := normSemver(t)
		if n == "" {
			continue
		}
		if bestNorm == "" || semver.Compare(n, bestNorm) > 0 {
			best, bestNorm = t, n
		}
	}
	return best
}

// sortSemverDesc returns the semver tags (original tag strings), greatest
// first. Bare and v-prefixed tags are ordered together.
func sortSemverDesc(tags []string) []string {
	type tagged struct{ orig, norm string }
	var valid []tagged
	for _, t := range tags {
		if n := normSemver(t); n != "" {
			valid = append(valid, tagged{orig: t, norm: n})
		}
	}
	sort.Slice(valid, func(i, j int) bool { return semver.Compare(valid[i].norm, valid[j].norm) > 0 })
	out := make([]string, len(valid))
	for i, v := range valid {
		out[i] = v.orig
	}
	return out
}
