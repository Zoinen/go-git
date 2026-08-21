package git

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/go-git/go-git/v6/config"
)

var (
	// ErrSigningFormatUnsupported is returned when gpg.format names a format
	// that this package cannot create.
	ErrSigningFormatUnsupported = errors.New("commit signing format is unsupported")
	// ErrX509SigningUnsupported is returned for gpg.format=x509. go-git can
	// preserve X.509 signatures in objects, but does not create them.
	ErrX509SigningUnsupported = errors.New("X.509 commit signing is unsupported")
)

// SigningKeyResolverOptions controls [Repository.ResolveCommitSigner]. A
// passphrase callback is intentionally called only for an encrypted key and
// receives the caller's context.
type SigningKeyResolverOptions struct {
	// KeyPath overrides user.signingKey. It must name a private-key file except
	// when SSHAgent and SSHAgentPublicKey are supplied.
	KeyPath string
	// Passphrase obtains a private key's passphrase. It is never retained after
	// the resolver returns.
	Passphrase func(context.Context) ([]byte, error)
	// SSHAgent and SSHAgentPublicKey select an SSH key already loaded in an
	// agent. Both values are required together.
	SSHAgent          agent.Agent
	SSHAgentPublicKey ssh.PublicKey
	// SSHNamespace is the sshsig namespace. Git uses "git" when it is empty.
	SSHNamespace string
}

// ResolveCommitSigner resolves the signing key configured by gpg.format and
// user.signingKey. It never invokes gpg, ssh-keygen, or any other process.
//
// The supported formats are openpgp (the Git default) and ssh. X.509 returns
// [ErrX509SigningUnsupported]. For SSH, a private key file is used unless the
// caller explicitly provides an agent and its selected public key.
func (r *Repository) ResolveCommitSigner(ctx context.Context, options *SigningKeyResolverOptions) (Signer, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}

	cfg, err := r.ConfigScoped(config.SystemScope)
	if err != nil {
		return nil, fmt.Errorf("read signing configuration: %w", err)
	}
	if options == nil {
		options = &SigningKeyResolverOptions{}
	}

	format := strings.ToLower(strings.TrimSpace(cfg.GPG.Format))
	if format == "" {
		format = "openpgp"
	}
	keyPath := options.KeyPath
	if keyPath == "" {
		keyPath = cfg.User.SigningKey
	}

	switch format {
	case "openpgp":
		if keyPath == "" {
			return nil, errors.New("openpgp signing requires user.signingKey or SigningKeyResolverOptions.KeyPath")
		}
		return NewOpenPGPFileSignerContext(ctx, keyPath, options.Passphrase)
	case "ssh":
		if options.SSHAgent != nil || options.SSHAgentPublicKey != nil {
			if options.SSHAgent == nil || options.SSHAgentPublicKey == nil {
				return nil, errors.New("SSH agent signing requires both SSHAgent and SSHAgentPublicKey")
			}
			return NewSSHAgentSigner(options.SSHAgent, options.SSHAgentPublicKey, options.SSHNamespace), nil
		}
		if keyPath == "" {
			return nil, errors.New("SSH signing requires user.signingKey or SigningKeyResolverOptions.KeyPath")
		}
		return NewSSHFileSignerContext(ctx, keyPath, options.Passphrase, options.SSHNamespace)
	case "x509":
		return nil, ErrX509SigningUnsupported
	default:
		return nil, fmt.Errorf("%w: %q", ErrSigningFormatUnsupported, cfg.GPG.Format)
	}
}

// NewOpenPGPFileSigner loads an unencrypted OpenPGP private-key file.
func NewOpenPGPFileSigner(path string) (Signer, error) {
	return NewOpenPGPFileSignerContext(context.Background(), path, nil)
}

// NewOpenPGPFileSignerContext loads an OpenPGP private-key file. Both armored
// and binary keyrings are accepted.
func NewOpenPGPFileSignerContext(ctx context.Context, path string, passphrase func(context.Context) ([]byte, error)) (Signer, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(expandSigningKeyPath(path))
	if err != nil {
		return nil, fmt.Errorf("read OpenPGP signing key: %w", err)
	}
	entities, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(data))
	if err != nil {
		entities, err = openpgp.ReadKeyRing(bytes.NewReader(data))
	}
	if err != nil {
		return nil, fmt.Errorf("parse OpenPGP signing key: %w", err)
	}
	if len(entities) == 0 {
		return nil, errors.New("OpenPGP signing keyring is empty")
	}
	entity := entities[0]
	if err := unlockOpenPGPEntity(ctx, entity, passphrase); err != nil {
		return nil, err
	}
	return openPGPFileSigner{entity: entity}, nil
}

type openPGPFileSigner struct{ entity *openpgp.Entity }

func (s openPGPFileSigner) Sign(ctx context.Context, message io.Reader) ([]byte, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	var signature bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&signature, s.entity, message, nil); err != nil {
		return nil, fmt.Errorf("sign OpenPGP object: %w", err)
	}
	return signature.Bytes(), nil
}

