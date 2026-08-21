package git

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-billy/v6/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

func TestApplyContextWorktree(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	require.NoError(t, util.WriteFile(filesystem, "file.txt", []byte("one\ntwo\n"), 0o644))
	base, err := wt.hashBlob([]byte("one\ntwo\n"))
	require.NoError(t, err)

	patch := applyPatch("file.txt", base.String(), "@@ -1,2 +1,2 @@\n one\n-two\n+changed\n")
	require.NoError(t, wt.ApplyContext(context.Background(), patch, nil))

	contents, err := util.ReadFile(filesystem, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, "one\nchanged\n", string(contents))

	idx, err := repo.Storer.Index()
	require.NoError(t, err)
	_, err = idx.Entry("file.txt")
	assert.ErrorIs(t, err, index.ErrEntryNotFound)
}

func TestApplyContextStaged(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	require.NoError(t, util.WriteFile(filesystem, "file.txt", []byte("one\ntwo\n"), 0o644))
	_, err := wt.Add("file.txt")
	require.NoError(t, err)
	idx, err := repo.Storer.Index()
	require.NoError(t, err)
	entry, err := idx.Entry("file.txt")
	require.NoError(t, err)

	patch := applyPatch("file.txt", entry.Hash.String(), "@@ -1,2 +1,2 @@\n one\n-two\n+changed\n")
	require.NoError(t, wt.ApplyContext(context.Background(), patch, &ApplyOptions{Staged: true}))

	worktreeContents, err := util.ReadFile(filesystem, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\n", string(worktreeContents))

	idx, err = repo.Storer.Index()
	require.NoError(t, err)
	entry, err = idx.Entry("file.txt")
	require.NoError(t, err)
	blob, err := object.GetBlob(repo.Storer, entry.Hash)
	require.NoError(t, err)
	reader, err := blob.Reader()
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	contents, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "one\nchanged\n", string(contents))
}

func TestApplyContextReverseWorktreeUsesNewBaseFingerprint(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	const before = "one\ntwo\n"
	const after = "one\nchanged\n"
	require.NoError(t, util.WriteFile(filesystem, "file.txt", []byte(after), 0o644))
	beforeHash, err := wt.hashBlob([]byte(before))
	require.NoError(t, err)
	afterHash, err := wt.hashBlob([]byte(after))
	require.NoError(t, err)
	patch := applyPatchWithObjectIDs("file.txt", beforeHash.String(), afterHash.String(), "@@ -1,2 +1,2 @@\n one\n-two\n+changed\n")

	require.NoError(t, wt.ApplyContext(context.Background(), patch, &ApplyOptions{Reverse: true}))
	contents, err := util.ReadFile(filesystem, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, before, string(contents))
}

func TestApplyContextReverseStaged(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	const before = "one\ntwo\n"
	const after = "one\nchanged\n"
	require.NoError(t, util.WriteFile(filesystem, "file.txt", []byte(before), 0o644))
	_, err := wt.Add("file.txt")
	require.NoError(t, err)
	beforeHash, err := wt.hashBlob([]byte(before))
	require.NoError(t, err)
	afterHash, err := wt.hashBlob([]byte(after))
	require.NoError(t, err)
	patch := applyPatchWithObjectIDs("file.txt", beforeHash.String(), afterHash.String(), "@@ -1,2 +1,2 @@\n one\n-two\n+changed\n")

	require.NoError(t, wt.ApplyContext(context.Background(), patch, &ApplyOptions{Staged: true}))
	require.NoError(t, wt.ApplyContext(context.Background(), patch, &ApplyOptions{Staged: true, Reverse: true}))
	idx, err := repo.Storer.Index()
	require.NoError(t, err)
	entry, err := idx.Entry("file.txt")
	require.NoError(t, err)
	assert.Equal(t, beforeHash, entry.Hash)
	contents, err := util.ReadFile(filesystem, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, before, string(contents))
}

