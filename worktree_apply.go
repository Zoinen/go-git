package git

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Apply errors.
var (
	// ErrApplyUnsupported is returned for a patch feature that Apply does not
	// support. Apply intentionally accepts only textual unified patches.
	ErrApplyUnsupported = errors.New("unsupported patch")
	// ErrApplyBinary is returned when a patch contains binary data or a binary
	// patch representation.
	ErrApplyBinary = errors.New("binary patches are not supported")
	// ErrApplyBaseMismatch is returned when an index object fingerprint in the
	// patch does not match the file being changed.
	ErrApplyBaseMismatch = errors.New("patch base does not match")
	// ErrApplyHunkFailed is returned when a hunk's context does not match the
	// source text at the location described by its unified range.
	ErrApplyHunkFailed = errors.New("patch hunk does not apply")
)

// ApplyOptions controls how a unified patch is applied.
type ApplyOptions struct {
	// Staged applies the patch to the index and leaves the worktree unchanged.
	// By default Apply updates the worktree and leaves the index unchanged.
	Staged bool
}

// Apply applies a textual unified patch to the worktree or, with Staged set,
// to the index.
//
// Apply rejects binary patches, gitlinks, unresolved index entries, renames,
// copies, and file-mode-only changes. When a patch has an "index" header,
// its old object ID is verified before any mutation. All hunks are verified
// before publication.
func (w *Worktree) Apply(patch []byte, opts *ApplyOptions) error {
	return w.ApplyContext(context.Background(), patch, opts)
}

// ApplyContext applies a textual unified patch to the worktree or, with
// ApplyOptions.Staged set, to the index.
//
// A nil context is treated as context.Background. Cancellation is observed
// while parsing and validating the patch. Once publication begins it cannot
// be interrupted: a staged application writes one new index, while a worktree
// application stages every output file before publishing it and rolls back
// published paths if a later rename fails.
func (w *Worktree) ApplyContext(ctx context.Context, patch []byte, opts *ApplyOptions) error {
	ctx = worktreeOperationContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}

	files, err := parseUnifiedPatch(ctx, patch)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%w: patch has no file changes", ErrApplyUnsupported)
	}

	staged := opts != nil && opts.Staged
	if staged {
		return w.applyToIndex(ctx, files)
	}

	return w.applyToWorktree(ctx, files)
}

type applyFile struct {
	oldPath string
	newPath string

	oldObjectID string
	oldMode     filemode.FileMode
	newMode     filemode.FileMode

	hunks []applyHunk
}

func (f applyFile) sourcePath() string {
	if f.oldPath != "" {
		return f.oldPath
	}
	return f.newPath
}

func (f applyFile) targetPath() string {
	if f.newPath != "" {
		return f.newPath
	}
	return f.oldPath
}

func (f applyFile) isCreate() bool { return f.oldPath == "" }

func (f applyFile) isDelete() bool { return f.newPath == "" }

type applyHunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []applyHunkLine
}

type applyHunkLine struct {
	kind      byte
	text      string
	noNewline bool
}

