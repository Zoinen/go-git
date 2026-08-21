package git

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"golang.org/x/crypto/ssh"

	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/storage/memory"
)

func TestOpenPGPFileSignerSignsDetachedSignature(t *testing.T) {
	entity, err := openpgp.NewEntity("Test Signer", "", "signer@example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	var privateKey bytes.Buffer
	if err := entity.SerializePrivate(&privateKey, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signing.key")
	if err := os.WriteFile(path, privateKey.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	signer, err := NewOpenPGPFileSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("tree deadbeef\n\ncommit\n")
	signature, err := signer.Sign(context.Background(), bytes.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(openpgp.EntityList{entity}, bytes.NewReader(message), bytes.NewReader(signature), nil); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func TestSSHFileSignerProducesVerifiableGitSSHSig(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	signer, err := NewSSHFileSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("tree deadbeef\n\ncommit\n")
	armored, err := signer.Sign(context.Background(), bytes.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	public, namespace, hashAlgorithm, signature := decodeSSHSig(t, armored)
	if namespace != "git" || hashAlgorithm != "sha512" {
		t.Fatalf("namespace/hash = %q/%q, want git/sha512", namespace, hashAlgorithm)
	}
	expectedHash := sha512.Sum512(message)
	if err := public.Verify(sshsigSignedData(namespace, expectedHash[:]), signature); err != nil {
		t.Fatalf("SSHSIG signature does not verify: %v", err)
	}
}

func TestResolveCommitSignerRejectsX509(t *testing.T) {
	repo, err := Init(memory.NewStorage(), WithWorkTree(memfs.New()))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	cfg := config.NewConfig()
	cfg.GPG.Format = "x509"
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	_, err = repo.ResolveCommitSigner(context.Background(), nil)
	if !errors.Is(err, ErrX509SigningUnsupported) {
		t.Fatalf("ResolveCommitSigner error = %v, want ErrX509SigningUnsupported", err)
	}
}

func TestOpenPGPFileSignerHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewOpenPGPFileSignerContext(ctx, "not-read", nil)
	if err != context.Canceled {
		t.Fatalf("NewOpenPGPFileSignerContext error = %v, want context.Canceled", err)
	}
}

func decodeSSHSig(t *testing.T, armored []byte) (ssh.PublicKey, string, string, *ssh.Signature) {
	t.Helper()
	block, rest := pem.Decode(armored)
	if block == nil || block.Type != "SSH SIGNATURE" || len(rest) != 0 {
		t.Fatal("invalid SSH signature PEM")
	}
	b := block.Bytes
	if len(b) < len("SSHSIG") || string(b[:len("SSHSIG")]) != "SSHSIG" {
		t.Fatal("invalid SSHSIG magic")
	}
	b = b[len("SSHSIG"):]
	if takeUint32(t, &b) != 1 {
		t.Fatal("invalid SSHSIG header")
	}
	public, err := ssh.ParsePublicKey(takeSSHString(t, &b))
	if err != nil {
		t.Fatal(err)
	}
	namespace := string(takeSSHString(t, &b))
	_ = takeSSHString(t, &b) // reserved
	hashAlgorithm := string(takeSSHString(t, &b))
	rawSignature := takeSSHString(t, &b)
	if len(b) != 0 {
		t.Fatal("trailing SSHSIG data")
	}
	if len(rawSignature) == 0 {
		t.Fatal("empty SSH signature")
	}
	format := string(takeSSHString(t, &rawSignature))
	if len(rawSignature) == 0 {
		t.Fatalf("SSH signature %q has no blob", format)
	}
	blob := takeSSHString(t, &rawSignature)
	if len(rawSignature) != 0 {
		t.Fatal("trailing SSH signature data")
	}
	return public, namespace, hashAlgorithm, &ssh.Signature{Format: format, Blob: blob}
}

func takeSSHString(t *testing.T, data *[]byte) []byte {
	t.Helper()
	length := takeUint32(t, data)
	if uint64(length) > uint64(len(*data)) {
		t.Fatal("truncated SSH string")
	}
	value := (*data)[:length]
	*data = (*data)[length:]
	return value
}

func takeUint32(t *testing.T, data *[]byte) uint32 {
	t.Helper()
	if len(*data) < 4 {
		t.Fatalf("truncated uint32 (%d bytes remain)", len(*data))
	}
	value := binary.BigEndian.Uint32((*data)[:4])
	*data = (*data)[4:]
	return value
}
