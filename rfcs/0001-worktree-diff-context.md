# RFC: Context-aware worktree diff

## Summary

Add `Worktree.DiffContext`, returning a unified textual diff for either
`HEAD` versus the index or the index versus the worktree, without executing
an external Git program.

## Motivation

go-git exposes object-to-object patches, but applications that need to inspect
the index and the working tree otherwise have to compose unexported worktree
details. A public operation gives terminal UIs, IDEs, and other integrations a
common, cancellable implementation that does not require `git diff` to be
installed.

## Detailed design

```go
func (w *Worktree) DiffContext(ctx context.Context, o DiffOptions) (string, error)
```

With `DiffOptions.Staged`, the source is `HEAD` and the destination is the
index. Otherwise the source is the index and the destination is the worktree.
`Paths` selects worktree-relative files or directories; `IncludeUntracked`
adds untracked files to an unstaged diff. The output uses go-git's unified
encoder and normal Git `a/` / `b/` prefixes.

The operation reads blob data through the configured object storer and working
tree data through the configured billy filesystem. It does not write objects,
the index, or the worktree, and it does not execute `git`. Worktree text is
normalised consistently with `core.autocrlf` before comparison. Binary files
receive the usual `Binary files ... differ` marker without hunks.

An unresolved index path returns `ErrDiffUnmerged`: choosing one of its
multiple stages would make the produced patch ambiguous. Cancellation is
observed before and between status, filesystem, object-store, and individual
file operations. Like existing object patch APIs, the line-diff calculation
itself is synchronous once both file contents are loaded.

## Drawbacks

This initial API intentionally does not perform rename or copy detection,
external filters, LFS smudging, or a streaming output mode. These are distinct
compatibility policies and can be added in later, independently reviewable
proposals.

## Rationale and alternatives

Returning an encoded unified diff rather than an `object.Patch` matters for
the unstaged case: a worktree file has no object-tree parent. It also makes the
result directly usable by tools that present or edit textual patches.

Delegating to `git diff` was rejected because it breaks pure-Go embedding,
makes behavior depend on a process outside the repository's configured
storage, and cannot offer a uniform cancellation contract.