func parseUnifiedPatch(ctx context.Context, patch []byte) ([]applyFile, error) {
	if bytes.IndexByte(patch, 0) >= 0 {
		return nil, ErrApplyBinary
	}

	lines := splitPatchLines(patch)
	files := make([]applyFile, 0)
	var current *applyFile
	var hunk *applyHunk

	flush := func() error {
		if current == nil {
			return nil
		}
		if current.oldPath == "" && current.newPath == "" {
			return fmt.Errorf("%w: missing file headers", ErrApplyUnsupported)
		}
		if len(current.hunks) == 0 && !current.isCreate() && !current.isDelete() {
			return fmt.Errorf("%w: patch for %q has no textual hunks", ErrApplyUnsupported, current.targetPath())
		}
		if current.oldPath != "" && current.newPath != "" && current.oldPath != current.newPath {
			return fmt.Errorf("%w: renames are not supported", ErrApplyUnsupported)
		}
		if current.oldMode != filemode.Empty && current.newMode != filemode.Empty && current.oldMode != current.newMode {
			return fmt.Errorf("%w: mode changes are not supported", ErrApplyUnsupported)
		}
		if current.oldMode == filemode.Submodule || current.newMode == filemode.Submodule {
			return fmt.Errorf("%w: gitlinks are not supported", ErrApplyUnsupported)
		}
		files = append(files, *current)
		current = nil
		hunk = nil
		return nil
	}

	for lineNumber, line := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		text := line.text
		if hunk != nil {
			if text == "\\ No newline at end of file" {
				if len(hunk.lines) == 0 {
					return nil, fmt.Errorf("line %d: %w: newline marker without a hunk line", lineNumber+1, ErrApplyUnsupported)
				}
				hunk.lines[len(hunk.lines)-1].noNewline = true
				continue
			}
			if len(text) > 0 && (text[0] == ' ' || text[0] == '+' || text[0] == '-') {
				hunk.lines = append(hunk.lines, applyHunkLine{kind: text[0], text: text[1:]})
				continue
			}
		}
		switch {
		case strings.HasPrefix(text, "diff --git "):
			if err := flush(); err != nil {
				return nil, err
			}
			oldPath, newPath, err := parseDiffGitPaths(text)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			current = &applyFile{oldPath: oldPath, newPath: newPath}
		case current == nil:
			if text == "" || strings.HasPrefix(text, "#") {
				continue
			}
			return nil, fmt.Errorf("line %d: %w: expected diff header", lineNumber+1, ErrApplyUnsupported)
		case strings.HasPrefix(text, "Binary files ") || text == "GIT binary patch":
			return nil, fmt.Errorf("line %d: %w", lineNumber+1, ErrApplyBinary)
		case strings.HasPrefix(text, "rename from ") || strings.HasPrefix(text, "rename to ") ||
			strings.HasPrefix(text, "copy from ") || strings.HasPrefix(text, "copy to ") ||
			strings.HasPrefix(text, "similarity index ") || strings.HasPrefix(text, "dissimilarity index "):
			return nil, fmt.Errorf("line %d: %w: rename and copy metadata", lineNumber+1, ErrApplyUnsupported)
		case strings.HasPrefix(text, "old mode "):
			mode, err := parseApplyMode(strings.TrimPrefix(text, "old mode "))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			current.oldMode = mode
		case strings.HasPrefix(text, "new mode "):
			mode, err := parseApplyMode(strings.TrimPrefix(text, "new mode "))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			current.newMode = mode
		case strings.HasPrefix(text, "new file mode "):
			mode, err := parseApplyMode(strings.TrimPrefix(text, "new file mode "))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			current.newMode = mode
		case strings.HasPrefix(text, "deleted file mode "):
			mode, err := parseApplyMode(strings.TrimPrefix(text, "deleted file mode "))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			current.oldMode = mode
		case strings.HasPrefix(text, "index "):
			oldID, err := parseOldObjectID(strings.TrimPrefix(text, "index "))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			current.oldObjectID = oldID
		case strings.HasPrefix(text, "--- "):
			filePath, err := parsePatchHeaderPath(strings.TrimPrefix(text, "--- "), "a/")
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			if current.oldPath != "" && filePath != "" && current.oldPath != filePath {
				return nil, fmt.Errorf("line %d: %w: old path differs from diff header", lineNumber+1, ErrApplyUnsupported)
			}
			current.oldPath = filePath
		case strings.HasPrefix(text, "+++ "):
			filePath, err := parsePatchHeaderPath(strings.TrimPrefix(text, "+++ "), "b/")
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			if current.newPath != "" && filePath != "" && current.newPath != filePath {
				return nil, fmt.Errorf("line %d: %w: new path differs from diff header", lineNumber+1, ErrApplyUnsupported)
			}
			current.newPath = filePath
		case strings.HasPrefix(text, "@@ "):
			parsed, err := parseApplyHunkHeader(text)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			current.hunks = append(current.hunks, parsed)
			hunk = &current.hunks[len(current.hunks)-1]
		default:
			return nil, fmt.Errorf("line %d: %w: unexpected patch line", lineNumber+1, ErrApplyUnsupported)
		}
	}

	if err := flush(); err != nil {
		return nil, err
	}

	seenPaths := make(map[string]struct{}, len(files))
	for _, file := range files {
		if err := validateApplyFile(file); err != nil {
			return nil, err
		}
		target := file.targetPath()
		if _, ok := seenPaths[target]; ok {
			return nil, fmt.Errorf("%w: patch changes %q more than once", ErrApplyUnsupported, target)
		}
		seenPaths[target] = struct{}{}
		for other := range seenPaths {
			if other != target && (strings.HasPrefix(other, target+"/") || strings.HasPrefix(target, other+"/")) {
				return nil, fmt.Errorf("%w: patch changes both %q and %q", ErrApplyUnsupported, target, other)
			}
		}
	}
	return files, nil
}

