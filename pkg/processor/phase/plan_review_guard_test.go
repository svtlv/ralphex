package phase

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRepoReader is a package-local RepoStateReader; tests change head and status between snapshots.
type fakeRepoReader struct {
	root      string
	head      string
	status    string
	headErr   error
	statusErr error
}

func (f *fakeRepoReader) Root() string { return f.root }

func (f *fakeRepoReader) HeadHash() (string, error) { return f.head, f.headErr }

func (f *fakeRepoReader) StatusPorcelain() (string, error) { return f.status, f.statusErr }

// zStatus joins porcelain entries with NUL, the way `status --porcelain -z` does.
func zStatus(entries ...string) string {
	if len(entries) == 0 {
		return ""
	}
	return strings.Join(entries, "\x00") + "\x00"
}

func writeGuardFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
}

// guardEnv builds a temp repo root with a plan file and returns the reader and plan path.
func guardEnv(t *testing.T) (reader *fakeRepoReader, planFile string) {
	t.Helper()
	root := t.TempDir()
	writeGuardFile(t, root, "docs/plans/p.md", "# plan\n")
	return &fakeRepoReader{root: root, head: "aaa", status: zStatus("?? docs/plans/p.md")}, filepath.Join(root, "docs", "plans", "p.md")
}

func TestSnapshotDiff(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, r *fakeRepoReader)
		mutate     func(t *testing.T, r *fakeRepoReader)
		want       []string
		wantMoved  bool
		wantNoDiff bool
	}{
		{
			name:       "no change",
			mutate:     func(*testing.T, *fakeRepoReader) {},
			wantNoDiff: true,
		},
		{
			name: "new file",
			mutate: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, "new.go", "x")
				r.status = zStatus("?? docs/plans/p.md", "?? new.go")
			},
			want: []string{"new.go"},
		},
		{
			name: "new file inside untracked directory",
			mutate: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, "newdir/deep/file.go", "x")
				r.status = zStatus("?? docs/plans/p.md", "?? newdir/deep/file.go")
			},
			want: []string{"newdir/deep/file.go"},
		},
		{
			name: "edit to already dirty file keeps status but changes hash",
			setup: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, "dirty.go", "v1")
				r.status = zStatus(" M dirty.go", "?? docs/plans/p.md")
			},
			mutate: func(t *testing.T, r *fakeRepoReader) { writeGuardFile(t, r.root, "dirty.go", "v2") },
			want:   []string{"dirty.go"},
		},
		{
			name: "deleted file",
			setup: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, "gone.go", "x")
				r.status = zStatus(" M gone.go", "?? docs/plans/p.md")
			},
			mutate: func(t *testing.T, r *fakeRepoReader) {
				require.NoError(t, os.Remove(filepath.Join(r.root, "gone.go")))
				r.status = zStatus(" D gone.go", "?? docs/plans/p.md")
			},
			want: []string{"gone.go"},
		},
		{
			name: "file reverted to clean disappears from status",
			setup: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, "x.go", "x")
				r.status = zStatus(" M x.go", "?? docs/plans/p.md")
			},
			mutate: func(_ *testing.T, r *fakeRepoReader) { r.status = zStatus("?? docs/plans/p.md") },
			want:   []string{"x.go"},
		},
		{
			name: "staged file",
			setup: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, "s.go", "x")
				r.status = zStatus(" M s.go", "?? docs/plans/p.md")
			},
			mutate: func(_ *testing.T, r *fakeRepoReader) { r.status = zStatus("M  s.go", "?? docs/plans/p.md") },
			want:   []string{"s.go"},
		},
		{
			name: "path with spaces and quotes",
			mutate: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, `dir/we ird "name".go`, "x")
				r.status = zStatus("?? docs/plans/p.md", `?? dir/we ird "name".go`)
			},
			want: []string{`dir/we ird "name".go`},
		},
		{
			name: "rename entry consumes origin field",
			mutate: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, "renamed.go", "x")
				r.status = zStatus("?? docs/plans/p.md", "R  renamed.go", "orig.go")
			},
			want: []string{"renamed.go"},
		},
		{
			name: "change under .ralphex is ignored",
			mutate: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, ".ralphex/progress/p.txt", "log")
				r.status = zStatus("?? .ralphex/progress/p.txt", "?? docs/plans/p.md")
			},
			wantNoDiff: true,
		},
		{
			name:       "ralphex directory entry itself is ignored",
			mutate:     func(_ *testing.T, r *fakeRepoReader) { r.status = zStatus("?? .ralphex", "?? docs/plans/p.md") },
			wantNoDiff: true,
		},
		{
			name:      "head moved by a commit",
			mutate:    func(_ *testing.T, r *fakeRepoReader) { r.head = "bbb" },
			wantMoved: true,
		},
		{
			name: "head moved and file changed",
			setup: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, "c.go", "1")
				r.status = zStatus(" M c.go", "?? docs/plans/p.md")
			},
			mutate: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, "c.go", "2")
				r.head = "ccc"
			},
			want:      []string{"c.go"},
			wantMoved: true,
		},
		{
			name: "changed paths are sorted",
			mutate: func(t *testing.T, r *fakeRepoReader) {
				writeGuardFile(t, r.root, "z.go", "1")
				writeGuardFile(t, r.root, "a.go", "1")
				r.status = zStatus("?? z.go", "?? a.go", "?? docs/plans/p.md")
			},
			want: []string{"a.go", "z.go"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader, planFile := guardEnv(t)
			if tc.setup != nil {
				tc.setup(t, reader)
			}
			before, err := snapshotRepo(reader, planFile)
			require.NoError(t, err)

			tc.mutate(t, reader)
			after, err := snapshotRepo(reader, planFile)
			require.NoError(t, err)

			changed, moved := diffSnapshots(before, after)
			assert.Equal(t, tc.wantMoved, moved)
			if tc.wantNoDiff {
				assert.Empty(t, changed)
				return
			}
			if len(tc.want) == 0 {
				assert.Empty(t, changed)
				return
			}
			assert.Equal(t, tc.want, changed)
		})
	}
}

