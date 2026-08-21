package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-git/go-billy/v6"
)

// LFS errors.
var (
	// ErrLFSStorageUnsupported is returned when the repository storage does not
	// expose a filesystem suitable for the local LFS object store.
	ErrLFSStorageUnsupported = errors.New("git lfs local storage is unsupported by this repository")
	// ErrInvalidLFSPointer is returned when a value is not a canonical Git LFS
	// v1 pointer.
	ErrInvalidLFSPointer = errors.New("invalid git lfs pointer")
	// ErrLFSObjectNotFound is returned when an LFS object is absent locally.
	ErrLFSObjectNotFound = errors.New("git lfs object not found")
	// ErrLFSObjectIntegrity is returned when an LFS object's bytes do not match
	// its oid or declared size.
	ErrLFSObjectIntegrity = errors.New("git lfs object integrity check failed")
)

const lfsPointerVersion = "https://git-lfs.github.com/spec/v1"

// LFSPointer is the canonical textual representation of a Git LFS object.
// OID is the lowercase hexadecimal SHA-256 digest, without the "sha256:"
// prefix. Size is measured in bytes.
type LFSPointer struct {
	OID  string
	Size int64
}

// Validate checks that p can be represented as a canonical LFS v1 pointer.
func (p LFSPointer) Validate() error {
	if len(p.OID) != sha256.Size*2 {
		return fmt.Errorf("%w: sha256 oid must contain %d hex characters", ErrInvalidLFSPointer, sha256.Size*2)
	}
	if strings.ToLower(p.OID) != p.OID {
		return fmt.Errorf("%w: oid must use lowercase hexadecimal", ErrInvalidLFSPointer)
	}
	if _, err := hex.DecodeString(p.OID); err != nil {
		return fmt.Errorf("%w: malformed sha256 oid: %v", ErrInvalidLFSPointer, err)
	}
	if p.Size < 0 {
		return fmt.Errorf("%w: negative object size", ErrInvalidLFSPointer)
	}
	return nil
}

// MarshalText formats p as a canonical, three-line Git LFS v1 pointer.
func (p LFSPointer) MarshalText() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return []byte("version " + lfsPointerVersion + "\n" +
		"oid sha256:" + p.OID + "\n" +
		"size " + strconv.FormatInt(p.Size, 10) + "\n"), nil
}

// ParseLFSPointer parses a canonical Git LFS v1 pointer. Extension lines,
// whitespace variations and non-canonical encodings are rejected deliberately:
// callers can use a successful parse as an unambiguous signal that a blob is an
// LFS pointer rather than ordinary text.
func ParseLFSPointer(data []byte) (LFSPointer, error) {
	if len(data) == 0 || data[len(data)-1] != '\n' || strings.Contains(string(data), "\r") {
		return LFSPointer{}, fmt.Errorf("%w: pointer must use LF-terminated lines", ErrInvalidLFSPointer)
	}
	lines := strings.Split(string(data[:len(data)-1]), "\n")
	if len(lines) != 3 || lines[0] != "version "+lfsPointerVersion || !strings.HasPrefix(lines[1], "oid sha256:") || !strings.HasPrefix(lines[2], "size ") {
		return LFSPointer{}, fmt.Errorf("%w: expected canonical v1 fields", ErrInvalidLFSPointer)
	}
	if len(lines[1]) != len("oid sha256:")+sha256.Size*2 {
		return LFSPointer{}, fmt.Errorf("%w: malformed oid", ErrInvalidLFSPointer)
	}
	sizeText := strings.TrimPrefix(lines[2], "size ")
	if len(sizeText) == 0 || (len(sizeText) > 1 && sizeText[0] == '0') {
		return LFSPointer{}, fmt.Errorf("%w: non-canonical size", ErrInvalidLFSPointer)
	}
	for _, character := range sizeText {
		if character < '0' || character > '9' {
			return LFSPointer{}, fmt.Errorf("%w: malformed size", ErrInvalidLFSPointer)
		}
	}
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil {
		return LFSPointer{}, fmt.Errorf("%w: malformed size: %v", ErrInvalidLFSPointer, err)
	}
	p := LFSPointer{OID: strings.TrimPrefix(lines[1], "oid sha256:"), Size: size}
	if err := p.Validate(); err != nil {
		return LFSPointer{}, err
	}
	return p, nil
}

// LFS returns a client backed by this repository's local .git/lfs object
// store. It does not invoke git-lfs or git. Remote transfer is intentionally
// not performed by this API.
func (r *Repository) LFS() (*LFSClient, error) {
	type filesystemStorage interface {
		Filesystem() billy.Filesystem
	}
	storage, ok := r.Storer.(filesystemStorage)
	if !ok {
		return nil, ErrLFSStorageUnsupported
	}
	return &LFSClient{filesystem: storage.Filesystem()}, nil
}

// LFSClient manages the repository-local Git LFS object store. The store is
// located at lfs/objects/aa/bb/<sha256>, as specified by Git LFS.
type LFSClient struct {
	filesystem billy.Filesystem
	temporary  atomic.Uint64
}

// ObjectPath returns the filesystem-relative location of oid in the local LFS
// store. oid must be a lowercase SHA-256 hexadecimal digest.
func (c *LFSClient) ObjectPath(oid string) (string, error) {
	if err := (LFSPointer{OID: oid, Size: 0}).Validate(); err != nil {
		return "", err
	}
	return path.Join("lfs", "objects", oid[:2], oid[2:4], oid), nil
}