type patchTextLine struct {
	text string
}

func splitPatchLines(patch []byte) []patchTextLine {
	if len(patch) == 0 {
		return nil
	}

	parts := bytes.SplitAfter(patch, []byte{'\n'})
	lines := make([]patchTextLine, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		if part[len(part)-1] == '\n' {
			part = part[:len(part)-1]
		}
		lines = append(lines, patchTextLine{text: string(part)})
	}
	return lines
}

func parseDiffGitPaths(line string) (string, string, error) {
	value := strings.TrimPrefix(line, "diff --git ")
	oldValue, value, err := parseGitPathField(value)
	if err != nil {
		return "", "", err
	}
	newValue, value, err := parseGitPathField(value)
	if err != nil || strings.TrimSpace(value) != "" {
		return "", "", fmt.Errorf("%w: quoted or malformed diff paths", ErrApplyUnsupported)
	}

	oldPath, err := parsePatchPath(oldValue, "a/")
	if err != nil {
		return "", "", err
	}
	newPath, err := parsePatchPath(newValue, "b/")
	if err != nil {
		return "", "", err
	}
	return oldPath, newPath, nil
}

func parsePatchHeaderPath(value, prefix string) (string, error) {
	pathValue, remainder, err := parseGitPathField(value)
	if err != nil {
		return "", err
	}
	if remainder != "" && !strings.HasPrefix(remainder, "\t") {
		return "", fmt.Errorf("%w: malformed file header", ErrApplyUnsupported)
	}
	return parsePatchPath(pathValue, prefix)
}

