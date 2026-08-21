# RFC: Context-aware configured content filters

## Summary

Add a small `FilterResolver` API for resolving a `filter=<driver>`
gitattributes value and applying that driver's clean or smudge conversion.
The initial OS implementation runs only explicitly configured filter programs;
it does not invoke `git`, a shell, or any f4-specific code.

## Motivation

go-git already parses gitattributes, but consumers that need Git-compatible
content conversion must currently either ignore custom filters or construct a
shell command themselves. That makes it difficult to stage filtered content,
show a filtered diff, or support Git LFS' `filter.<name>.process` protocol
without an installed Git executable.

A public resolver keeps the policy boundary explicit. Applications that do
not permit child processes can provide their own resolver; applications that
do can opt into the supplied OS implementation and cancellation semantics.

## Detailed design

`FilterResolver.Resolve(context.Context, name)` returns a `Filter` for one
`[filter "name"]` subsection. Attribute matching remains separate: callers use
`plumbing/format/gitattributes` to select `name` for a repository-relative
path. `Filter.Apply` accepts a full `FilterRequest` with a clean/smudge
direction, relative path, and complete content. A complete byte slice allows
the caller to retain the original content if Git's optional-filter fallback is
needed before atomically publishing an index or worktree update.

`OSFilterResolver` snapshots `clean`, `smudge`, `process`, and `required` at
resolve time. A configured `process` takes precedence over the one-shot
commands, as it does in Git. It speaks the version-2 pkt-line handshake,
capability negotiation, request, content, and final-status exchange used by
the long-running filter process protocol. The first implementation opens one
process for each `Apply` and closes its input after the exchange. This is a
correct subset of the protocol that avoids global workers, makes cancellation
and cleanup deterministic, and is suitable for initial worktree integration;
a future `FilterSession` may retain a successfully negotiated process for a
batch without changing the resolver API.

One-shot command strings are parsed into a literal argv vector and started
with `exec.CommandContext`. No shell is ever used. The parser supports
whitespace, single quotes, double quotes, and backslash escaping; shell
operators and expansions remain literal argv bytes. `%f` is substituted in an
already-separated argv element with the repository-relative request path.

For a command or protocol failure, a driver without `required=true` returns an
unchanged copy of its input, matching Git's optional-filter behavior. A
required driver returns an error matching `ErrFilterRequired`. Cancellation is
never converted to that optional fallback. A filter process failure never
falls back to configured `clean` or `smudge`, because Git gives `process`
precedence.

## Drawbacks

The complete-buffer API is not ideal for arbitrarily large blobs, and the
per-apply process lifetime does not yet provide the throughput benefit that
motivates long-running filters. Both constraints are deliberate: publishing a
worktree or index update needs a complete result, and process pooling needs a
separate ownership, retry, and shutdown design. The API also does not infer a
shell interpreter for existing shell-oriented filter configuration; users can
supply an explicit executable argv command or a custom resolver.

## Rationale and alternatives

Delegating to `git check-attr` or `git add` would make filtering depend on a
separate executable and would bypass go-git's index and object code. Running
configuration strings through a shell would reproduce Git's historical
convenience but would make embedding-process policy opaque and expose callers
to shell-specific expansion. A narrow no-shell resolver provides a safe common
base while retaining an extension point for applications with different
execution policy.