func TestSnapshotRepo_LargeFileUsesSizeAndMtime(t *testing.T) {
	reader, planFile := guardEnv(t)
	big := strings.Repeat("a", maxHashedFileSize+1)
	writeGuardFile(t, reader.root, "big.bin", big)
	reader.status = zStatus(" M big.bin", "?? docs/plans/p.md")

	before, err := snapshotRepo(reader, planFile)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(before.hashes["big.bin"], "big:"), "got %q", before.hashes["big.bin"])

	// same size, different content, different mtime
	writeGuardFile(t, reader.root, "big.bin", strings.Repeat("b", maxHashedFileSize+1))
	later := filepath.Join(reader.root, "big.bin")
	require.NoError(t, os.Chtimes(later, fileTimeAfter(t, later), fileTimeAfter(t, later)))

	after, err := snapshotRepo(reader, planFile)
	require.NoError(t, err)
	changed, _ := diffSnapshots(before, after)
	assert.Equal(t, []string{"big.bin"}, changed)
}

func TestSnapshotRepo_SymlinkAndDirectoryMarkers(t *testing.T) {
	reader, planFile := guardEnv(t)
	require.NoError(t, os.MkdirAll(filepath.Join(reader.root, "sub"), 0o750))
	reader.status = zStatus("?? sub", "?? missing.go", "?? docs/plans/p.md")

	snap, err := snapshotRepo(reader, planFile)
	require.NoError(t, err)
	assert.Equal(t, "dir", snap.hashes["sub"])
	assert.Equal(t, "missing", snap.hashes["missing.go"])

	link := filepath.Join(reader.root, "lnk")
	if linkErr := os.Symlink("target-a", link); linkErr != nil {
		t.Skipf("symlinks not supported: %v", linkErr)
	}
	reader.status = zStatus("?? lnk", "?? docs/plans/p.md")
	snap, err = snapshotRepo(reader, planFile)
	require.NoError(t, err)
	assert.Equal(t, "link:target-a", snap.hashes["lnk"])
}

func TestSnapshotRepo_PathsResolveAgainstRootNotCWD(t *testing.T) {
	reader, planFile := guardEnv(t)
	writeGuardFile(t, reader.root, "pkg/a.go", "one")
	reader.status = zStatus(" M pkg/a.go", "?? docs/plans/p.md")

	// run from an unrelated working directory
	other := t.TempDir()
	t.Chdir(other)

	before, err := snapshotRepo(reader, planFile)
	require.NoError(t, err)
	assert.Equal(t, "docs/plans/p.md", before.planRel)
	assert.True(t, strings.HasPrefix(before.hashes["pkg/a.go"], "sha256:"))

	writeGuardFile(t, reader.root, "pkg/a.go", "two")
	after, err := snapshotRepo(reader, planFile)
	require.NoError(t, err)
	changed, _ := diffSnapshots(before, after)
	assert.Equal(t, []string{"pkg/a.go"}, changed)
}

func TestSnapshotRepo_ReaderErrorDowngradesToPlanBytes(t *testing.T) {
	tests := []struct {
		name   string
		reader RepoStateReader
	}{
		{name: "head error", reader: &fakeRepoReader{root: t.TempDir(), headErr: errors.New("not a git repo")}},
		{name: "status error", reader: &fakeRepoReader{root: t.TempDir(), head: "a", statusErr: errors.New("vcs failed")}},
		{name: "nil reader", reader: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			planFile := filepath.Join(t.TempDir(), "plan.md")
			require.NoError(t, os.WriteFile(planFile, []byte("v1"), 0o600))

			before, err := snapshotRepo(tc.reader, planFile)
			require.Error(t, err)
			assert.False(t, before.hasRepo)
			assert.Equal(t, []byte("v1"), before.plan, "plan bytes are still recorded")

			// plan-only comparison still works
			unchanged, _ := diffSnapshots(before, before)
			assert.Empty(t, unchanged)

			require.NoError(t, os.WriteFile(planFile, []byte("v2"), 0o600))
			after, err := snapshotRepo(tc.reader, planFile)
			require.Error(t, err)
			changed, moved := diffSnapshots(before, after)
			assert.False(t, moved)
			assert.Equal(t, []string{before.planAbs}, changed)
		})
	}
}

