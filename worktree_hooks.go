package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-git/go-billy/v6"

	"github.com/go-git/go-git/v6/plumbing"
)

// HookName identifies one of the client-side hooks run by
// CommitWithHooksContext.
type HookName string

// Client-side commit hooks. They are run in this order: HookPreCommit,
// HookPrepareCommitMessage, HookCommitMessage, and HookPostCommit.
const (
	HookPreCommit            HookName = "pre-commit"
	HookPrepareCommitMessage HookName = "prepare-commit-msg"
	HookCommitMessage        HookName = "commit-msg"
	HookPostCommit           HookName = "post-commit"
)

var (
	// ErrHookNotFound tells CommitWithHooksContext that a hook is not
	// installed. It is treated as a successful no-op by that method.
	ErrHookNotFound = errors.New("hook not found")

	// ErrHookPathUnavailable is returned by HookPath when the worktree is not
	// backed by the native OS filesystem. Such a worktree has no pathname that
	// can safely be passed to an operating-system process.
	ErrHookPathUnavailable = errors.New("hook path unavailable")

	// ErrInvalidHookName is returned when HookPath is called with a hook which
	// is not a commit hook supported by CommitWithHooksContext.
	ErrInvalidHookName = errors.New("invalid commit hook name")
)

// HookInvocation describes one hook invocation.
//
// Path is the resolved hook program pathname. It is empty when the worktree
// has no native OS-backed path; a HookRunner may treat that as a missing hook.
// Dir is the worktree root, and Env contains the Git environment for the
// process. Args follows Git's client-side hook protocol: pre-commit and
// post-commit receive no arguments, prepare-commit-msg receives the commit
// message pathname and "message", and commit-msg receives the message
// pathname. The message pathname is empty if it could not be represented on
// the host OS.
//
// Callers implementing HookRunner must not invoke a shell for Path. Treat
// Path as the executable and Args as its already separated argument vector.
// This avoids both shell expansion and accidentally treating hook arguments
// as code.
type HookInvocation struct {
	Name HookName
	Path string
	Args []string
	Dir  string
	Env  []string
}

// HookRunner runs a single client-side hook. A runner should return
// ErrHookNotFound when the hook is not installed; CommitWithHooksContext then
// continues as Git does. Any other error from a pre-commit,
// prepare-commit-msg, or commit-msg hook prevents publication of the commit.
// An error from post-commit is returned together with the already published
// commit hash.
//
// The supplied context is canceled when the caller cancels the commit. A
// runner that starts a process should use a context-aware process API.
type HookRunner interface {
	RunHook(context.Context, HookInvocation) error
}

// HookRunnerFunc adapts a function to HookRunner.
type HookRunnerFunc func(context.Context, HookInvocation) error

// RunHook implements HookRunner.
func (f HookRunnerFunc) RunHook(ctx context.Context, invocation HookInvocation) error {
	if f == nil {
		return ErrHookNotFound
	}

	return f(ctx, invocation)
}

// HookError identifies the hook that failed. It unwraps to the process or
// runner error.
type HookError struct {
	Hook HookName
	Err  error
}

// Error implements error.
func (e *HookError) Error() string {
	return fmt.Sprintf("%s hook failed: %v", e.Hook, e.Err)
}

// Unwrap implements errors.Unwrap.
func (e *HookError) Unwrap() error {
	return e.Err
}

