package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v6/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/memory"
)

func TestCommitWithHooksContext(t *testing.T) {
	t.Run("runs commit hooks in Git order", func(t *testing.T) {
		repo, wt, fs := newContextOperationsWorktree(t, memory.NewStorage())
		defer func() { _ = repo.Close() }()

		require.NoError(t, util.WriteFile(fs, "file.txt", []byte("contents"), 0o644))
		require.NoError(t, wt.StageContext(context.Background(), "file.txt"))

		var invocations []HookInvocation
		runner := HookRunnerFunc(func(_ context.Context, invocation HookInvocation) error {
			invocations = append(invocations, invocation)
			return nil
		})

		hash, err := wt.CommitWithHooksContext(context.Background(), "commit", contextOperationsCommitOptions(), runner)
		require.NoError(t, err)
		assert.False(t, hash.IsZero())
		assert.Equal(t, []HookName{
			HookPreCommit,
			HookPrepareCommitMessage,
			HookCommitMessage,
			HookPostCommit,
		}, hookNames(invocations))
		assert.Empty(t, invocations[0].Args)
		assert.Equal(t, []string{"", "message"}, invocations[1].Args)
		assert.Equal(t, []string{""}, invocations[2].Args)
		assert.Empty(t, invocations[3].Args)

		head, err := repo.Head()
		require.NoError(t, err)
		assert.Equal(t, hash, head.Hash())
	})

	t.Run("cancellation stops before the next hook and commit", func(t *testing.T) {
		repo, wt, fs := newContextOperationsWorktree(t, memory.NewStorage())
		defer func() { _ = repo.Close() }()

		require.NoError(t, util.WriteFile(fs, "file.txt", []byte("contents"), 0o644))
		require.NoError(t, wt.StageContext(context.Background(), "file.txt"))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var invocations []HookName
		runner := HookRunnerFunc(func(_ context.Context, invocation HookInvocation) error {
			invocations = append(invocations, invocation.Name)
			cancel()
			return nil
		})

		hash, err := wt.CommitWithHooksContext(ctx, "commit", contextOperationsCommitOptions(), runner)
		assert.True(t, hash.IsZero())
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, []HookName{HookPreCommit}, invocations)
		_, err = repo.Head()
		assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
	})

	t.Run("commit error skips post-commit", func(t *testing.T) {
		repo, wt, _ := newContextOperationsWorktree(t, memory.NewStorage())
		defer func() { _ = repo.Close() }()

		var invocations []HookName
		runner := HookRunnerFunc(func(_ context.Context, invocation HookInvocation) error {
			invocations = append(invocations, invocation.Name)
			return nil
		})

		hash, err := wt.CommitWithHooksContext(context.Background(), "commit", contextOperationsCommitOptions(), runner)
		assert.True(t, hash.IsZero())
		require.ErrorIs(t, err, ErrEmptyCommit)
		assert.Equal(t, []HookName{
			HookPreCommit,
			HookPrepareCommitMessage,
			HookCommitMessage,
		}, invocations)
	})

	t.Run("post-commit error reports the published hash", func(t *testing.T) {
		repo, wt, fs := newContextOperationsWorktree(t, memory.NewStorage())
		defer func() { _ = repo.Close() }()

		require.NoError(t, util.WriteFile(fs, "file.txt", []byte("contents"), 0o644))
		require.NoError(t, wt.StageContext(context.Background(), "file.txt"))
		postCommitErr := errors.New("post-commit failed")
		runner := HookRunnerFunc(func(_ context.Context, invocation HookInvocation) error {
			if invocation.Name == HookPostCommit {
				return postCommitErr
			}
			return nil
		})

		hash, err := wt.CommitWithHooksContext(context.Background(), "commit", contextOperationsCommitOptions(), runner)
		assert.False(t, hash.IsZero())
		require.ErrorIs(t, err, postCommitErr)
		head, headErr := repo.Head()
		require.NoError(t, headErr)
		assert.Equal(t, hash, head.Hash())
	})

	t.Run("published commit still runs post-commit after cancellation", func(t *testing.T) {
		storage := &cancelAfterIndexWriteStorage{Storage: memory.NewStorage()}
		repo, wt, fs := newContextOperationsWorktree(t, storage)
		defer func() { _ = repo.Close() }()

		require.NoError(t, util.WriteFile(fs, "file.txt", []byte("contents"), 0o644))
		require.NoError(t, wt.StageContext(context.Background(), "file.txt"))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		storage.cancel = cancel
		storage.cancelOnReferenceWrite = true
		var invocations []HookName
		runner := HookRunnerFunc(func(_ context.Context, invocation HookInvocation) error {
			invocations = append(invocations, invocation.Name)
			return nil
		})

		hash, err := wt.CommitWithHooksContext(ctx, "commit", contextOperationsCommitOptions(), runner)
		assert.False(t, hash.IsZero())
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, []HookName{
			HookPreCommit,
			HookPrepareCommitMessage,
			HookCommitMessage,
			HookPostCommit,
		}, invocations)
	})
}