func unlockOpenPGPEntity(ctx context.Context, entity *openpgp.Entity, passphrase func(context.Context) ([]byte, error)) error {
	var encrypted bool
	if entity.PrivateKey != nil && entity.PrivateKey.Encrypted {
		encrypted = true
	}
	for _, subkey := range entity.Subkeys {
		if subkey.PrivateKey != nil && subkey.PrivateKey.Encrypted {
			encrypted = true
		}
	}
	if !encrypted {
		return nil
	}
	if passphrase == nil {
		return errors.New("OpenPGP signing key is encrypted and no passphrase callback was supplied")
	}
	phrase, err := passphrase(ctx)
	if err != nil {
		return fmt.Errorf("read OpenPGP signing-key passphrase: %w", err)
	}
	defer clearBytes(phrase)
	if entity.PrivateKey != nil && entity.PrivateKey.Encrypted {
		if err := entity.PrivateKey.Decrypt(phrase); err != nil {
			return fmt.Errorf("decrypt OpenPGP signing key: %w", err)
		}
	}
	for _, subkey := range entity.Subkeys {
		if subkey.PrivateKey != nil && subkey.PrivateKey.Encrypted {
			if err := subkey.PrivateKey.Decrypt(phrase); err != nil {
				return fmt.Errorf("decrypt OpenPGP signing subkey: %w", err)
			}
		}
	}
	return nil
}

// NewSSHFileSigner loads an unencrypted SSH private key and creates Git's
// armored sshsig signatures using the "git" namespace.
func NewSSHFileSigner(path string) (Signer, error) {
	return NewSSHFileSignerContext(context.Background(), path, nil, "git")
}

// NewSSHFileSignerContext loads an SSH private key and creates Git sshsig
// signatures. It supports encrypted OpenSSH/PEM keys with a passphrase
// callback, without starting ssh-keygen.
func NewSSHFileSignerContext(ctx context.Context, path string, passphrase func(context.Context) ([]byte, error), namespace string) (Signer, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	key, err := os.ReadFile(expandSigningKeyPath(path))
	if err != nil {
		return nil, fmt.Errorf("read SSH signing key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	var passphraseMissing *ssh.PassphraseMissingError
	if errors.As(err, &passphraseMissing) {
		if passphrase == nil {
			return nil, errors.New("SSH signing key is encrypted and no passphrase callback was supplied")
		}
		phrase, phraseErr := passphrase(ctx)
		if phraseErr != nil {
			return nil, fmt.Errorf("read SSH signing-key passphrase: %w", phraseErr)
		}
		defer clearBytes(phrase)
		signer, err = ssh.ParsePrivateKeyWithPassphrase(key, phrase)
	}
	if err != nil {
		return nil, fmt.Errorf("parse SSH signing key: %w", err)
	}
	return sshSignatureSigner{signer: signer, namespace: sshNamespace(namespace)}, nil
}

// NewSSHAgentSigner creates a Git sshsig signer around an already connected
// SSH agent. The caller owns the agent connection and is responsible for
// closing it when appropriate.
func NewSSHAgentSigner(client agent.Agent, key ssh.PublicKey, namespace string) Signer {
	return sshSignatureSigner{agent: client, publicKey: key, namespace: sshNamespace(namespace)}
}

type sshSignatureSigner struct {
	signer    ssh.Signer
	agent     agent.Agent
	publicKey ssh.PublicKey
	namespace string
}

func (s sshSignatureSigner) Sign(ctx context.Context, message io.Reader) ([]byte, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	contents, err := io.ReadAll(message)
	if err != nil {
		return nil, err
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	hash := sha512.Sum512(contents)
	signed := sshsigSignedData(s.namespace, hash[:])
	var sig *ssh.Signature
	var public ssh.PublicKey
	if s.signer != nil {
		public = s.signer.PublicKey()
		sig, err = s.signer.Sign(rand.Reader, signed)
	} else if s.agent != nil && s.publicKey != nil {
		public = s.publicKey
		sig, err = s.agent.Sign(public, signed)
	} else {
		return nil, errors.New("SSH signer has no private key or agent identity")
	}
	if err != nil {
		return nil, fmt.Errorf("sign SSH object: %w", err)
	}

	encoded := append([]byte(nil), "SSHSIG"...)
	encoded = appendUint32(encoded, 1)
	encoded = appendSSHString(encoded, public.Marshal())
	encoded = appendSSHString(encoded, []byte(s.namespace))
	encoded = appendSSHString(encoded, nil)
	encoded = appendSSHString(encoded, []byte("sha512"))
	encoded = appendSSHString(encoded, marshalSSHSignature(sig))
	block := &pem.Block{Type: "SSH SIGNATURE", Bytes: encoded}
	return pem.EncodeToMemory(block), nil
}

func sshsigSignedData(namespace string, hash []byte) []byte {
	b := append([]byte(nil), "SSHSIG"...)
	b = appendUint32(b, 1)
	b = appendSSHString(b, []byte(namespace))
	b = appendSSHString(b, nil)
	b = appendSSHString(b, []byte("sha512"))
	return appendSSHString(b, hash)
}

func marshalSSHSignature(sig *ssh.Signature) []byte {
	b := appendSSHString(nil, []byte(sig.Format))
	return appendSSHString(b, sig.Blob)
}

func appendSSHString(dst, value []byte) []byte {
	dst = appendUint32(dst, uint32(len(value)))
	return append(dst, value...)
}

func appendUint32(dst []byte, value uint32) []byte {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], value)
	return append(dst, raw[:]...)
}

func sshNamespace(namespace string) string {
	if namespace == "" {
		return "git"
	}
	return namespace
}

func expandSigningKeyPath(path string) string {
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	return os.ExpandEnv(path)
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
