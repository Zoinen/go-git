package git

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-billy/v6/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/memory"
)

func TestStageContext(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		repo, wt, fs := newContextOperationsWorktree(t, memory.NewStorage())
		defer func() { _ = repo.Close() }()

		require.NoError(t, util.WriteFile(fs, "first.txt", []byte("first"), 0o644))
		require.NoError(t, util.WriteFile(fs, "second.txt", []byte("second"), 0o644))

		require.NoError(t, wt.StageContext(nil, "first.txt", "second.txt"))
		idx, err := repo.Storer.Index()
		require.NoError(t, err)
		_, err = idx.Entry("first.txt")
		require.NoError(t, err)
		_, err = idx.Entry("second.txt")
		require.NoError(t, err)
	})

	t.Run("pre-canceled context does not mutate the index", func(t *testing.T) {
		repo, wt, fs := newContextOperationsWorktree(t, memory.NewStorage())
		defer func() { _ = repo.Close() }()

		require.NoError(t, util.WriteFile(fs, "file.txt", []byte("contents"), 0o644))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		require.ErrorIs(t, wt.StageContext(ctx, "file.txt"), context.Canceled)
		idx, err := repo.Storer.Index()
		require.NoError(t, err)
		_, err = idx.Entry("file.txt")
		assert.ErrorIs(t, err, index.ErrEntryNotFound)
	})

	t.Run("cancels between paths", func(t *testing.T) {
		storage := &cancelAfterIndexWriteStorage{Storage: memory.NewStorage()}
		repo, wt, fs := newContextOperationsWorktree(t, storage)
		defer func() { _ = repo.Close() }()

		require.NoError(t, util.WriteFile(fs, "first.txt", []byte("first"), 0o644))
		require.NoError(t, util.WriteFile(fs, "second.txt", []byte("second"), 0o644))
		ctx, cancel := context.WithCancel(context.Background())
		storage.cancel = cancel
		storage.indexWrites = 0

		require.ErrorIs(t, wt.StageContext(ctx, "first.txt", "second.txt"), context.Canceled)
		idx, err := repo.Storer.Index()
		require.NoError(t, err)
		_, err = idx.Entry("first.txt")
		require.NoError(t, err)
		_, err = idx.Entry("second.txt")
		assert.ErrorIs(t, err, index.ErrEntryNotFound)
	})
}

func TestUnstageContext(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		repo, wt, fs := newContextOperationsWorktree(t, memory.NewStorage())
		defer func() { _ = repo.Close() }()

		firstHash, _ := createContextOperationsCommit(t, wt, fs)
		require.NoError(t, util.WriteFile(fs, "first.txt", []byte("changed"), 0o644))
		_, err := wt.Add("first.txt")
		require.NoError(t, err)

		require.NoError(t, wt.UnstageContext(nil, "first.txt"))
		idx, err := repo.Storer.Index()
		require.NoError(t, err)
		entry, err := idx.Entry("first.txt")
		require.NoError(t, err)
		assert.Equal(t, firstHash, entry.Hash)
	})

	t.Run("pre-canceled context does not mutate the index", func(t *testing.T) {
		repo, wt, fs := newContextOperationsWorktree(t, memory.NewStorage())
		defer func() { _ = repo.Close() }()

		_, _ = createContextOperationsCommit(t, wt, fs)
		require.NoError(t, util.WriteFile(fs, "first.txt", []byte("changed"), 0o644))
		_, err := wt.Add("first.txt")
		require.NoError(t, err)
		idx, err := repo.Storer.Index()
		require.NoError(t, err)
		staged, err := idx.Entry("first.txt")
		require.NoError(t, err)
		stagedHash := staged.Hash

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.ErrorIs(t, wt.UnstageContext(ctx, "first.txt"), context.Canceled)

		entry, err := idx.Entry("first.txt")
		require.NoError(t, err)
		assert.Equal(t, stagedHash, entry.Hash)
	})

	t.Run("cancels between paths", func(t *testing.T) {
		storage := &cancelAfterIndexWriteStorage{Storage: memory.NewStorage()}
		repo, wt, fs := newContextOperationsWorktree(t, storage)
		defer func() { _ = repo.Close() }()

		firstHash, secondHash := createContextOperationsCommit(t, wt, fs)
		require.NoError(t, util.WriteFile(fs, "first.txt", []byte("first changed"), 0o644))
		require.NoError(t, util.WriteFile(fs, "second.txt", []byte("second changed"), 0o644))
		_, err := wt.Add("first.txt")
		require.NoError(t, err)
		_, err = wt.Add("second.txt")
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		storage.cancel = cancel
		storage.indexWrites = 0
		require.ErrorIs(t, wt.UnstageContext(ctx, "first.txt", "second.txt"), context.Canceled)

		idx, err := repo.Storer.Index()
		require.NoError(t, err)
		first, err := idx.Entry("first.txt")
		require.NoError(t, err)
		assert.Equal(t, firstHash, first.Hash)
		second, err := idx.Entry("second.txt")
		require.NoError(t, err)
		assert.NotEqual(t, secondHash, second.Hash)
	})
}

