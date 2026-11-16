package phase

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:generate moq -out mocks/repo_state_reader.go -pkg mocks -skip-ensure -fmt goimports . RepoStateReader

// maxHashedFileSize is the largest file whose content is hashed by the repo guard.
// larger files are fingerprinted by size and mtime only.
const maxHashedFileSize = 1 << 20

// guardSkipDir is the project-local directory the guard ignores: the progress file grows during a review.
const guardSkipDir = ".ralphex"

// RepoStateReader reads repository state for the plan review read-only guard.
type RepoStateReader interface {
	// Root returns the absolute path to the repository root; git paths are resolved against it.
	Root() string
	// HeadHash returns the current HEAD commit hash.
	HeadHash() (string, error)
	// StatusPorcelain returns `git status --porcelain -uall -z` output (NUL-separated entries).
	StatusPorcelain() (string, error)
}

// repoSnapshot is the state compared before and after a reviewer call.
type repoSnapshot struct {
	hasRepo  bool              // repo state was read; when false only the plan bytes are compared
	head     string            // HEAD commit hash
	status   map[string]string // repo-relative slash path -> status code (and rename origin)
	hashes   map[string]string // repo-relative slash path -> content fingerprint
	planAbs  string            // absolute plan file path
	planRel  string            // plan path relative to the repo root, slash separated (empty without repo)
	plan     []byte            // plan file bytes
	planMode os.FileMode       // plan file mode, kept for restore
}

// snapshotRepo records the state the guard compares after a review.
// the returned snapshot always carries the plan bytes when the plan file is readable. when repo state
// cannot be read, it returns that error together with a plan-bytes-only snapshot, so the caller can
// downgrade the guard instead of failing the review.
func snapshotRepo(reader RepoStateReader, planFile string) (repoSnapshot, error) {
	snap := repoSnapshot{}
	abs, err := filepath.Abs(planFile)
	if err != nil {
		return snap, fmt.Errorf("resolve plan path: %w", err)
	}
	snap.planAbs = abs
	data, err := os.ReadFile(abs) //nolint:gosec // plan path comes from the user's own selection
	if err != nil {
		return snap, fmt.Errorf("read plan file: %w", err)
	}
	snap.plan = data
	snap.planMode = 0o644
	if fi, statErr := os.Stat(abs); statErr == nil {
		snap.planMode = fi.Mode().Perm()
	}

	if reader == nil {
		return snap, errors.New("repo state reader is not set")
	}
	root := reader.Root()
	head, err := reader.HeadHash()
	if err != nil {
		return snap, fmt.Errorf("read HEAD: %w", err)
	}
	porcelain, err := reader.StatusPorcelain()
	if err != nil {
		return snap, fmt.Errorf("read status: %w", err)
	}

	snap.hasRepo = true
	snap.head = head
	snap.status = parsePorcelainZ(porcelain)
	snap.hashes = make(map[string]string, len(snap.status))
	for path := range snap.status {
		snap.hashes[path] = fingerprintFile(filepath.Join(root, filepath.FromSlash(path)))
	}
	if rel, relErr := filepath.Rel(root, abs); relErr == nil && !strings.HasPrefix(rel, "..") {
		snap.planRel = filepath.ToSlash(rel)
	}
	return snap, nil
}

// parsePorcelainZ parses `status --porcelain -uall -z` output into path -> status entries.
// rename and copy entries carry the origin path as the next NUL field; it is folded into the status value.
// paths under .ralphex/ are dropped.
func parsePorcelainZ(out string) map[string]string {
	entries := make(map[string]string)
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 { // "XY p" is the shortest valid entry
			continue
		}
		code, path := f[:2], f[3:]
		if code[0] == 'R' || code[0] == 'C' || code[1] == 'R' || code[1] == 'C' {
			if i+1 < len(fields) {
				code += "<" + fields[i+1]
				i++
			}
		}
		if isGuardSkipped(path) {
			continue
		}
		entries[path] = code
	}
	return entries
}

// isGuardSkipped reports whether a repo-relative slash path is under the ignored .ralphex/ directory.
func isGuardSkipped(path string) bool {
	return path == guardSkipDir || strings.HasPrefix(path, guardSkipDir+"/")
}

// fingerprintFile returns a content fingerprint: sha256 for files up to maxHashedFileSize,
// size and mtime for larger files, and fixed markers for missing files, directories and symlinks.
func fingerprintFile(path string) string {
	fi, err := os.Lstat(path)
	if err != nil {
		return "missing"
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, linkErr := os.Readlink(path)
		if linkErr != nil {
			return "link:?"
		}
		return "link:" + target
	case fi.IsDir():
		return "dir"
	case fi.Size() > maxHashedFileSize:
		return fmt.Sprintf("big:%d:%d", fi.Size(), fi.ModTime().UnixNano())
	}
	data, err := os.ReadFile(path) //nolint:gosec // path comes from git status inside the repo
	if err != nil {
		return "unreadable"
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// diffSnapshots returns the sorted repo-relative paths that differ between two snapshots
// (added, removed, status changed or content changed) and whether HEAD moved.
// the plan file counts as changed when its bytes differ, even without repo state.
func diffSnapshots(before, after repoSnapshot) (changed []string, headMoved bool) {
	set := make(map[string]struct{})

	if before.hasRepo && after.hasRepo {
		headMoved = before.head != after.head
		for path, st := range before.status {
			if afterSt, ok := after.status[path]; !ok || afterSt != st || before.hashes[path] != after.hashes[path] {
				set[path] = struct{}{}
			}
		}
		for path := range after.status {
			if _, ok := before.status[path]; !ok {
				set[path] = struct{}{}
			}
		}
	}

	if !bytes.Equal(before.plan, after.plan) {
		name := before.planRel
		if name == "" {
			name = before.planAbs
		}
		set[name] = struct{}{}
	}

	changed = make([]string, 0, len(set))
	for path := range set {
		changed = append(changed, path)
	}
	sort.Strings(changed)
	return changed, headMoved
}

// planChanged reports whether the plan file bytes differ between two snapshots.
func planChanged(before, after repoSnapshot) bool {
	return !bytes.Equal(before.plan, after.plan)
}

// restorePlan writes the plan bytes stored in the snapshot back to the plan file.
// the caller drops the plan path from the changed list with withoutPlan, so the plan is not reported twice.
func restorePlan(snap repoSnapshot) error {
	if snap.planAbs == "" {
		return errors.New("no plan path in snapshot")
	}
	if err := os.WriteFile(snap.planAbs, snap.plan, snap.planMode); err != nil {
		return fmt.Errorf("restore plan file: %w", err)
	}
	return nil
}

// withoutPlan returns changed without the snapshot's plan path.
func withoutPlan(changed []string, snap repoSnapshot) []string {
	name := snap.planRel
	if name == "" {
		name = snap.planAbs
	}
	out := make([]string, 0, len(changed))
	for _, p := range changed {
		if p != name {
			out = append(out, p)
		}
	}
	return out
}
