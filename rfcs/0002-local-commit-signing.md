# RFC 0002: local commit-signing resolvers

## Summary

Add a small resolver for the existing public `Signer` interface. It reads the
repository's `gpg.format` and `user.signingKey` configuration and returns a
signer without executing `gpg`, `ssh-keygen`, or a shell.

## API

`Repository.ResolveCommitSigner(context.Context, *SigningKeyResolverOptions)`
returns a normal `Signer`; callers keep using `CommitOptions.Signer` unchanged.
The resolver supports `openpgp` (the default) and `ssh`. `x509` returns the
exported `ErrX509SigningUnsupported` error and an unknown format returns
`ErrSigningFormatUnsupported`.

`NewOpenPGPFileSignerContext` accepts armored or binary private keyrings.
`NewSSHFileSignerContext` creates armored OpenSSH `sshsig` signatures in the
Git `git` namespace. `NewSSHAgentSigner` accepts an already connected
`ssh/agent.Agent`; connection ownership stays with the caller.

## Rationale

Applications embedding go-git need Git-compatible signing but must not need a
`git`, `gpg`, or `ssh-keygen` executable. Resolving to the established `Signer`
interface preserves existing custom signer and object-signer plugin behaviour.
Passphrases are obtained through a caller callback only when needed and are
cleared before return.

## Non-goals

This does not verify SSH or X.509 signatures, discover private keys by key id,
or open an SSH-agent connection implicitly. Those choices would require policy
and lifecycle decisions outside a repository-scoped resolver.