func TestSnapshotRepo_PlanFileErrors(t *testing.T) {
	reader, _ := guardEnv(t)
	_, err := snapshotRepo(reader, filepath.Join(t.TempDir(), "missing.md"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read plan file")
}

func TestSnapshotRepo_PlanFileMissingKeepsRepoState(t *testing.T) {
	reader, planFile := guardEnv(t)
	require.NoError(t, os.Remove(planFile))

	snap, err := snapshotRepo(reader, planFile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read plan file")
	assert.True(t, snap.hasRepo, "repo state is read although the plan file is gone")
	assert.Nil(t, snap.plan)
}

func TestPlanChanged_PlanRemoved(t *testing.T) {
	before := repoSnapshot{plan: []byte{}} // empty plan file
	assert.True(t, planChanged(before, repoSnapshot{}), "removing an empty plan file is a change")
	assert.False(t, planChanged(before, repoSnapshot{plan: []byte{}}))
}

func TestSnapshotRepo_PlanPathThroughSymlink(t *testing.T) {
	realRoot := t.TempDir()
	writeGuardFile(t, realRoot, "docs/plans/p.md", "# plan\n")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}
	// git reports the real root, the plan path goes through the symlink
	reader := &fakeRepoReader{root: realRoot, head: "a", status: zStatus("?? docs/plans/p.md")}

	snap, err := snapshotRepo(reader, filepath.Join(link, "docs", "plans", "p.md"))
	require.NoError(t, err)

	assert.Equal(t, "docs/plans/p.md", snap.planRel, "plan path is made relative to the real root")
}

func TestRelToRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	tests := []struct{ name, abs, want string }{
		{name: "inside", abs: filepath.Join(root, "a", "b.md"), want: "a/b.md"},
		{name: "outside", abs: filepath.Join(filepath.Dir(root), "other", "b.md"), want: ""},
		{name: "dot-dot prefixed name is inside", abs: filepath.Join(root, "..plan.md"), want: "..plan.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, relToRoot(root, tc.abs))
		})
	}
}

func TestPlanRestore(t *testing.T) {
	reader, planFile := guardEnv(t)
	before, err := snapshotRepo(reader, planFile)
	require.NoError(t, err)

	// reviewer edits the plan; the plan stays untracked, so only its hash changes
	require.NoError(t, os.WriteFile(planFile, []byte("# edited by reviewer\n"), 0o600))
	writeGuardFile(t, reader.root, "code.go", "oops")
	reader.status = zStatus("?? docs/plans/p.md", "?? code.go")
	after, err := snapshotRepo(reader, planFile)
	require.NoError(t, err)

	changed, _ := diffSnapshots(before, after)
	assert.Equal(t, []string{"code.go", "docs/plans/p.md"}, changed)
	assert.True(t, planChanged(before, after))

	require.NoError(t, restorePlan(before))
	got, err := os.ReadFile(planFile) //nolint:gosec // test path in t.TempDir
	require.NoError(t, err)
	assert.Equal(t, "# plan\n", string(got), "plan restored from stored bytes")

	assert.Equal(t, []string{"code.go"}, withoutPlan(changed, before), "restored plan path is not listed again")
}

func TestPlanRestore_PlanCommittedAndCleanBeforeReview(t *testing.T) {
	// plan is tracked and clean before the review, so it is absent from status; bytes still catch the edit
	root := t.TempDir()
	writeGuardFile(t, root, "plan.md", "clean")
	reader := &fakeRepoReader{root: root, head: "a"}
	planFile := filepath.Join(root, "plan.md")

	before, err := snapshotRepo(reader, planFile)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(planFile, []byte("edited"), 0o600))
	reader.status = zStatus(" M plan.md")
	after, err := snapshotRepo(reader, planFile)
	require.NoError(t, err)

	changed, _ := diffSnapshots(before, after)
	assert.Equal(t, []string{"plan.md"}, changed)

	require.NoError(t, restorePlan(before))
	assert.Empty(t, withoutPlan(changed, before))
	got, err := os.ReadFile(planFile) //nolint:gosec // test path in t.TempDir
	require.NoError(t, err)
	assert.Equal(t, "clean", string(got))
}

func TestRestorePlan_WithoutPath(t *testing.T) {
	require.Error(t, restorePlan(repoSnapshot{}))
}

func TestParsePorcelainZ(t *testing.T) {
	got := parsePorcelainZ(zStatus(" M a.go", "?? b c.go", "R  new.go", "old.go", "?? .ralphex/x", "A  .ralphex"))
	assert.Equal(t, " M", got["a.go"])
	assert.Equal(t, "??", got["b c.go"])
	assert.Equal(t, "R <old.go", got["new.go"])
	assert.NotContains(t, got, ".ralphex/x")
	assert.NotContains(t, got, ".ralphex")
	assert.Len(t, got, 3)

	assert.Empty(t, parsePorcelainZ(""))
}

// fileTimeAfter returns a time one minute after the file mtime, so mtime based fingerprints differ.
func fileTimeAfter(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi.ModTime().Add(time.Minute)
}
