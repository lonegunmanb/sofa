package git

import (
	"path"
	"sort"
	"strings"
)

// ChangedPath describes one changed entry reported by git status in a workspace.
type ChangedPath struct {
	Path      string
	OldPath   string
	Untracked bool
}

// FindUnauthorizedPaths returns a sorted, deduplicated list of changed workspace paths
// that do not match any glob in writableGlobs.
//
// Semantics:
//   - writableGlobs == nil: allow all paths (returns nil).
//   - writableGlobs != nil && len(writableGlobs) == 0: forbid all paths.
//   - For renames (OldPath != ""), both OldPath and Path must satisfy writableGlobs.
func FindUnauthorizedPaths(changed []ChangedPath, writableGlobs []string) []string {
	if writableGlobs == nil || len(changed) == 0 {
		return nil
	}

	unauthorized := make(map[string]struct{})
	addIfUnauthorized := func(raw string) {
		p := cleanRelPOSIXPath(raw)
		if p == "" {
			return
		}
		if len(writableGlobs) == 0 || !matchesAnyGlob(p, writableGlobs) {
			unauthorized[p] = struct{}{}
		}
	}

	for _, c := range changed {
		addIfUnauthorized(c.Path)
		if c.OldPath != "" {
			addIfUnauthorized(c.OldPath)
		}
	}

	if len(unauthorized) == 0 {
		return nil
	}
	out := make([]string, 0, len(unauthorized))
	for p := range unauthorized {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func cleanRelPOSIXPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	p = strings.TrimPrefix(p, "./")
	if p == "." {
		return ""
	}
	return p
}

func matchesAnyGlob(relPath string, globs []string) bool {
	for _, g := range globs {
		if MatchGlob(g, relPath) {
			return true
		}
	}
	return false
}

// MatchGlob reports whether relPath matches a POSIX '/' glob pattern supporting *, ?, and ** (0+ segments).
func MatchGlob(pattern, relPath string) bool {
	pat := strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/")
	if pat == "" {
		return false
	}
	if strings.HasSuffix(pat, "/") {
		pat += "**"
	}
	pat = path.Clean(pat)
	pat = strings.TrimPrefix(pat, "./")
	if pat == "." || pat == "**" {
		return relPath != ""
	}

	rel := cleanRelPOSIXPath(relPath)
	if rel == "" {
		return false
	}

	patSegs := strings.Split(pat, "/")
	pathSegs := strings.Split(rel, "/")

	dp := make([][]bool, len(patSegs)+1)
	for i := range dp {
		dp[i] = make([]bool, len(pathSegs)+1)
	}
	dp[len(patSegs)][len(pathSegs)] = true

	// Trailing "**" segments can match zero remaining path segments.
	for pi := len(patSegs) - 1; pi >= 0; pi-- {
		if patSegs[pi] == "**" {
			dp[pi][len(pathSegs)] = dp[pi+1][len(pathSegs)]
		}
	}

	for pi := len(patSegs) - 1; pi >= 0; pi-- {
		for si := len(pathSegs) - 1; si >= 0; si-- {
			if patSegs[pi] == "**" {
				dp[pi][si] = dp[pi+1][si] || dp[pi][si+1]
				continue
			}
			matched, err := path.Match(patSegs[pi], pathSegs[si])
			if err == nil && matched {
				dp[pi][si] = dp[pi+1][si+1]
			}
		}
	}

	return dp[0][0]
}