func parseGitPathField(value string) (string, string, error) {
	value = strings.TrimLeft(value, " ")
	if value == "" {
		return "", "", fmt.Errorf("%w: missing path", ErrApplyUnsupported)
	}
	if value[0] != '"' {
		end := strings.IndexAny(value, " \t")
		if end < 0 {
			return value, "", nil
		}
		return value[:end], value[end:], nil
	}

	escaped := false
	for i := 1; i < len(value); i++ {
		switch {
		case escaped:
			escaped = false
		case value[i] == '\\':
			escaped = true
		case value[i] == '"':
			decoded, err := strconv.Unquote(value[:i+1])
			if err != nil {
				return "", "", fmt.Errorf("%w: malformed quoted path", ErrApplyUnsupported)
			}
			return decoded, value[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("%w: unterminated quoted path", ErrApplyUnsupported)
}

func parsePatchPath(value, prefix string) (string, error) {
	if value == "/dev/null" {
		return "", nil
	}
	if !strings.HasPrefix(value, prefix) {
		return "", fmt.Errorf("%w: path %q does not use %q prefix", ErrApplyUnsupported, value, prefix)
	}
	value = strings.TrimPrefix(value, prefix)
	if value == "" || path.Clean(value) != value || strings.HasPrefix(value, "../") || strings.ContainsRune(value, '\\') {
		return "", fmt.Errorf("%w: unsafe path %q", ErrApplyUnsupported, value)
	}
	return value, nil
}

func parseApplyMode(value string) (filemode.FileMode, error) {
	mode, err := filemode.New(strings.TrimSpace(value))
	if err != nil {
		return filemode.Empty, fmt.Errorf("%w: invalid file mode: %v", ErrApplyUnsupported, err)
	}
	if !mode.IsRegular() && mode != filemode.Executable {
		return filemode.Empty, fmt.Errorf("%w: file mode %s", ErrApplyUnsupported, mode)
	}
	return mode, nil
}

func parseOldObjectID(value string) (string, error) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return "", fmt.Errorf("%w: malformed index header", ErrApplyUnsupported)
	}
	pair := strings.Split(fields[0], "..")
	if len(pair) != 2 || pair[0] == "" || pair[1] == "" {
		return "", fmt.Errorf("%w: malformed index object IDs", ErrApplyUnsupported)
	}
	if !isObjectIDPrefix(pair[0]) || !isObjectIDPrefix(pair[1]) {
		return "", fmt.Errorf("%w: invalid index object ID", ErrApplyUnsupported)
	}
	return pair[0], nil
}

func isObjectIDPrefix(value string) bool {
	if len(value) < 4 || len(value) > 64 {
		return false
	}
	for _, ch := range value {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

func parseApplyHunkHeader(line string) (applyHunk, error) {
	fields := strings.Fields(line)
	if len(fields) < 4 || fields[0] != "@@" || fields[3] != "@@" {
		return applyHunk{}, fmt.Errorf("%w: malformed hunk header", ErrApplyUnsupported)
	}
	oldStart, oldCount, err := parseHunkRange(fields[1], '-')
	if err != nil {
		return applyHunk{}, err
	}
	newStart, newCount, err := parseHunkRange(fields[2], '+')
	if err != nil {
		return applyHunk{}, err
	}
	return applyHunk{oldStart: oldStart, oldCount: oldCount, newStart: newStart, newCount: newCount}, nil
}

func parseHunkRange(value string, prefix byte) (int, int, error) {
	if len(value) < 2 || value[0] != prefix {
		return 0, 0, fmt.Errorf("%w: malformed hunk range", ErrApplyUnsupported)
	}
	parts := strings.Split(strings.TrimPrefix(value, string(prefix)), ",")
	if len(parts) > 2 {
		return 0, 0, fmt.Errorf("%w: malformed hunk range", ErrApplyUnsupported)
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("%w: invalid hunk start", ErrApplyUnsupported)
	}
	count := 1
	if len(parts) == 2 {
		count, err = strconv.Atoi(parts[1])
		if err != nil || count < 0 {
			return 0, 0, fmt.Errorf("%w: invalid hunk count", ErrApplyUnsupported)
		}
	}
	if start == 0 && count != 0 {
		return 0, 0, fmt.Errorf("%w: invalid zero hunk start", ErrApplyUnsupported)
	}
	return start, count, nil
}

func validateApplyFile(file applyFile) error {
	if file.isCreate() && file.isDelete() {
		return fmt.Errorf("%w: empty file patch", ErrApplyUnsupported)
	}
	for _, hunk := range file.hunks {
		oldCount, newCount := 0, 0
		for _, line := range hunk.lines {
			switch line.kind {
			case ' ':
				oldCount++
				newCount++
			case '-':
				oldCount++
			case '+':
				newCount++
			default:
				return fmt.Errorf("%w: invalid hunk line", ErrApplyUnsupported)
			}
		}
		if oldCount != hunk.oldCount || newCount != hunk.newCount {
			return fmt.Errorf("%w: hunk line counts do not match header", ErrApplyUnsupported)
		}
		if file.isCreate() && hunk.oldStart != 0 {
			return fmt.Errorf("%w: new file hunk has a source range", ErrApplyUnsupported)
		}
		if file.isDelete() && hunk.newStart != 0 {
			return fmt.Errorf("%w: deleted file hunk has a target range", ErrApplyUnsupported)
		}
	}
	return nil
}

func (w *Worktree) applyToWorktree(ctx context.Context, files []applyFile) error {
	plans := make([]worktreeApplyPlan, 0, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		plan, err := w.planWorktreeApply(ctx, file)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return w.publishWorktreeApply(plans)
}

type worktreeApplyPlan struct {
	path    string
	delete  bool
	content []byte
	perm    fs.FileMode
}

func (w *Worktree) planWorktreeApply(ctx context.Context, file applyFile) (worktreeApplyPlan, error) {
	path := file.sourcePath()
	content, info, err := w.readWorktreeText(path)
	if err != nil {
		if !file.isCreate() || !errors.Is(err, os.ErrNotExist) {
			return worktreeApplyPlan{}, err
		}
		content = nil
		info = nil
	}
	if err := ctx.Err(); err != nil {
		return worktreeApplyPlan{}, err
	}
	if err := w.verifyPatchBase(file, content, info != nil); err != nil {
		return worktreeApplyPlan{}, err
	}
	if file.isCreate() && info != nil {
		return worktreeApplyPlan{}, fmt.Errorf("%w: new file %q already exists", ErrApplyBaseMismatch, file.newPath)
	}
	if file.isDelete() && info == nil {
		return worktreeApplyPlan{}, fmt.Errorf("%w: deleted file %q does not exist", ErrApplyBaseMismatch, file.oldPath)
	}

	output, err := applyTextHunks(ctx, content, file.hunks)
	if err != nil {
		return worktreeApplyPlan{}, fmt.Errorf("%s: %w", path, err)
	}
	if file.isDelete() && len(output) != 0 {
		return worktreeApplyPlan{}, fmt.Errorf("%w: deletion patch leaves content", ErrApplyUnsupported)
	}

	perm := fs.FileMode(0o644)
	if info != nil {
		perm = info.Mode().Perm()
	} else if file.newMode == filemode.Executable {
		perm = 0o755
	}
	return worktreeApplyPlan{path: file.targetPath(), delete: file.isDelete(), content: output, perm: perm}, nil
}

func (w *Worktree) readWorktreeText(name string) ([]byte, fs.FileInfo, error) {
	info, err := w.filesystem.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: %q is not a regular text file", ErrApplyUnsupported, name)
	}
	file, err := w.filesystem.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, nil, fmt.Errorf("%w: %q", ErrApplyBinary, name)
	}
	return content, info, nil
}

func (w *Worktree) verifyPatchBase(file applyFile, content []byte, exists bool) error {
	if file.oldObjectID == "" {
		return nil
	}
	if allZeroes(file.oldObjectID) {
		if exists {
			return fmt.Errorf("%w: %q exists but patch expects a new file", ErrApplyBaseMismatch, file.targetPath())
		}
		return nil
	}
	if !exists {
		return fmt.Errorf("%w: %q is absent", ErrApplyBaseMismatch, file.sourcePath())
	}
	hash, err := w.hashBlob(content)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(hash.String(), file.oldObjectID) {
		return fmt.Errorf("%w: %q has %s, patch expects %s", ErrApplyBaseMismatch, file.sourcePath(), hash, file.oldObjectID)
	}
	return nil
}

func (w *Worktree) hashBlob(content []byte) (plumbing.Hash, error) {
	encoded := w.r.Storer.NewEncodedObject()
	encoded.SetType(plumbing.BlobObject)
	encoded.SetSize(int64(len(content)))
	writer, err := encoded.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	_, writeErr := writer.Write(content)
	closeErr := writer.Close()
	if writeErr != nil {
		return plumbing.ZeroHash, writeErr
	}
	if closeErr != nil {
		return plumbing.ZeroHash, closeErr
	}
	return encoded.Hash(), nil
}

func allZeroes(value string) bool {
	return strings.Trim(value, "0") == ""
}

func applyTextHunks(ctx context.Context, content []byte, hunks []applyHunk) ([]byte, error) {
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, ErrApplyBinary
	}
	source := splitTextLines(content)
	output := make([]textLine, 0, len(source))
	sourceOffset := 0

	for _, hunk := range hunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start := hunk.oldStart
		if start == 0 {
			start = 1
		}
		targetOffset := start - 1
		if targetOffset < sourceOffset || targetOffset > len(source) {
			return nil, ErrApplyHunkFailed
		}
		output = append(output, source[sourceOffset:targetOffset]...)
		sourceOffset = targetOffset

		oldCount, newCount := 0, 0
		for _, line := range hunk.lines {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			switch line.kind {
			case ' ':
				if sourceOffset >= len(source) || source[sourceOffset].text != line.text {
					return nil, ErrApplyHunkFailed
				}
				if line.noNewline && source[sourceOffset].newline {
					return nil, ErrApplyHunkFailed
				}
				output = append(output, source[sourceOffset])
				sourceOffset++
				oldCount++
				newCount++
			case '-':
				if sourceOffset >= len(source) || source[sourceOffset].text != line.text {
					return nil, ErrApplyHunkFailed
				}
				if line.noNewline && source[sourceOffset].newline {
					return nil, ErrApplyHunkFailed
				}
				sourceOffset++
				oldCount++
			case '+':
				output = append(output, textLine{text: line.text, newline: !line.noNewline})
				newCount++
			}
		}
		if oldCount != hunk.oldCount || newCount != hunk.newCount {
			return nil, fmt.Errorf("%w: parsed hunk count differs", ErrApplyUnsupported)
		}
	}
	output = append(output, source[sourceOffset:]...)
	return joinTextLines(output), nil
}