func TestCommitContext(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		repo, wt, fs := newContextOperationsWorktree(t, memory.NewStorage())
		defer func() { _ = repo.Close() }()

		require.NoError(t, util.WriteFile(fs, "file.txt", []byte("contents"), 0o644))
		require.NoError(t, wt.StageContext(context.Background(), "file.txt"))

		hash, err := wt.CommitContext(nil, "commit", contextOperationsCommitOptions())
		require.NoError(t, err)
		assert.False(t, hash.IsZero())
		head, err := repo.Head()
		require.NoError(t, err)
		assert.Equal(t, hash, head.Hash())
	})

	t.Run("pre-canceled context does not publish a commit", func(t *testing.T) {
		repo, wt, fs := newContextOperationsWorktree(t, memory.NewStorage())
		defer func() { _ = repo.Close() }()

		require.NoError(t, util.WriteFile(fs, "file.txt", []byte("contents"), 0o644))
		require.NoError(t, wt.StageContext(context.Background(), "file.txt"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		hash, err := wt.CommitContext(ctx, "commit", contextOperationsCommitOptions())
		assert.True(t, hash.IsZero())
		require.ErrorIs(t, err, context.Canceled)
		_, err = repo.Head()
		assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
	})

	t.Run("reports cancellation after publishing the commit", func(t *testing.T) {
		storage := &cancelAfterIndexWriteStorage{Storage: memory.NewStorage()}
		repo, wt, fs := newContextOperationsWorktree(t, storage)
		defer func() { _ = repo.Close() }()

		require.NoError(t, util.WriteFile(fs, "file.txt", []byte("contents"), 0o644))
		require.NoError(t, wt.StageContext(context.Background(), "file.txt"))
		ctx, cancel := context.WithCancel(context.Background())
		storage.cancel = cancel
		storage.cancelOnReferenceWrite = true

		hash, err := wt.CommitContext(ctx, "commit", contextOperationsCommitOptions())
		assert.False(t, hash.IsZero())
		require.ErrorIs(t, err, context.Canceled)
		head, headErr := repo.Head()
		require.NoError(t, headErr)
		assert.Equal(t, hash, head.Hash())
	})
}

func newContextOperationsWorktree(t *testing.T, s storage.Storer) (*Repository, *Worktree, billy.Filesystem) {
	t.Helper()

	fs := memfs.New()
	repo, err := Init(s, WithWorkTree(fs))
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	return repo, wt, fs
}

func createContextOperationsCommit(t *testing.T, wt *Worktree, fs billy.Filesystem) (plumbing.Hash, plumbing.Hash) {
	t.Helper()

	require.NoError(t, util.WriteFile(fs, "first.txt", []byte("first"), 0o644))
	require.NoError(t, util.WriteFile(fs, "second.txt", []byte("second"), 0o644))
	_, err := wt.Add("first.txt")
	require.NoError(t, err)
	_, err = wt.Add("second.txt")
	require.NoError(t, err)
	_, err = wt.Commit("base", contextOperationsCommitOptions())
	require.NoError(t, err)

	idx, err := wt.r.Storer.Index()
	require.NoError(t, err)
	first, err := idx.Entry("first.txt")
	require.NoError(t, err)
	second, err := idx.Entry("second.txt")
	require.NoError(t, err)
	return first.Hash, second.Hash
}

func contextOperationsCommitOptions() *CommitOptions {
	signature := &object.Signature{
		Name:  "context operations",
		Email: "context-operations@example.com",
		When:  time.Unix(1, 0).UTC(),
	}
	return &CommitOptions{Author: signature, Committer: signature}
}

type cancelAfterIndexWriteStorage struct {
	*memory.Storage

	cancel                 context.CancelFunc
	indexWrites            int
	cancelOnReferenceWrite bool
}

func (s *cancelAfterIndexWriteStorage) SetIndex(idx *index.Index) error {
	if err := s.Storage.SetIndex(idx); err != nil {
		return err
	}

	s.indexWrites++
	if s.indexWrites == 1 && s.cancel != nil {
		s.cancel()
	}

	return nil
}

func (s *cancelAfterIndexWriteStorage) SetReference(ref *plumbing.Reference) error {
	if err := s.Storage.SetReference(ref); err != nil {
		return err
	}

	if s.cancelOnReferenceWrite && s.cancel != nil {
		s.cancel()
	}

	return nil
}
