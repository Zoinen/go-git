package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/go-git/go-git/v6/storage/memory"
)

func TestLFSPointerRoundTrip(t *testing.T) {
	t.Parallel()
	p := LFSPointer{OID: "0000000000000000000000000000000000000000000000000000000000000000", Size: 42}
	text, err := p.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLFSPointer(text)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("pointer = %#v, want %#v", got, p)
	}

	for _, malformed := range [][]byte{
		[]byte("version https://git-lfs.github.com/spec/v1\noid sha256:" + p.OID + "\nsize 042\n"),
		[]byte("version https://git-lfs.github.com/spec/v1\noid sha256:" + p.OID + "\nsize +42\n"),
		[]byte("version https://git-lfs.github.com/spec/v1\noid sha256:" + p.OID + "\nsize 42\nextra\n"),
		[]byte("version https://git-lfs.github.com/spec/v1\noid sha256:" + p.OID + "\nsize 42"),
	} {
		if _, err := ParseLFSPointer(malformed); !errors.Is(err, ErrInvalidLFSPointer) {
			t.Fatalf("ParseLFSPointer(%q) error = %v, want ErrInvalidLFSPointer", malformed, err)
		}
	}
}

func TestLFSLocalStoreCleanSmudgeAndVerify(t *testing.T) {
	t.Parallel()
	repo := lfsTestRepository(t)
	client, err := repo.LFS()
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("content that does not fit in a pointer\n"), 4096)
	p, err := client.CleanContext(context.Background(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	wantOID := sha256.Sum256(content)
	if p.OID != stringHex(wantOID[:]) || p.Size != int64(len(content)) {
		t.Fatalf("pointer = %#v", p)
	}
	has, err := client.HasContext(context.Background(), p)
	if err != nil || !has {
		t.Fatalf("HasContext = %v, %v", has, err)
	}
	var smudged bytes.Buffer
	if err := client.SmudgeContext(context.Background(), p, &smudged); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(smudged.Bytes(), content) {
		t.Fatal("smudged content differs")
	}
	if err := client.VerifyContext(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	name, err := client.ObjectPath(p.OID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.filesystem.Stat(name); err != nil {
		t.Fatalf("local object is not in the git-lfs layout: %v", err)
	}
}

func TestLFSStoreRejectsMismatchedAndCancelledInput(t *testing.T) {
	t.Parallel()
	repo := lfsTestRepository(t)
	client, err := repo.LFS()
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("expected content")
	digest := sha256.Sum256(content)
	p := LFSPointer{OID: stringHex(digest[:]), Size: int64(len(content))}
	if err := client.StoreContext(context.Background(), p, bytes.NewReader([]byte("wrong"))); !errors.Is(err, ErrLFSObjectIntegrity) {
		t.Fatalf("StoreContext mismatch error = %v", err)
	}
	has, err := client.HasContext(context.Background(), p)
	if err != nil || has {
		t.Fatalf("corrupt object was published: has=%v err=%v", has, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.CleanContext(ctx, bytes.NewReader(content)); !errors.Is(err, context.Canceled) {
		t.Fatalf("CleanContext cancellation error = %v", err)
	}
}

func TestLFSVerifyDetectsCorruption(t *testing.T) {
	t.Parallel()
	repo := lfsTestRepository(t)
	client, err := repo.LFS()
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("known good object")
	p, err := client.CleanContext(context.Background(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	name, err := client.ObjectPath(p.OID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := client.filesystem.OpenFile(name, 0o2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("!"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyContext(context.Background(), p); !errors.Is(err, ErrLFSObjectIntegrity) {
		t.Fatalf("VerifyContext corruption error = %v", err)
	}
}

func TestLFSRepositoryRequiresFilesystemStorage(t *testing.T) {
	t.Parallel()
	repo, err := Init(memory.NewStorage())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LFS(); !errors.Is(err, ErrLFSStorageUnsupported) {
		t.Fatalf("LFS error = %v, want ErrLFSStorageUnsupported", err)
	}
}

func lfsTestRepository(t *testing.T) *Repository {
	t.Helper()
	dir := t.TempDir()
	repo, err := PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func stringHex(value []byte) string {
	const hex = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for i, b := range value {
		result[i*2] = hex[b>>4]
		result[i*2+1] = hex[b&0x0f]
	}
	return string(result)
}