// OSHookRunner runs an installed hook directly as an operating-system
// program. It never invokes a shell, git, gpg, ssh, or git-lfs.
//
// Environment entries are appended after the invocation's Git environment
// and therefore override entries with the same name. Stdout, Stderr, and
// Stdin are optional process streams. A nil stream has the normal os/exec
// behavior. On Unix a hook must be an executable regular file; on Windows an
// executable regular file is passed directly to the OS and no script
// interpreter is inferred.
//
// The standard invocation environment contains GIT_DIR, GIT_COMMON_DIR,
// GIT_WORK_TREE, GIT_INDEX_FILE, GIT_PREFIX, and GIT_EDITOR. GIT_EDITOR is
// set to ':' to match Git's no-editor hook environment. The parent process
// environment is inherited, then invocation Env, then Environment are
// applied, with later values replacing earlier values.
type OSHookRunner struct {
	Environment []string
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

// RunHook implements HookRunner.
func (r OSHookRunner) RunHook(ctx context.Context, invocation HookInvocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if invocation.Path == "" {
		return fmt.Errorf("%w: %s", ErrHookNotFound, invocation.Name)
	}

	info, err := os.Stat(invocation.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrHookNotFound, invocation.Name)
		}
		return &HookError{Hook: invocation.Name, Err: err}
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		return fmt.Errorf("%w: %s", ErrHookNotFound, invocation.Name)
	}

	command := exec.CommandContext(ctx, invocation.Path, invocation.Args...)
	command.Dir = invocation.Dir
	command.Env = mergeHookEnvironment(os.Environ(), invocation.Env, r.Environment)
	command.Stdin = r.Stdin
	command.Stdout = r.Stdout
	command.Stderr = r.Stderr

	if err := command.Run(); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return &HookError{Hook: invocation.Name, Err: err}
	}

	return nil
}

// HookPath returns the native OS path at which the named commit hook is
// looked up. It honors core.hooksPath and uses the repository common Git
// directory for the default hooks directory, which makes linked worktrees
// share hooks with their main worktree.
//
// HookPath does not require the returned file to exist. It is intentionally
// unavailable for in-memory and other non-OS worktrees because exposing an
// invented host path could cause a HookRunner to execute an unrelated file.
func (w *Worktree) HookPath(name HookName) (string, error) {
	if !name.isCommitHook() {
		return "", fmt.Errorf("%w: %q", ErrInvalidHookName, name)
	}

	layout, err := w.commitHookLayout()
	if err != nil {
		return "", err
	}

	return filepath.Join(layout.hooksDirectory, string(name)), nil
}

// CommitWithHooksContext stores the current index in a commit after running
// client-side commit hooks. A nil context is treated as context.Background.
// A nil runner selects OSHookRunner.
//
// The hook order is pre-commit, prepare-commit-msg, commit-msg, then
// post-commit. The first three hooks may reject the commit. post-commit runs
// only after a commit was published; its error is returned with the published
// hash because it cannot roll the commit back. If cancellation races after
// publication, post-commit still runs with a context that retains values but
// is no longer cancelable, then the original cancellation error is returned.
// Missing hooks are no-ops.
//
// prepare-commit-msg and commit-msg receive a temporary message file in the
// Git directory when the worktree has a native OS path. A hook may edit that
// file; its final contents become the commit message. This method does not
// invoke git, a shell, gpg, ssh, or git-lfs. OSHookRunner only starts the hook
// program explicitly configured by the repository owner.
func (w *Worktree) CommitWithHooksContext(ctx context.Context, msg string, opts *CommitOptions, runner HookRunner) (plumbing.Hash, error) {
	ctx = worktreeOperationContext(ctx)
	if err := ctx.Err(); err != nil {
		return plumbing.ZeroHash, err
	}

	if runner == nil {
		runner = OSHookRunner{}
	}

	layout, layoutErr := w.commitHookLayout()
	if layoutErr != nil && !errors.Is(layoutErr, ErrHookPathUnavailable) {
		return plumbing.ZeroHash, layoutErr
	}

	message, err := newCommitHookMessage(layout, msg)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	defer message.close()

	preCommit := w.commitHookInvocation(HookPreCommit, layout, nil)
	if err := runCommitHook(ctx, runner, preCommit); err != nil {
		return plumbing.ZeroHash, err
	}

	prepareCommitMessage := w.commitHookInvocation(HookPrepareCommitMessage, layout,
		[]string{message.path, "message"})
	if err := runCommitHook(ctx, runner, prepareCommitMessage); err != nil {
		return plumbing.ZeroHash, err
	}
	if msg, err = message.read(); err != nil {
		return plumbing.ZeroHash, err
	}

	commitMessage := w.commitHookInvocation(HookCommitMessage, layout, []string{message.path})
	if err := runCommitHook(ctx, runner, commitMessage); err != nil {
		return plumbing.ZeroHash, err
	}
	if msg, err = message.read(); err != nil {
		return plumbing.ZeroHash, err
	}

	hash, commitErr := w.CommitContext(ctx, msg, opts)
	if commitErr != nil && !commitPublishedAfterCancellation(ctx, hash, commitErr) {
		return hash, commitErr
	}

	postCommit := w.commitHookInvocation(HookPostCommit, layout, nil)
	postContext := ctx
	if commitErr != nil {
		// A commit was already published. Preserve the expected post-commit
		// side effect even if the caller happened to cancel at the same time.
		postContext = context.WithoutCancel(ctx)
	}
	if err := runCommitHook(postContext, runner, postCommit); err != nil {
		if commitErr != nil {
			return hash, errors.Join(commitErr, err)
		}
		return hash, err
	}

	return hash, commitErr
}