type textLine struct {
	text    string
	newline bool
}

func splitTextLines(content []byte) []textLine {
	if len(content) == 0 {
		return nil
	}
	parts := bytes.SplitAfter(content, []byte{'\n'})
	lines := make([]textLine, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		line := textLine{newline: part[len(part)-1] == '\n'}
		if line.newline {
			part = part[:len(part)-1]
		}
		line.text = string(part)
		lines = append(lines, line)
	}
	return lines
}

func joinTextLines(lines []textLine) []byte {
	var output bytes.Buffer
	for _, line := range lines {
		output.WriteString(line.text)
		if line.newline {
			output.WriteByte('\n')
		}
	}
	return output.Bytes()
}

func (w *Worktree) publishWorktreeApply(plans []worktreeApplyPlan) (err error) {
	published := make([]publishedWorktreePath, 0, len(plans))
	staged := make([]stagedWorktreePath, 0, len(plans))

	for _, plan := range plans {
		if plan.delete {
			continue
		}
		if err := w.filesystem.MkdirAll(path.Dir(plan.path), 0o755); err != nil {
			return err
		}
		temporary, err := w.writeApplyTemporary(plan.path, plan.content, plan.perm)
		if err != nil {
			cleanupApplyTemporaries(w.filesystem, staged)
			return err
		}
		staged = append(staged, stagedWorktreePath{target: plan.path, temporary: temporary})
	}
	defer cleanupApplyTemporaries(w.filesystem, staged)

	for _, plan := range plans {
		publishedPath, publishErr := w.publishWorktreePath(plan, staged)
		if publishErr != nil {
			rollbackErr := rollbackWorktreePaths(w.filesystem, published)
			if rollbackErr != nil {
				return fmt.Errorf("apply publish: %w; rollback: %v", publishErr, rollbackErr)
			}
			return publishErr
		}
		published = append(published, publishedPath)
	}

	for _, publishedPath := range published {
		if publishedPath.backup != "" {
			if removeErr := w.filesystem.Remove(publishedPath.backup); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
		}
	}
	return nil
}