// HasContext reports whether p's local object exists. It does not trust file
// metadata as proof of content integrity; use VerifyContext when that matters.
func (c *LFSClient) HasContext(ctx context.Context, p LFSPointer) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := p.Validate(); err != nil {
		return false, err
	}
	name, err := c.ObjectPath(p.OID)
	if err != nil {
		return false, err
	}
	info, err := c.filesystem.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !info.IsDir() && info.Size() == p.Size, nil
}

// OpenContext opens p from the local LFS object store. The returned reader
// observes ctx between Read calls. VerifyContext or SmudgeContext verifies the
// digest; OpenContext is deliberately inexpensive.
func (c *LFSClient) OpenContext(ctx context.Context, p LFSPointer) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	name, err := c.ObjectPath(p.OID)
	if err != nil {
		return nil, err
	}
	f, err := c.filesystem.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrLFSObjectNotFound, p.OID)
	}
	if err != nil {
		return nil, err
	}
	return &lfsContextReadCloser{ctx: ctx, ReadCloser: f}, nil
}

// StoreContext copies source into the local LFS store after validating that it
// exactly matches p. Publication uses a same-directory temporary file followed
// by Rename, so cancelled or corrupt input never replaces a completed object.
func (c *LFSClient) StoreContext(ctx context.Context, p LFSPointer, source io.Reader) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	name, err := c.ObjectPath(p.OID)
	if err != nil {
		return err
	}
	if exists, err := c.HasContext(ctx, p); err != nil {
		return err
	} else if exists {
		return c.VerifyContext(ctx, p)
	}

	dir := path.Dir(name)
	if err := c.filesystem.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temporary := path.Join(dir, fmt.Sprintf(".%s.tmp-%d", p.OID, c.temporary.Add(1)))
	f, err := c.filesystem.Create(temporary)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = c.filesystem.Remove(temporary)
	}()

	hash := sha256.New()
	buf := make([]byte, 32*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := source.Read(buf)
		if n > 0 {
			written, writeErr := f.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			_, _ = hash.Write(buf[:n])
			size += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if size != p.Size || hex.EncodeToString(hash.Sum(nil)) != p.OID {
		return fmt.Errorf("%w: expected %s/%d, got %s/%d", ErrLFSObjectIntegrity, p.OID, p.Size, hex.EncodeToString(hash.Sum(nil)), size)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := c.filesystem.Rename(temporary, name); err != nil {
		// A concurrent writer might have won. It is safe to accept only a fully
		// verified object, never an arbitrary pre-existing file.
		if verifyErr := c.VerifyContext(ctx, p); verifyErr == nil {
			return nil
		}
		return err
	}
	return nil
}

// CleanContext stores source and returns the pointer that represents it.
func (c *LFSClient) CleanContext(ctx context.Context, source io.Reader) (LFSPointer, error) {
	if err := ctx.Err(); err != nil {
		return LFSPointer{}, err
	}
	if err := c.filesystem.MkdirAll(path.Join("lfs", "incomplete"), 0o755); err != nil {
		return LFSPointer{}, err
	}
	temporary := path.Join("lfs", "incomplete", fmt.Sprintf(".clean.tmp-%d", c.temporary.Add(1)))
	f, err := c.filesystem.Create(temporary)
	if err != nil {
		return LFSPointer{}, err
	}
	defer func() {
		_ = f.Close()
		_ = c.filesystem.Remove(temporary)
	}()
	buf := make([]byte, 32*1024)
	hash := sha256.New()
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return LFSPointer{}, err
		}
		n, err := source.Read(buf)
		if n > 0 {
			written, writeErr := f.Write(buf[:n])
			if writeErr != nil {
				return LFSPointer{}, writeErr
			}
			if written != n {
				return LFSPointer{}, io.ErrShortWrite
			}
			_, _ = hash.Write(buf[:n])
			size += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return LFSPointer{}, err
		}
	}
	p := LFSPointer{OID: hex.EncodeToString(hash.Sum(nil)), Size: size}
	if err := f.Close(); err != nil {
		return LFSPointer{}, err
	}
	name, err := c.ObjectPath(p.OID)
	if err != nil {
		return LFSPointer{}, err
	}
	if exists, err := c.HasContext(ctx, p); err != nil {
		return LFSPointer{}, err
	} else if exists {
		return p, c.VerifyContext(ctx, p)
	}
	if err := c.filesystem.MkdirAll(path.Dir(name), 0o755); err != nil {
		return LFSPointer{}, err
	}
	if err := c.filesystem.Rename(temporary, name); err != nil {
		if verifyErr := c.VerifyContext(ctx, p); verifyErr == nil {
			return p, nil
		}
		return LFSPointer{}, err
	}
	return p, nil
}

// SmudgeContext verifies and writes p's local object to destination.
func (c *LFSClient) SmudgeContext(ctx context.Context, p LFSPointer, destination io.Writer) error {
	r, err := c.OpenContext(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	hash := sha256.New()
	buf := make([]byte, 32*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			written, writeErr := destination.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			_, _ = hash.Write(buf[:n])
			size += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if size != p.Size || hex.EncodeToString(hash.Sum(nil)) != p.OID {
		return fmt.Errorf("%w: expected %s/%d", ErrLFSObjectIntegrity, p.OID, p.Size)
	}
	return nil
}

// VerifyContext checks the complete local object against p.
func (c *LFSClient) VerifyContext(ctx context.Context, p LFSPointer) error {
	return c.SmudgeContext(ctx, p, io.Discard)
}

type lfsContextReadCloser struct {
	ctx context.Context
	io.ReadCloser
}

func (r *lfsContextReadCloser) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadCloser.Read(p)
}
