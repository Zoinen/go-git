# RFC: Context-aware textual worktree apply

## Summary

Add `Worktree.Apply` and `Worktree.ApplyContext` for applying a textual
unified diff to either a worktree or its index without executing `git`.

## Motivation

Programs that use go-git can already inspect a worktree and update its index,
but cannot safely present an editable diff and write it back without composing
private plumbing operations. A public operation gives applications one shared,
reviewable interpretation of the small safe subset required by an interactive
patch editor.

## Detailed design

`ApplyContext` accepts the complete unified patch as `[]byte` and an optional
`ApplyOptions`. With the zero options it updates regular text files in the
worktree. With `Staged` it updates only the index. With `Reverse` it applies
the inverse of the patch, including file paths, modes, object fingerprints,
and hunk additions and removals. The non-context `Apply` method delegates to
it with `context.Background()`.

The initial scope is intentionally narrow:

- only regular text files and textual unified hunks are accepted;
- `GIT binary patch`, NUL bytes, gitlinks, unresolved index entries, renames,
  copies, and mode-only changes return an explicit error;
- if an `index OLD..NEW` header is present, the source fingerprint (`OLD`, or
  `NEW` for `Reverse`) must match the current source object before any
  mutation;
- every file and hunk is parsed and verified before publishing changes;
- staged application builds one replacement index; worktree application first
  writes all replacement files, then renames them into place and restores
  previously-published paths if a later rename fails.

Cancellation is observed during parsing and validation. Publication is a
critical section and is not interrupted once started, matching the semantics
of other context-aware worktree write operations.

The RFC deliberately leaves patch fuzz, three-way application, filter
integration, sparse-index special handling, and binary deltas for subsequent
proposals. Those features have materially different compatibility and failure
semantics and should not be inferred from a UI use case.

## Drawbacks

This is not a drop-in replacement for every `git apply` option. In particular,
users must regenerate an editable patch after a source file changes, and a
multi-file filesystem publication cannot be made globally atomic on all billy
backends.

## Rationale and alternatives

The API uses `[]byte` rather than an `io.Reader` because an interactive editor
already owns a complete buffer and safe application requires complete parsing
and validation before publication. A streaming API could be added later if a
server-side use case demonstrates that it can preserve those guarantees.

Delegating to `git apply` was rejected because go-git users frequently run in
environments without an external Git executable, and it would make behavior
and cancellation platform-dependent.