type stagedWorktreePath struct {
	target    string
	temporary string
}

type publishedWorktreePath struct {
	target  string
	backup  string
	created bool
}

func (w *Worktree) writeApplyTemporary(target string, content []byte, perm fs.FileMode) (string, error) {
	for attempts := 0; attempts < 10; attempts++ {
		temporary, err := applyTemporaryName(target)
		if err != nil {
			return "", err
		}
		file, err := w.filesystem.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, writeErr := file.Write(content)
		closeErr := file.Close()
		if writeErr != nil {
			_ = w.filesystem.Remove(temporary)
			return "", writeErr
		}
		if closeErr != nil {
			_ = w.filesystem.Remove(temporary)
			return "", closeErr
		}
		return temporary, nil
	}
	return "", fmt.Errorf("unable to allocate temporary patch file for %q", target)
}

func applyTemporaryName(target string) (string, error) {
	buffer := make([]byte, 8)
	if _, err := cryptorand.Read(buffer); err != nil {
		return "", err
	}
	return path.Join(path.Dir(target), ".go-git-apply-"+hex.EncodeToString(buffer)), nil
}

func cleanupApplyTemporaries(filesystem *worktreeFilesystem, staged []stagedWorktreePath) {
	for _, stagedPath := range staged {
		if stagedPath.temporary != "" {
			_ = filesystem.Remove(stagedPath.temporary)
		}
	}
}