func TestCommitWithHooksContextUsesEditedMessageFile(t *testing.T) {
	directory := t.TempDir()
	repo, err := PlainInit(directory, false)
	require.NoError(t, err)
	defer func() { _ = repo.Close() }()

	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "file.txt"), []byte("contents"), 0o644))
	require.NoError(t, wt.StageContext(context.Background(), "file.txt"))

	var messagePath string
	runner := HookRunnerFunc(func(_ context.Context, invocation HookInvocation) error {
		if invocation.Name != HookPrepareCommitMessage {
			return nil
		}
		require.NotEmpty(t, invocation.Path)
		require.Equal(t, directory, invocation.Dir)
		require.Len(t, invocation.Args, 2)
		messagePath = invocation.Args[0]
		require.NoError(t, os.WriteFile(messagePath, []byte("edited by hook"), 0o600))
		return nil
	})

	hash, err := wt.CommitWithHooksContext(context.Background(), "original", contextOperationsCommitOptions(), runner)
	require.NoError(t, err)
	commit, err := repo.CommitObject(hash)
	require.NoError(t, err)
	assert.Equal(t, "edited by hook", commit.Message)
	_, err = os.Stat(messagePath)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestHookPath(t *testing.T) {
	directory := t.TempDir()
	repo, err := PlainInit(directory, false)
	require.NoError(t, err)
	defer func() { _ = repo.Close() }()

	wt, err := repo.Worktree()
	require.NoError(t, err)

	hookPath, err := wt.HookPath(HookPreCommit)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(directory, GitDirName, "hooks", string(HookPreCommit)), hookPath)

	cfg, err := repo.Config()
	require.NoError(t, err)
	cfg.Core.HooksPath = "custom-hooks"
	require.NoError(t, repo.Storer.SetConfig(cfg))

	hookPath, err = wt.HookPath(HookCommitMessage)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(directory, "custom-hooks", string(HookCommitMessage)), hookPath)

	_, err = wt.HookPath(HookName("pre-push"))
	require.ErrorIs(t, err, ErrInvalidHookName)
}

func TestHookDirectoryHelpersSupportLinkedWorktreeLayout(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	gitDirectory := filepath.Join(root, "metadata", "linked")
	commonDirectory := filepath.Join(root, "common")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.MkdirAll(gitDirectory, 0o755))
	require.NoError(t, os.MkdirAll(commonDirectory, 0o755))

	gitdirRelative, err := filepath.Rel(worktree, gitDirectory)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(worktree, GitDirName), []byte("gitdir: "+gitdirRelative+"\n"), 0o600))
	commonRelative, err := filepath.Rel(gitDirectory, commonDirectory)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(gitDirectory, "commondir"), []byte(commonRelative+"\n"), 0o600))

	resolvedGitDirectory, err := gitDirectoryForWorktree(worktree)
	require.NoError(t, err)
	assert.Equal(t, gitDirectory, resolvedGitDirectory)
	resolvedCommonDirectory, err := commonGitDirectory(resolvedGitDirectory)
	require.NoError(t, err)
	assert.Equal(t, commonDirectory, resolvedCommonDirectory)
}

func TestOSHookRunnerDoesNotTreatMissingHookAsFailure(t *testing.T) {
	err := (OSHookRunner{}).RunHook(context.Background(), HookInvocation{
		Name: HookPreCommit,
		Path: filepath.Join(t.TempDir(), "pre-commit"),
	})
	require.ErrorIs(t, err, ErrHookNotFound)
}

func hookNames(invocations []HookInvocation) []HookName {
	result := make([]HookName, len(invocations))
	for index, invocation := range invocations {
		result[index] = invocation.Name
	}
	return result
}
