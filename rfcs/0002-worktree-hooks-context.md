# RFC: Context-aware client-side commit hooks

## Summary

Add `Worktree.CommitWithHooksContext`, a small `HookRunner` abstraction, and
`OSHookRunner` for running the four client-side hooks around a commit without
delegating the commit itself to the `git` executable.

## Motivation

Applications embedding go-git need the same opportunity for repository
policy hooks as command-line Git, but they cannot safely construct a shell
command from a hook path or assume that a `git` executable is installed. A
public runner interface lets an application select its process policy while
keeping hook order, cancellation, the commit-message protocol, and linked
worktree path resolution in one reviewable implementation.

## Detailed design

`CommitWithHooksContext(ctx, message, commitOptions, runner)` invokes hooks in
this order:

1. `pre-commit`;
2. `prepare-commit-msg <message-file> message`;
3. `commit-msg <message-file>`;
4. publish the commit with `CommitContext`;
5. `post-commit`.

The first three hooks can reject publication. `post-commit` runs only after a
commit was published; if it fails, the method returns both the published hash
and the hook error. Missing hooks are successful no-ops. Cancellation is
checked before each hook and is passed to the runner. As with `CommitContext`,
a cancellation racing after publication returns the created hash and the
context error. Because the commit is then durable, `post-commit` still runs
with the context values but without its cancellation signal, before that error
is returned.

`HookRunner` receives a `HookInvocation` with an already-separated program
path and argument vector. `OSHookRunner` starts that program with
`exec.CommandContext`; it never invokes a shell or starts Git, GPG, SSH, or
Git LFS. The runner is intentionally the policy boundary: an embedding
application may supply a runner that refuses process execution, sends the
request to a sandbox, or records invocations for testing.

The default runner resolves `core.hooksPath`; without it, hooks are found in
the common Git directory's `hooks` directory. The latter is significant for
linked worktrees, whose `.git` file identifies a per-worktree Git directory
and whose `commondir` identifies the shared hooks location. Relative
`core.hooksPath` values are resolved from the worktree root, matching the
directory where Git runs client-side hooks. The invocation environment sets
`GIT_DIR`, `GIT_COMMON_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_PREFIX`,
and `GIT_EDITOR`, then overlays caller-provided environment entries.

For OS-backed worktrees, `prepare-commit-msg` and `commit-msg` receive a
temporary message file in the per-worktree Git directory. Changes made by
either hook are read back and become the commit message. In-memory worktrees
can still use custom runners for policy decisions, but have no host pathname
to expose to a child process and therefore receive an empty message pathname.

## Drawbacks

The initial implementation does not implement every Git hook, editor flow,
or hook interpreter convention. In particular it does not infer an
interpreter for a non-native script: `OSHookRunner` only runs a directly
executable program. This avoids implicitly starting a shell and lets callers
that need an interpreter make that policy explicit in their own runner.

## Rationale and alternatives

Calling `git commit` was rejected because it would bypass go-git's index and
object implementation, make behavior dependent on a separately installed
binary, and make cancellation opaque. Embedding hook execution directly in
`Commit` was rejected because many go-git users intentionally run without
process creation privileges. A separate, opt-in operation preserves the
existing `Commit` and `CommitContext` behavior while allowing applications to
make hooks mandatory.