func (w *Worktree) publishWorktreePath(plan worktreeApplyPlan, staged []stagedWorktreePath) (publishedWorktreePath, error) {
	var temporary string
	if !plan.delete {
		for _, stagedPath := range staged {
			if stagedPath.target == plan.path {
				temporary = stagedPath.temporary
				break
			}
		}
		if temporary == "" {
			return publishedWorktreePath{}, fmt.Errorf("missing staged patch output for %q", plan.path)
		}
	}

	_, err := w.filesystem.Lstat(plan.path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return publishedWorktreePath{}, err
	}

	published := publishedWorktreePath{target: plan.path, created: !exists}
	if exists {
		backup, err := applyTemporaryName(plan.path)
		if err != nil {
			return publishedWorktreePath{}, err
		}
		if err := w.filesystem.Rename(plan.path, backup); err != nil {
			return publishedWorktreePath{}, err
		}
		published.backup = backup
	}

	if plan.delete {
		return published, nil
	}
	if err := w.filesystem.Rename(temporary, plan.path); err != nil {
		if published.backup != "" {
			_ = w.filesystem.Rename(published.backup, plan.path)
		}
		return publishedWorktreePath{}, err
	}
	return published, nil
}

func rollbackWorktreePaths(filesystem *worktreeFilesystem, paths []publishedWorktreePath) error {
	var rollbackErr error
	for i := len(paths) - 1; i >= 0; i-- {
		published := paths[i]
		if published.created {
			if err := filesystem.Remove(published.target); err != nil && !errors.Is(err, os.ErrNotExist) && rollbackErr == nil {
				rollbackErr = err
			}
		}
		if published.backup != "" {
			if err := filesystem.Remove(published.target); err != nil && !errors.Is(err, os.ErrNotExist) && rollbackErr == nil {
				rollbackErr = err
			}
			if err := filesystem.Rename(published.backup, published.target); err != nil && rollbackErr == nil {
				rollbackErr = err
			}
		}
	}
	return rollbackErr
}

func (w *Worktree) applyToIndex(ctx context.Context, files []applyFile) error {
	idx, err := w.r.Storer.Index()
	if err != nil {
		return err
	}
	newIndex := cloneApplyIndex(idx)

	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.applyFileToIndex(ctx, newIndex, file); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return w.r.Storer.SetIndex(newIndex)
}

func cloneApplyIndex(idx *index.Index) *index.Index {
	clone := *idx
	clone.Entries = make([]*index.Entry, len(idx.Entries))
	for i, entry := range idx.Entries {
		entryCopy := *entry
		clone.Entries[i] = &entryCopy
	}
	clone.Cache = nil
	clone.EndOfIndexEntry = nil
	return &clone
}