func commitPublishedAfterCancellation(ctx context.Context, hash plumbing.Hash, err error) bool {
	if hash.IsZero() || ctx.Err() == nil {
		return false
	}
	return errors.Is(err, ctx.Err())
}

func runCommitHook(ctx context.Context, runner HookRunner, invocation HookInvocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	err := runner.RunHook(ctx, invocation)
	if errors.Is(err, ErrHookNotFound) {
		return nil
	}
	return err
}

func (w *Worktree) commitHookInvocation(name HookName, layout commitHookLayout, args []string) HookInvocation {
	invocation := HookInvocation{
		Name: name,
		Args: args,
	}
	if layout.worktree == "" {
		return invocation
	}

	invocation.Path = filepath.Join(layout.hooksDirectory, string(name))
	invocation.Dir = layout.worktree
	invocation.Env = []string{
		"GIT_DIR=" + layout.gitDirectory,
		"GIT_COMMON_DIR=" + layout.commonGitDirectory,
		"GIT_WORK_TREE=" + layout.worktree,
		"GIT_INDEX_FILE=" + filepath.Join(layout.gitDirectory, "index"),
		"GIT_PREFIX=",
		"GIT_EDITOR=:",
	}

	return invocation
}

type commitHookLayout struct {
	worktree           string
	gitDirectory       string
	commonGitDirectory string
	hooksDirectory     string
}

func (w *Worktree) commitHookLayout() (commitHookLayout, error) {
	worktree, ok := nativeOSFilesystemRoot(w.Filesystem())
	if !ok {
		return commitHookLayout{}, ErrHookPathUnavailable
	}

	var err error
	worktree, err = filepath.Abs(worktree)
	if err != nil {
		return commitHookLayout{}, fmt.Errorf("resolve hook worktree: %w", err)
	}

	gitDirectory, err := gitDirectoryForWorktree(worktree)
	if err != nil {
		return commitHookLayout{}, fmt.Errorf("resolve hook Git directory: %w", err)
	}
	commonGitDirectory, err := commonGitDirectory(gitDirectory)
	if err != nil {
		return commitHookLayout{}, fmt.Errorf("resolve hook common Git directory: %w", err)
	}

	cfg, err := w.r.Config()
	if err != nil {
		return commitHookLayout{}, fmt.Errorf("read hook configuration: %w", err)
	}
	hooksDirectory := filepath.Join(commonGitDirectory, "hooks")
	if cfg.Core.HooksPath != "" {
		hooksDirectory = cfg.Core.HooksPath
		if !filepath.IsAbs(hooksDirectory) {
			// Git resolves a relative core.hooksPath from the directory in
			// which hooks run: the worktree root for a non-bare repository.
			hooksDirectory = filepath.Join(worktree, hooksDirectory)
		}
	}

	return commitHookLayout{
		worktree:           worktree,
		gitDirectory:       gitDirectory,
		commonGitDirectory: commonGitDirectory,
		hooksDirectory:     filepath.Clean(hooksDirectory),
	}, nil
}