func TestApplyContextReverseDeletesAFileCreatedByThePatch(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	newHash, err := wt.hashBlob([]byte("new\n"))
	require.NoError(t, err)
	patch := []byte("diff --git a/new.txt b/new.txt\n" +
		"new file mode 100644\n" +
		"index 0000000000000000000000000000000000000000.." + newHash.String() + "\n" +
		"--- /dev/null\n" +
		"+++ b/new.txt\n" +
		"@@ -0,0 +1 @@\n" +
		"+new\n")
	require.NoError(t, wt.ApplyContext(context.Background(), patch, nil))
	require.NoError(t, wt.ApplyContext(context.Background(), patch, &ApplyOptions{Reverse: true}))
	_, err = filesystem.Stat("new.txt")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestApplyFileReverseInvertsAllPatchDirections(t *testing.T) {
	file := applyFile{
		oldPath:     "before.txt",
		newPath:     "after.txt",
		oldObjectID: "1111111",
		newObjectID: "2222222",
		oldMode:     filemode.Regular,
		newMode:     filemode.Executable,
		hunks: []applyHunk{{
			oldStart: 2,
			oldCount: 3,
			newStart: 4,
			newCount: 5,
			lines: []applyHunkLine{
				{kind: ' ', text: "context"},
				{kind: '-', text: "removed", noNewline: true},
				{kind: '+', text: "added"},
			},
		}},
	}

	file.reverse()
	assert.Equal(t, "after.txt", file.oldPath)
	assert.Equal(t, "before.txt", file.newPath)
	assert.Equal(t, "2222222", file.oldObjectID)
	assert.Equal(t, "1111111", file.newObjectID)
	assert.Equal(t, filemode.Executable, file.oldMode)
	assert.Equal(t, filemode.Regular, file.newMode)
	assert.Equal(t, 4, file.hunks[0].oldStart)
	assert.Equal(t, 5, file.hunks[0].oldCount)
	assert.Equal(t, 2, file.hunks[0].newStart)
	assert.Equal(t, 3, file.hunks[0].newCount)
	assert.Equal(t, byte(' '), file.hunks[0].lines[0].kind)
	assert.Equal(t, byte('+'), file.hunks[0].lines[1].kind)
	assert.True(t, file.hunks[0].lines[1].noNewline)
	assert.Equal(t, byte('-'), file.hunks[0].lines[2].kind)
}

func TestApplyContextPreCanceledDoesNotMutate(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	require.NoError(t, util.WriteFile(filesystem, "file.txt", []byte("one\ntwo\n"), 0o644))
	base, err := wt.hashBlob([]byte("one\ntwo\n"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = wt.ApplyContext(ctx, applyPatch("file.txt", base.String(), "@@ -1,2 +1,2 @@\n one\n-two\n+changed\n"), nil)
	require.ErrorIs(t, err, context.Canceled)
	contents, readErr := util.ReadFile(filesystem, "file.txt")
	require.NoError(t, readErr)
	assert.Equal(t, "one\ntwo\n", string(contents))
}

func TestApplyContextValidatesEveryFileBeforePublishing(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	require.NoError(t, util.WriteFile(filesystem, "first.txt", []byte("first\n"), 0o644))
	require.NoError(t, util.WriteFile(filesystem, "second.txt", []byte("second\n"), 0o644))
	firstHash, err := wt.hashBlob([]byte("first\n"))
	require.NoError(t, err)
	secondHash, err := wt.hashBlob([]byte("second\n"))
	require.NoError(t, err)
	patch := append(applyPatch("first.txt", firstHash.String(), "@@ -1 +1 @@\n-first\n+changed\n"),
		applyPatch("second.txt", secondHash.String(), "@@ -1 +1 @@\n-not-the-source\n+changed\n")...)

	err = wt.ApplyContext(context.Background(), patch, nil)
	require.ErrorIs(t, err, ErrApplyHunkFailed)
	first, readErr := util.ReadFile(filesystem, "first.txt")
	require.NoError(t, readErr)
	second, readErr := util.ReadFile(filesystem, "second.txt")
	require.NoError(t, readErr)
	assert.Equal(t, "first\n", string(first))
	assert.Equal(t, "second\n", string(second))
}

func TestApplyContextRejectsStaleBase(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	require.NoError(t, util.WriteFile(filesystem, "file.txt", []byte("one\ntwo\n"), 0o644))
	err := wt.ApplyContext(context.Background(), applyPatch("file.txt", strings.Repeat("a", 40), "@@ -1,2 +1,2 @@\n one\n-two\n+changed\n"), nil)
	require.ErrorIs(t, err, ErrApplyBaseMismatch)
	contents, readErr := util.ReadFile(filesystem, "file.txt")
	require.NoError(t, readErr)
	assert.Equal(t, "one\ntwo\n", string(contents))
}

func TestApplyContextAcceptsHunkTextThatLooksLikeAFileHeader(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	require.NoError(t, util.WriteFile(filesystem, "file.txt", []byte("-- old\n"), 0o644))
	base, err := wt.hashBlob([]byte("-- old\n"))
	require.NoError(t, err)
	patch := applyPatch("file.txt", base.String(), "@@ -1 +1 @@\n--- old\n+++ new\n")
	require.NoError(t, wt.ApplyContext(context.Background(), patch, nil))
	contents, err := util.ReadFile(filesystem, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, "++ new\n", string(contents))
}

func TestApplyContextPreservesNoNewlineMarker(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	require.NoError(t, util.WriteFile(filesystem, "file.txt", []byte("old"), 0o644))
	base, err := wt.hashBlob([]byte("old"))
	require.NoError(t, err)
	patch := applyPatch("file.txt", base.String(), "@@ -1 +1 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n")
	require.NoError(t, wt.ApplyContext(context.Background(), patch, nil))
	contents, err := util.ReadFile(filesystem, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, "new", string(contents))
}

func TestApplyContextSupportsQuotedTextPath(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	const name = "file name.txt"
	require.NoError(t, util.WriteFile(filesystem, name, []byte("old\n"), 0o644))
	base, err := wt.hashBlob([]byte("old\n"))
	require.NoError(t, err)
	patch := []byte("diff --git \"a/file name.txt\" \"b/file name.txt\"\n" +
		"index " + base.String() + ".." + strings.Repeat("b", len(base.String())) + " 100644\n" +
		"--- \"a/file name.txt\"\n" +
		"+++ \"b/file name.txt\"\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n")
	require.NoError(t, wt.ApplyContext(context.Background(), patch, nil))
	contents, err := util.ReadFile(filesystem, name)
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(contents))
}

func TestApplyContextCreatesAndDeletesTextFiles(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	create := []byte("diff --git a/new.txt b/new.txt\n" +
		"new file mode 100644\n" +
		"index 0000000000000000000000000000000000000000..1111111111111111111111111111111111111111\n" +
		"--- /dev/null\n" +
		"+++ b/new.txt\n" +
		"@@ -0,0 +1 @@\n" +
		"+new\n")
	require.NoError(t, wt.ApplyContext(context.Background(), create, nil))
	contents, err := util.ReadFile(filesystem, "new.txt")
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(contents))

	base, err := wt.hashBlob([]byte("new\n"))
	require.NoError(t, err)
	deletePatch := []byte("diff --git a/new.txt b/new.txt\n" +
		"deleted file mode 100644\n" +
		"index " + base.String() + "..0000000000000000000000000000000000000000\n" +
		"--- a/new.txt\n" +
		"+++ /dev/null\n" +
		"@@ -1 +0,0 @@\n" +
		"-new\n")
	require.NoError(t, wt.ApplyContext(context.Background(), deletePatch, nil))
	_, err = filesystem.Stat("new.txt")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestApplyContextCreatesAnEmptyTextFile(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	patch := []byte("diff --git a/empty.txt b/empty.txt\n" +
		"new file mode 100644\n" +
		"index 0000000000000000000000000000000000000000..e69de29bb2d1d6434b8b29ae775ad8c2e48c5391\n" +
		"--- /dev/null\n" +
		"+++ b/empty.txt\n")
	require.NoError(t, wt.ApplyContext(context.Background(), patch, nil))
	contents, err := util.ReadFile(filesystem, "empty.txt")
	require.NoError(t, err)
	assert.Empty(t, contents)
}

func TestApplyContextRejectsBinaryAndUnresolvedIndex(t *testing.T) {
	repo, wt, filesystem := newApplyWorktree(t)
	defer func() { _ = repo.Close() }()

	err := wt.ApplyContext(context.Background(), []byte("diff --git a/file b/file\nGIT binary patch\n"), nil)
	require.ErrorIs(t, err, ErrApplyBinary)

	require.NoError(t, util.WriteFile(filesystem, "file.txt", []byte("one\n"), 0o644))
	_, err = wt.Add("file.txt")
	require.NoError(t, err)
	idx, err := repo.Storer.Index()
	require.NoError(t, err)
	entry, err := idx.Entry("file.txt")
	require.NoError(t, err)
	conflict := *entry
	conflict.Stage = 2
	idx.Entries = append(idx.Entries, &conflict)
	require.NoError(t, repo.Storer.SetIndex(idx))

	err = wt.ApplyContext(context.Background(), applyPatch("file.txt", entry.Hash.String(), "@@ -1 +1 @@\n-one\n+changed\n"), &ApplyOptions{Staged: true})
	require.ErrorIs(t, err, ErrApplyUnsupported)
}

func TestApplyContextMatchesGitApplyForTextPatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available for parity verification")
	}

	gitDirectory := t.TempDir()
	goGitDirectory := t.TempDir()
	for _, directory := range []string{gitDirectory, goGitDirectory} {
		runGit(t, directory, "init", "--quiet")
		runGit(t, directory, "config", "core.autocrlf", "false")
		require.NoError(t, os.WriteFile(filepath.Join(directory, "file.txt"), []byte("one\ntwo\n"), 0o644))
	}
	patch := []byte("diff --git a/file.txt b/file.txt\n" +
		"--- a/file.txt\n" +
		"+++ b/file.txt\n" +
		"@@ -1,2 +1,2 @@\n" +
		" one\n" +
		"-two\n" +
		"+changed\n")
	patchPath := filepath.Join(gitDirectory, "change.patch")
	require.NoError(t, os.WriteFile(patchPath, patch, 0o644))
	runGit(t, gitDirectory, "apply", "change.patch")

	repo, err := PlainOpen(goGitDirectory)
	require.NoError(t, err)
	defer func() { _ = repo.Close() }()
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.ApplyContext(context.Background(), patch, nil))

	want, err := os.ReadFile(filepath.Join(gitDirectory, "file.txt"))
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(goGitDirectory, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, want, got)

	runGit(t, gitDirectory, "apply", "-R", "change.patch")
	require.NoError(t, wt.ApplyContext(context.Background(), patch, &ApplyOptions{Reverse: true}))
	want, err = os.ReadFile(filepath.Join(gitDirectory, "file.txt"))
	require.NoError(t, err)
	got, err = os.ReadFile(filepath.Join(goGitDirectory, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestApplyContextStagedMatchesGitApplyCached(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available for parity verification")
	}

	gitDirectory := t.TempDir()
	goGitDirectory := t.TempDir()
	for _, directory := range []string{gitDirectory, goGitDirectory} {
		runGit(t, directory, "init", "--quiet")
		runGit(t, directory, "config", "core.autocrlf", "false")
		require.NoError(t, os.WriteFile(filepath.Join(directory, "file.txt"), []byte("one\ntwo\n"), 0o644))
		runGit(t, directory, "add", "file.txt")
	}
	patch := []byte("diff --git a/file.txt b/file.txt\n" +
		"--- a/file.txt\n" +
		"+++ b/file.txt\n" +
		"@@ -1,2 +1,2 @@\n" +
		" one\n" +
		"-two\n" +
		"+changed\n")
	patchPath := filepath.Join(gitDirectory, "change.patch")
	require.NoError(t, os.WriteFile(patchPath, patch, 0o644))
	runGit(t, gitDirectory, "apply", "--cached", "change.patch")

	repo, err := PlainOpen(goGitDirectory)
	require.NoError(t, err)
	defer func() { _ = repo.Close() }()
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.ApplyContext(context.Background(), patch, &ApplyOptions{Staged: true}))

	want := runGitOutput(t, gitDirectory, "show", ":file.txt")
	got := runGitOutput(t, goGitDirectory, "show", ":file.txt")
	assert.Equal(t, want, got)
	worktreeContents, err := os.ReadFile(filepath.Join(goGitDirectory, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\n", string(worktreeContents))

	runGit(t, gitDirectory, "apply", "-R", "--cached", "change.patch")
	require.NoError(t, wt.ApplyContext(context.Background(), patch, &ApplyOptions{Staged: true, Reverse: true}))
	want = runGitOutput(t, gitDirectory, "show", ":file.txt")
	got = runGitOutput(t, goGitDirectory, "show", ":file.txt")
	assert.Equal(t, want, got)
}

func newApplyWorktree(t *testing.T) (*Repository, *Worktree, billy.Filesystem) {
	t.Helper()
	filesystem := memfs.New()
	repo, err := Init(memory.NewStorage(), WithWorkTree(filesystem))
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	return repo, wt, filesystem
}

func applyPatch(name, oldHash, hunk string) []byte {
	return applyPatchWithObjectIDs(name, oldHash, strings.Repeat("b", len(oldHash)), hunk)
}

func applyPatchWithObjectIDs(name, oldHash, newHash, hunk string) []byte {
	return []byte("diff --git a/" + name + " b/" + name + "\n" +
		"index " + oldHash + ".." + newHash + " 100644\n" +
		"--- a/" + name + "\n" +
		"+++ b/" + name + "\n" + hunk)
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func runGitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(output)
}
