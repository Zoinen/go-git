package git

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-billy/v6/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

func TestDiffContextSeparatesStagedAndUnstagedChanges(t *testing.T) {
	t.Parallel()

	wt := newDiffTestWorktree(t)
	writeDiffTestFile(t, wt, "note.txt", "one\ntwo\n")
	_, err := wt.Add("note.txt")
	require.NoError(t, err)
	_, err = wt.Commit("initial", &CommitOptions{Author: diffTestSignature()})
	require.NoError(t, err)

	writeDiffTestFile(t, wt, "note.txt", "one\nTWO\n")
	_, err = wt.Add("note.txt")
	require.NoError(t, err)
	writeDiffTestFile(t, wt, "note.txt", "one\ntres\n")

	staged, err := wt.DiffContext(context.Background(), DiffOptions{Staged: true})
	require.NoError(t, err)
	assert.Contains(t, staged, "diff --git a/note.txt b/note.txt")
	assert.Contains(t, staged, "-two\n")
	assert.Contains(t, staged, "+TWO\n")
	assert.NotContains(t, staged, "tres")

	unstaged, err := wt.DiffContext(context.Background(), DiffOptions{})
	require.NoError(t, err)
	assert.Contains(t, unstaged, "diff --git a/note.txt b/note.txt")
	assert.Contains(t, unstaged, "-TWO\n")
	assert.Contains(t, unstaged, "+tres\n")
	assert.NotContains(t, unstaged, "-two\n")
}

func TestDiffContextIncludesUntrackedOnlyWhenRequested(t *testing.T) {
	t.Parallel()

	wt := newDiffTestWorktree(t)
	writeDiffTestFile(t, wt, "untracked.txt", "untracked\n")

	withoutUntracked, err := wt.DiffContext(context.Background(), DiffOptions{})
	require.NoError(t, err)
	assert.Empty(t, withoutUntracked)

	withUntracked, err := wt.DiffContext(context.Background(), DiffOptions{IncludeUntracked: true})
	require.NoError(t, err)
	assert.Contains(t, withUntracked, "diff --git a/untracked.txt b/untracked.txt")
	assert.Contains(t, withUntracked, "new file mode 100644")
	assert.Contains(t, withUntracked, "+untracked\n")

	filtered, err := wt.DiffContext(context.Background(), DiffOptions{
		IncludeUntracked: true,
		Paths:            []string{"other.txt"},
	})
	require.NoError(t, err)
	assert.Empty(t, filtered)
}

func TestDiffContextReportsBinaryFilesWithoutTextHunks(t *testing.T) {
	t.Parallel()

	wt := newDiffTestWorktree(t)
	writeDiffTestFile(t, wt, "binary.dat", "before\x00")
	_, err := wt.Add("binary.dat")
	require.NoError(t, err)
	_, err = wt.Commit("initial", &CommitOptions{Author: diffTestSignature()})
	require.NoError(t, err)
	writeDiffTestFile(t, wt, "binary.dat", "after\x00")
	_, err = wt.Add("binary.dat")
	require.NoError(t, err)

	patch, err := wt.DiffContext(context.Background(), DiffOptions{Staged: true})
	require.NoError(t, err)
	assert.Contains(t, patch, "Binary files a/binary.dat and b/binary.dat differ")
	assert.NotContains(t, patch, "@@")
}

func TestDiffContextHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	wt := newDiffTestWorktree(t)
	writeDiffTestFile(t, wt, "note.txt", "changed\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	patch, err := wt.DiffContext(ctx, DiffOptions{IncludeUntracked: true})
	assert.Empty(t, patch)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestDiffContextRejectsUnmergedPaths(t *testing.T) {
	t.Parallel()

	wt := newDiffTestWorktree(t)
	writeDiffTestFile(t, wt, "conflict.txt", "content\n")
	_, err := wt.Add("conflict.txt")
	require.NoError(t, err)

	idx, err := wt.r.Storer.Index()
	require.NoError(t, err)
	merged, err := idx.Entry("conflict.txt")
	require.NoError(t, err)
	ours := *merged
	ours.Stage = index.OurMode
	idx.Entries = append(idx.Entries, &ours)
	require.NoError(t, wt.r.Storer.SetIndex(idx))

	patch, err := wt.DiffContext(context.Background(), DiffOptions{Staged: true})
	assert.Empty(t, patch)
	assert.ErrorIs(t, err, ErrDiffUnmerged)
}

func newDiffTestWorktree(t *testing.T) *Worktree {
	t.Helper()
	repo, err := Init(memory.NewStorage(), WithWorkTree(memfs.New()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	wt, err := repo.Worktree()
	require.NoError(t, err)
	return wt
}

func writeDiffTestFile(t *testing.T, wt *Worktree, name, content string) {
	t.Helper()
	require.NoError(t, util.WriteFile(wt.Filesystem(), name, []byte(content), 0o644))
}

func diffTestSignature() *object.Signature {
	return &object.Signature{
		Name:  "Diff Test",
		Email: "diff@example.com",
		When:  time.Unix(1, 0),
	}
}

func TestDiffContextRejectsUnsafePathFilters(t *testing.T) {
	t.Parallel()

	wt := newDiffTestWorktree(t)
	_, err := wt.DiffContext(context.Background(), DiffOptions{Paths: []string{".", "../outside"}})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "invalid diff path"))
}
