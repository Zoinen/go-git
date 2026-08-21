# RFC 0002: local Git LFS object store

## Summary

Add a small, context-aware Git LFS API to the v6 root package.  It reads and
writes the local `lfs/objects` store without shelling out to `git` or
`git-lfs`.

## API

`Repository.LFS` obtains an `LFSClient` for filesystem-backed repositories.
`LFSPointer`, `ParseLFSPointer`, and `LFSPointer.MarshalText` provide strict,
canonical LFS v1 pointer handling.  `CleanContext`, `StoreContext`,
`OpenContext`, `SmudgeContext`, `HasContext`, and `VerifyContext` all expose
context cancellation at I/O boundaries.

## Semantics

Objects use Git LFS's standard `lfs/objects/aa/bb/<sha256>` location.
Writes first target temporary files and publish only after SHA-256 and size
verification.  A racing writer is accepted only after the object it published
also passes full verification.  This prevents corrupt, partial, or cancelled
data from becoming an accepted object.

The initial proposal intentionally contains no transfer API.  HTTP Batch and
pure-SSH transfer need a separate design covering endpoint discovery,
credential handling, resumability, verification, and the interaction with
partial clones.  This API therefore neither runs user programs nor silently
attempts network activity.

## Compatibility

The API is new in v6 and does not change existing repository or worktree
operations.  Repositories backed by a non-filesystem storage return
`ErrLFSStorageUnsupported`, because there is no durable Git-compatible local
store to use.

## Tests

Tests cover canonical pointer parsing, filesystem layout, clean/smudge round
trips, integrity rejection, cancellation, and unsupported in-memory storage.