func (w *Worktree) applyFileToIndex(ctx context.Context, idx *index.Index, file applyFile) error {
	if countIndexEntries(idx, file.sourcePath()) > 1 {
		return fmt.Errorf("%w: %q has unresolved index entries", ErrApplyUnsupported, file.sourcePath())
	}
	entry, err := idx.Entry(file.sourcePath())
	if err != nil && !errors.Is(err, index.ErrEntryNotFound) {
		return err
	}
	exists := err == nil
	if exists && entry.Stage != 0 {
		return fmt.Errorf("%w: %q has an unresolved index entry", ErrApplyUnsupported, file.sourcePath())
	}
	if file.isCreate() && exists {
		return fmt.Errorf("%w: new file %q is already in the index", ErrApplyBaseMismatch, file.newPath)
	}
	if file.isDelete() && !exists {
		return fmt.Errorf("%w: deleted file %q is absent from the index", ErrApplyBaseMismatch, file.oldPath)
	}

	var content []byte
	if exists {
		content, err = w.readIndexText(entry)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyIndexPatchBase(file, entry, exists); err != nil {
		return err
	}

	output, err := applyTextHunks(ctx, content, file.hunks)
	if err != nil {
		return fmt.Errorf("%s: %w", file.sourcePath(), err)
	}
	if file.isDelete() && len(output) != 0 {
		return fmt.Errorf("%w: deletion patch leaves content", ErrApplyUnsupported)
	}

	if file.isDelete() {
		_, err := idx.Remove(file.oldPath)
		return err
	}

	hash, err := w.storeApplyBlob(output)
	if err != nil {
		return err
	}
	if !exists {
		entry, err = idx.Add(file.newPath)
		if err != nil {
			return err
		}
		entry.Mode = file.newMode
		if entry.Mode == filemode.Empty {
			entry.Mode = filemode.Regular
		}
	}
	entry.Hash = hash
	entry.Size = uint32(len(output))
	entry.ModifiedAt = time.Now()
	entry.CreatedAt = time.Time{}
	return nil
}

func countIndexEntries(idx *index.Index, name string) int {
	count := 0
	for _, entry := range idx.Entries {
		if entry.Name == name {
			count++
		}
	}
	return count
}

func (w *Worktree) readIndexText(entry *index.Entry) ([]byte, error) {
	if !entry.Mode.IsRegular() && entry.Mode != filemode.Executable {
		return nil, fmt.Errorf("%w: %q is not a regular text file", ErrApplyUnsupported, entry.Name)
	}
	blob, err := object.GetBlob(w.r.Storer, entry.Hash)
	if err != nil {
		return nil, err
	}
	reader, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, fmt.Errorf("%w: %q", ErrApplyBinary, entry.Name)
	}
	return content, nil
}

func verifyIndexPatchBase(file applyFile, entry *index.Entry, exists bool) error {
	if file.oldObjectID == "" {
		return nil
	}
	if allZeroes(file.oldObjectID) {
		if exists {
			return fmt.Errorf("%w: %q is already in the index", ErrApplyBaseMismatch, file.targetPath())
		}
		return nil
	}
	if !exists {
		return fmt.Errorf("%w: %q is absent from the index", ErrApplyBaseMismatch, file.sourcePath())
	}
	if !strings.HasPrefix(entry.Hash.String(), file.oldObjectID) {
		return fmt.Errorf("%w: %q has %s, patch expects %s", ErrApplyBaseMismatch, file.sourcePath(), entry.Hash, file.oldObjectID)
	}
	return nil
}

func (w *Worktree) storeApplyBlob(content []byte) (plumbing.Hash, error) {
	encoded := w.r.Storer.NewEncodedObject()
	encoded.SetType(plumbing.BlobObject)
	encoded.SetSize(int64(len(content)))
	writer, err := encoded.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err = writer.Write(content); err != nil {
		_ = writer.Close()
		return plumbing.ZeroHash, err
	}
	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return w.r.Storer.SetEncodedObject(encoded)
}
