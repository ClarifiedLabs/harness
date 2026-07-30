package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// pathSuggestMaxEntries bounds one directory scan so a huge directory does
	// not turn an error path into real work.
	pathSuggestMaxEntries = 512
	// pathSuggestMaxResults caps the suggestion list appended to an error.
	pathSuggestMaxResults = 3
	// pathSuggestMinScore is the minimum bigram-Dice similarity a candidate
	// name must reach to be suggested, so unrelated entries are not offered.
	pathSuggestMinScore = 0.3
)

// pathSuggestion is one candidate path with its similarity score.
type pathSuggestion struct {
	path  string
	score float64
}

// notExistingPathError appends similar existing path suggestions to a
// not-exist error so the model can retarget without an exploratory list_dir.
// Other errors pass through unchanged.
func notExistingPathError(path string, err error) error {
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if suggestions := similarExistingPaths(".", path); len(suggestions) > 0 {
		return fmt.Errorf("%w; similar existing paths: %s", err, strings.Join(suggestions, ", "))
	}
	return err
}

// similarExistingPaths suggests up to pathSuggestMaxResults existing paths near
// a missing path, so a not-found error can name plausible retargets. It scans
// the missing path's directory for similarly named entries; when the directory
// itself does not exist (a mistyped directory component) it also scans one
// parent level up for a similarly named directory. Both scans are bounded and
// never recurse. root resolves relative missing paths, so tests can pass a
// temp dir instead of changing the process cwd.
func similarExistingPaths(root, missing string) []string {
	missing = strings.TrimSpace(missing)
	if missing == "" {
		return nil
	}
	cleaned := filepath.Clean(missing)
	if !filepath.IsAbs(cleaned) {
		cleaned = filepath.Join(root, cleaned)
	}
	dir, base := filepath.Split(cleaned)
	dir = filepath.Clean(dir)

	var found []pathSuggestion
	sameDir, dirReadable := similarDirEntries(dir, base)
	found = append(found, sameDir...)
	// One parent level up, but only when the directory itself is unreadable
	// (a mistyped directory component like dock/usage.md): a similar sibling
	// directory (docs) combined with the original base is the most useful
	// retarget. When the directory exists, sibling directories are noise
	// rather than likely intent.
	if !dirReadable {
		if parent, dirBase := filepath.Split(dir); dirBase != "" && dirBase != "." {
			siblings, _ := similarDirEntries(filepath.Clean(parent), dirBase)
			for _, s := range siblings {
				if base != "" {
					candidate := filepath.Join(s.path, base)
					if _, err := os.Stat(candidate); err == nil {
						found = append(found, pathSuggestion{path: candidate, score: s.score})
						continue
					}
				}
				found = append(found, s)
			}
		}
	}

	best := make(map[string]float64, len(found))
	for _, f := range found {
		if f.path == cleaned {
			continue
		}
		if best[f.path] < f.score {
			best[f.path] = f.score
		}
	}
	paths := make([]string, 0, len(best))
	for p := range best {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		if best[paths[i]] != best[paths[j]] {
			return best[paths[i]] > best[paths[j]]
		}
		return paths[i] < paths[j]
	})
	if len(paths) > pathSuggestMaxResults {
		paths = paths[:pathSuggestMaxResults]
	}
	return paths
}

// similarDirEntries returns the entries of dir whose names are similar to base,
// scored by character-bigram Dice with a containment boost. readable is false
// when dir could not be read at all.
func similarDirEntries(dir, base string) (out []pathSuggestion, readable bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	lowerBase := strings.ToLower(base)
	baseBigrams := charBigrams(lowerBase)
	for i, entry := range entries {
		if i >= pathSuggestMaxEntries {
			break
		}
		name := entry.Name()
		lowerName := strings.ToLower(name)
		score := diceCoefficient(baseBigrams, charBigrams(lowerName))
		if len(lowerBase) >= 3 && (strings.Contains(lowerName, lowerBase) || strings.Contains(lowerBase, lowerName)) {
			score = max(score, 0.9)
		}
		if score < pathSuggestMinScore {
			continue
		}
		out = append(out, pathSuggestion{path: filepath.Join(dir, name), score: score})
	}
	return out, true
}
