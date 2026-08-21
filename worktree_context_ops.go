package git

import (
	"context"

	"github.com/go-git/go-git/v6/plumbing"
)

// StageContext adds each path to the index, equivalent to calling Add for
// every path. Directories are staged recursively by the same rules as Add.
//
// Cancellation is checked before and after every path. If ctx is canceled
// after one path has been staged, paths staged before cancellation remain in
// the index. A nil context is treated as context.Background.
func (w *Worktree) StageContext(ctx context.Context, paths ...string) error {
	ctx = worktreeOperationContext(ctx)

	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}

		if _, err := w.Add(path); err != nil {
			return err
		}
	}

	return ctx.Err()
}

// UnstageContext restores each path in the index from HEAD, leaving the
// working tree unchanged. It is equivalent to a mixed reset for every path.
//
// Cancellation is checked before and after every path. If ctx is canceled
// after one path has been unstaged, paths unstaged before cancellation remain
// unstaged. A nil context is treated as context.Background.
func (w *Worktree) UnstageContext(ctx context.Context, paths ...string) error {
	ctx = worktreeOperationContext(ctx)

	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := w.Restore(&RestoreOptions{
			Staged: true,
			Files:  []string{path},
		}); err != nil {
			return err
		}
	}

	return ctx.Err()
}

// CommitContext stores the current index in a new commit. It is equivalent to
// Commit, with cancellation checked before and after the commit operation.
//
// Commit cannot be interrupted once publishing the commit has begun. If ctx
// is canceled while Commit is running, CommitContext returns the created hash
// together with the context error so callers can determine whether the commit
// was published. A nil context is treated as context.Background.
func (w *Worktree) CommitContext(ctx context.Context, msg string, opts *CommitOptions) (plumbing.Hash, error) {
	ctx = worktreeOperationContext(ctx)
	if err := ctx.Err(); err != nil {
		return plumbing.ZeroHash, err
	}

	hash, err := w.Commit(msg, opts)
	if err != nil {
		return hash, err
	}

	if err := ctx.Err(); err != nil {
		return hash, err
	}

	return hash, nil
}

func worktreeOperationContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}

	return ctx
}
