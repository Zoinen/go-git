package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/internal/pathutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v6/plumbing/format/diff"
	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/utils/binary"
	"github.com/go-git/go-git/v6/utils/convert"
	gitdiff "github.com/go-git/go-git/v6/utils/diff"
)

// ErrDiffUnmerged is returned when a requested path has unmerged index
// entries. A unified diff has no unambiguous base or destination in that
// state.
var ErrDiffUnmerged = errors.New("cannot diff an unmerged index entry")

// DiffOptions controls Worktree.DiffContext.
type DiffOptions struct {
	// Staged compares HEAD with the index. When false, DiffContext compares
	// the index with the working tree.
	Staged bool

	// Paths restricts the diff to the supplied worktree-relative paths. A
	// directory includes all of its descendants. An empty list includes all
	// changed paths.
	Paths []string

	// ContextLines is the number of unchanged lines emitted around each hunk.
	// Zero uses the standard three context lines.
	ContextLines int

	// IncludeUntracked includes untracked working-tree files in an unstaged
	// diff. It has no effect for staged diffs.
	IncludeUntracked bool
}

// DiffContext returns a unified, text diff for either the staged or unstaged
// changes in the worktree. It never invokes the git executable.
//
// Binary files are represented by the usual unified-diff binary marker and
// have no hunks. Unmerged paths return ErrDiffUnmerged. Context cancellation
// is checked between filesystem, object-store, and per-file diff operations;
// a nil context is treated as context.Background.
func (w *Worktree) DiffContext(ctx context.Context, o DiffOptions) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if o.ContextLines < 0 {
		return "", fmt.Errorf("diff context lines must not be negative")
	}

	paths, err := diffPathFilters(o.Paths)
	if err != nil {
		return "", err
	}

	status, err := w.StatusContext(ctx, StatusOptions{})
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	cfg, err := w.r.Config()
	if err != nil {
		return "", err
	}
	idx, err := w.r.Storer.Index()
	if err != nil {
		return "", err
	}
	head, err := w.headTree()
	if err != nil {
		return "", err
	}

	names := make([]string, 0, len(status))
	for name, fileStatus := range status {
		if fileStatus == nil || !diffPathIncluded(paths, name) {
			continue
		}
		if !diffStatusIncluded(fileStatus, o) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	patches := make([]fdiff.FilePatch, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if diffIndexPathUnmerged(idx, name) {
			return "", fmt.Errorf("%w: %s", ErrDiffUnmerged, name)
		}

		var from, to *worktreeDiffFile
		if o.Staged {
			from, err = w.diffTreeFileContext(ctx, head, name)
			if err == nil {
				to, err = w.diffIndexFileContext(ctx, idx, name)
			}
		} else {
			from, err = w.diffIndexFileContext(ctx, idx, name)
			if err == nil {
				to, err = w.diffWorktreeFileContext(ctx, cfg, name)
			}
		}
		if err != nil {
			return "", err
		}
		if from == nil && to == nil {
			continue
		}

		patch, err := newWorktreeDiffFilePatch(ctx, from, to)
		if err != nil {
			return "", err
		}
		patches = append(patches, patch)
	}

	contextLines := o.ContextLines
	if contextLines == 0 {
		contextLines = fdiff.DefaultContextLines
	}

	var out strings.Builder
	if err := fdiff.NewUnifiedEncoder(&out, contextLines).Encode(worktreeDiffPatch{files: patches}); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func diffPathFilters(paths []string) ([]string, error) {
	filters := make([]string, 0, len(paths))
	for _, name := range paths {
		name = filepath.ToSlash(filepath.Clean(name))
		if name == "." {
			return nil, nil
		}
		if err := pathutil.ValidTreePath(name); err != nil {
			return nil, fmt.Errorf("invalid diff path %q: %w", name, err)
		}
		filters = append(filters, name)
	}
	return filters, nil
}

func diffPathIncluded(filters []string, name string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, filter := range filters {
		if name == filter || strings.HasPrefix(name, filter+"/") {
			return true
		}
	}
	return false
}

func diffStatusIncluded(status *FileStatus, o DiffOptions) bool {
	if o.Staged {
		switch status.Staging {
		case Unmodified, Untracked, Ignored:
			return false
		default:
			return true
		}
	}

	switch status.Worktree {
	case Unmodified, Ignored:
		return false
	case Untracked:
		return o.IncludeUntracked
	default:
		return true
	}
}

func diffIndexPathUnmerged(idx *index.Index, name string) bool {
	for _, entry := range idx.Entries {
		if entry.Name == name && entry.Stage != 0 {
			return true
		}
	}
	return false
}

func (w *Worktree) diffTreeFileContext(ctx context.Context, tree *object.Tree, name string) (*worktreeDiffFile, error) {
	if tree == nil {
		return nil, nil
	}
	entry, err := tree.FindEntry(name)
	if err != nil {
		if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return w.diffObjectFileContext(ctx, name, entry.Mode, entry.Hash)
}

func (w *Worktree) diffIndexFileContext(ctx context.Context, idx *index.Index, name string) (*worktreeDiffFile, error) {
	for _, entry := range idx.Entries {
		if entry.Name != name || entry.Stage != 0 {
			continue
		}
		if entry.Hash.IsZero() {
			return nil, nil
		}
		return w.diffObjectFileContext(ctx, name, entry.Mode, entry.Hash)
	}
	return nil, nil
}

func (w *Worktree) diffObjectFileContext(ctx context.Context, name string, mode filemode.FileMode, hash plumbing.Hash) (*worktreeDiffFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mode == filemode.Submodule {
		return &worktreeDiffFile{
			path: name,
			hash: hash,
			mode: mode,
			text: fmt.Sprintf("Subproject commit %s\n", hash),
		}, nil
	}
	if !mode.IsFile() {
		return nil, fmt.Errorf("cannot diff non-file path %q with mode %s", name, mode)
	}

	blob, err := object.GetBlob(w.r.Storer, hash)
	if err != nil {
		return nil, err
	}
	reader, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	data, readErr := readDiffContext(ctx, reader)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}

	isBinary, err := binary.IsBinary(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return &worktreeDiffFile{
		path:   name,
		hash:   hash,
		mode:   mode,
		text:   string(data),
		binary: isBinary,
	}, nil
}

func (w *Worktree) diffWorktreeFileContext(ctx context.Context, cfg *config.Config, name string) (*worktreeDiffFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := w.filesystem.Lstat(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	mode, err := filemode.NewFromOSFileMode(info.Mode())
	if err != nil {
		return nil, err
	}
	if !mode.IsFile() {
		return nil, fmt.Errorf("cannot diff non-file path %q", name)
	}

	var data []byte
	if mode == filemode.Symlink {
		target, err := w.filesystem.Readlink(name)
		if err != nil {
			return nil, err
		}
		data = []byte(target)
	} else {
		file, err := w.filesystem.Open(name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		data, err = readDiffContext(ctx, file)
		closeErr := file.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}

	if mode != filemode.Symlink {
		data, err = normalizeDiffWorktreeContent(cfg, data)
		if err != nil {
			return nil, err
		}
	}
	isBinary, err := binary.IsBinary(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	hash, err := diffBlobHash(w.r.Storer, data)
	if err != nil {
		return nil, err
	}

	return &worktreeDiffFile{
		path:   name,
		hash:   hash,
		mode:   mode,
		text:   string(data),
		binary: isBinary,
	}, nil
}

func normalizeDiffWorktreeContent(cfg *config.Config, data []byte) ([]byte, error) {
	if cfg == nil || (cfg.Core.AutoCRLF != "true" && cfg.Core.AutoCRLF != "input") {
		return data, nil
	}
	stat, err := convert.GetStat(bytes.NewReader(data))
	if err != nil || stat.IsBinary() {
		return data, err
	}

	var normalized bytes.Buffer
	if _, err := convert.NewLFWriter(&normalized).Write(data); err != nil {
		return nil, err
	}
	return normalized.Bytes(), nil
}

func diffBlobHash(s storer.EncodedObjectStorer, data []byte) (plumbing.Hash, error) {
	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(data)))
	writer, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return plumbing.ZeroHash, err
	}
	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return obj.Hash(), nil
}

func readDiffContext(ctx context.Context, reader io.Reader) ([]byte, error) {
	const bufferSize = 32 * 1024
	var out bytes.Buffer
	buffer := make([]byte, bufferSize)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			_, _ = out.Write(buffer[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out.Bytes(), nil
			}
			return nil, err
		}
		if n == 0 {
			return out.Bytes(), nil
		}
	}
}

type worktreeDiffPatch struct {
	files []fdiff.FilePatch
}

func (p worktreeDiffPatch) FilePatches() []fdiff.FilePatch { return p.files }
func (worktreeDiffPatch) Message() string                  { return "" }

type worktreeDiffFile struct {
	path   string
	hash   plumbing.Hash
	mode   filemode.FileMode
	text   string
	binary bool
}

func (f *worktreeDiffFile) Hash() plumbing.Hash     { return f.hash }
func (f *worktreeDiffFile) Mode() filemode.FileMode { return f.mode }
func (f *worktreeDiffFile) Path() string            { return f.path }

type worktreeDiffFilePatch struct {
	from, to *worktreeDiffFile
	chunks   []fdiff.Chunk
	binary   bool
}

func newWorktreeDiffFilePatch(ctx context.Context, from, to *worktreeDiffFile) (*worktreeDiffFilePatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	patch := &worktreeDiffFilePatch{from: from, to: to}
	if (from != nil && from.binary) || (to != nil && to.binary) {
		patch.binary = true
		return patch, nil
	}

	var fromText, toText string
	if from != nil {
		fromText = from.text
	}
	if to != nil {
		toText = to.text
	}
	diffs := gitdiff.Do(fromText, toText)
	patch.chunks = make([]fdiff.Chunk, 0, len(diffs))
	for _, diff := range diffs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var operation fdiff.Operation
		switch diff.Type {
		case diffmatchpatch.DiffEqual:
			operation = fdiff.Equal
		case diffmatchpatch.DiffDelete:
			operation = fdiff.Delete
		case diffmatchpatch.DiffInsert:
			operation = fdiff.Add
		default:
			return nil, fmt.Errorf("unsupported diff operation %d", diff.Type)
		}
		patch.chunks = append(patch.chunks, worktreeDiffChunk{content: diff.Text, operation: operation})
	}
	return patch, nil
}

func (p *worktreeDiffFilePatch) IsBinary() bool { return p.binary }

func (p *worktreeDiffFilePatch) Files() (fdiff.File, fdiff.File) {
	var from, to fdiff.File
	if p.from != nil {
		from = p.from
	}
	if p.to != nil {
		to = p.to
	}
	return from, to
}

func (p *worktreeDiffFilePatch) Chunks() []fdiff.Chunk { return p.chunks }

type worktreeDiffChunk struct {
	content   string
	operation fdiff.Operation
}

func (c worktreeDiffChunk) Content() string       { return c.content }
func (c worktreeDiffChunk) Type() fdiff.Operation { return c.operation }