func nativeOSFilesystemRoot(filesystem billy.Filesystem) (string, bool) {
	return nativeOSFilesystemRootImpl(filesystem)
}

func gitDirectoryForWorktree(worktree string) (string, error) {
	marker := filepath.Join(worktree, GitDirName)
	info, err := os.Stat(marker)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return filepath.Clean(marker), nil
	}

	contents, err := os.ReadFile(marker)
	if err != nil {
		return "", err
	}
	const prefix = "gitdir: "
	firstLine := strings.SplitN(string(contents), "\n", 2)[0]
	if !strings.HasPrefix(firstLine, prefix) {
		return "", fmt.Errorf("%s file has no %s prefix", GitDirName, prefix)
	}

	gitDirectory := strings.TrimSpace(firstLine[len(prefix):])
	if gitDirectory == "" {
		return "", fmt.Errorf("%s file has an empty gitdir", GitDirName)
	}
	if !filepath.IsAbs(gitDirectory) {
		gitDirectory = filepath.Join(worktree, gitDirectory)
	}

	return filepath.Clean(gitDirectory), nil
}

func commonGitDirectory(gitDirectory string) (string, error) {
	contents, err := os.ReadFile(filepath.Join(gitDirectory, "commondir"))
	if errors.Is(err, os.ErrNotExist) {
		return gitDirectory, nil
	}
	if err != nil {
		return "", err
	}

	commonDirectory := strings.TrimSpace(string(contents))
	if commonDirectory == "" {
		return gitDirectory, nil
	}
	if !filepath.IsAbs(commonDirectory) {
		commonDirectory = filepath.Join(gitDirectory, commonDirectory)
	}

	return filepath.Clean(commonDirectory), nil
}

type commitHookMessage struct {
	path    string
	message string
}

func newCommitHookMessage(layout commitHookLayout, message string) (*commitHookMessage, error) {
	result := &commitHookMessage{message: message}
	if layout.gitDirectory == "" {
		return result, nil
	}

	file, err := os.CreateTemp(layout.gitDirectory, "COMMIT_EDITMSG.")
	if err != nil {
		return nil, fmt.Errorf("create commit hook message: %w", err)
	}
	result.path = file.Name()
	if _, err := io.WriteString(file, message); err != nil {
		_ = file.Close()
		_ = os.Remove(result.path)
		return nil, fmt.Errorf("write commit hook message: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(result.path)
		return nil, fmt.Errorf("close commit hook message: %w", err)
	}

	return result, nil
}

func (m *commitHookMessage) read() (string, error) {
	if m.path == "" {
		return m.message, nil
	}

	contents, err := os.ReadFile(m.path)
	if err != nil {
		return "", fmt.Errorf("read commit hook message: %w", err)
	}
	return string(contents), nil
}

func (m *commitHookMessage) close() {
	if m.path != "" {
		_ = os.Remove(m.path)
	}
}

func (name HookName) isCommitHook() bool {
	switch name {
	case HookPreCommit, HookPrepareCommitMessage, HookCommitMessage, HookPostCommit:
		return true
	default:
		return false
	}
}

func mergeHookEnvironment(groups ...[]string) []string {
	values := make([]string, 0)
	positions := make(map[string]int)
	for _, group := range groups {
		for _, value := range group {
			key := hookEnvironmentKey(value)
			if position, ok := positions[key]; ok {
				values[position] = value
				continue
			}
			positions[key] = len(values)
			values = append(values, value)
		}
	}
	return values
}

func hookEnvironmentKey(value string) string {
	key, _, found := strings.Cut(value, "=")
	if !found {
		return value
	}
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}
