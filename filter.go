package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"unicode"

	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing/format/pktline"
)

// Filter errors.
var (
	// ErrFilterNotFound is returned when no filter driver with the requested
	// name is configured.
	ErrFilterNotFound = errors.New("git filter driver not found")
	// ErrInvalidFilterConfig is returned when a filter configuration value
	// cannot be interpreted by the resolver.
	ErrInvalidFilterConfig = errors.New("invalid git filter configuration")
	// ErrInvalidFilterRequest is returned when a filter request cannot be sent
	// safely to a configured filter process.
	ErrInvalidFilterRequest = errors.New("invalid git filter request")
	// ErrFilterRequired is returned when a required filter cannot transform its
	// input. Optional filters instead preserve their input, matching Git.
	ErrFilterRequired = errors.New("required git filter failed")
	// ErrFilterProcess is returned when a configured filter process exits or
	// does not follow the filter-process protocol.
	ErrFilterProcess = errors.New("git filter process failed")
	// ErrFilterProtocol is returned when a filter.<name>.process command sends
	// an invalid long-running filter protocol message.
	ErrFilterProtocol = errors.New("invalid git filter process protocol")
)

var errFilterCommandMissing = errors.New("git filter command is not configured")

// FilterDirection specifies whether a filter converts worktree content to
// repository content or the reverse.
type FilterDirection uint8

const (
	// FilterClean converts worktree content to repository content.
	FilterClean FilterDirection = iota + 1
	// FilterSmudge converts repository content to worktree content.
	FilterSmudge
)

// FilterRequest describes one clean or smudge conversion. Path is relative to
// the repository root and uses slash separators. It is supplied to a process
// filter as its pathname request field and is substituted for %f in a
// single-blob clean or smudge command.
//
// Content is deliberately a complete byte slice. Callers that publish a
// converted file or index entry need the complete result before publishing it,
// and optional filters must be able to preserve the original input if their
// configured command cannot run.
type FilterRequest struct {
	Direction FilterDirection
	Path      string
	Content   []byte
}

// Filter applies a configured Git clean or smudge driver.
//
// Implementations must honor cancellation. An optional driver whose command
// fails returns an unchanged copy of FilterRequest.Content. A required driver
// returns an error matching ErrFilterRequired instead.
type Filter interface {
	// Name returns the configured filter driver name.
	Name() string
	// Required reports whether this driver is configured with required=true.
	Required() bool
	// Apply runs the requested conversion.
	Apply(context.Context, FilterRequest) ([]byte, error)
}

// FilterResolver resolves a filter attribute value to a configured filter
// driver. Attribute matching itself is intentionally outside this interface:
// callers can use plumbing/format/gitattributes to determine the driver name
// for a repository path before resolving it.
type FilterResolver interface {
	Resolve(context.Context, string) (Filter, error)
}

// OSFilterResolver resolves filter.<name> configuration from Config and runs
// its explicitly configured commands on the host operating system.
//
// Commands are never passed to a shell. The resolver accepts a conservative
// argv-like subset of Git's command-string syntax (whitespace, single quotes,
// double quotes, and backslash escaping), then starts the resulting program
// through exec.CommandContext. Shell redirections, expansions, pipelines, and
// aliases therefore have no effect. Applications that need a different
// process-execution policy can implement FilterResolver themselves.
//
// Dir, when non-empty, is the process working directory. It normally is the
// worktree root so filter commands see the same relative paths as Git.
type OSFilterResolver struct {
	Config *config.Config
	Dir    string
}

// NewOSFilterResolver creates a resolver for the supplied repository
// configuration. The resolver snapshots an individual driver's settings when
// Resolve is called, so a resolved Filter is independent of later changes to
// Config.
func NewOSFilterResolver(cfg *config.Config, dir string) *OSFilterResolver {
	return &OSFilterResolver{Config: cfg, Dir: dir}
}

// Resolve returns the driver named by a filter attribute. It returns
// ErrFilterNotFound if Config does not contain a matching [filter "name"]
// subsection. Resolve does not mutate Config.
func (r *OSFilterResolver) Resolve(ctx context.Context, name string) (Filter, error) {
	ctx = filterContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name == "" || strings.IndexByte(name, 0) >= 0 {
		return nil, fmt.Errorf("%w: empty or NUL-containing driver name", ErrInvalidFilterRequest)
	}

	spec, ok := configuredFilter(r, name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrFilterNotFound, name)
	}

	required, err := parseFilterBool(spec.required)
	if err != nil {
		return nil, fmt.Errorf("%w: filter %q required: %w", ErrInvalidFilterConfig, name, err)
	}

	return &osFilter{
		name:     name,
		required: required,
		process:  spec.process,
		clean:    spec.clean,
		smudge:   spec.smudge,
		dir:      resolverDir(r),
	}, nil
}

type configuredFilterSpec struct {
	process  string
	clean    string
	smudge   string
	required string
}

func configuredFilter(r *OSFilterResolver, name string) (configuredFilterSpec, bool) {
	if r == nil || r.Config == nil || r.Config.Raw == nil {
		return configuredFilterSpec{}, false
	}

	var spec configuredFilterSpec
	found := false
	for _, section := range r.Config.Raw.Sections {
		if section == nil || !section.IsName("filter") {
			continue
		}
		for _, subsection := range section.Subsections {
			if subsection == nil || !subsection.IsName(name) {
				continue
			}
			found = true
			spec = configuredFilterSpec{
				process:  subsection.Option("process"),
				clean:    subsection.Option("clean"),
				smudge:   subsection.Option("smudge"),
				required: subsection.Option("required"),
			}
		}
	}
	return spec, found
}

func resolverDir(r *OSFilterResolver) string {
	if r == nil {
		return ""
	}
	return r.Dir
}

func parseFilterBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "false", "no", "off", "0":
		return false, nil
	case "true", "yes", "on", "1":
		return true, nil
	default:
		return false, fmt.Errorf("%q is not a boolean", value)
	}
}

type osFilter struct {
	name     string
	required bool
	process  string
	clean    string
	smudge   string
	dir      string
}

func (f *osFilter) Name() string { return f.name }

func (f *osFilter) Required() bool { return f.required }

func (f *osFilter) Apply(ctx context.Context, request FilterRequest) ([]byte, error) {
	ctx = filterContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateFilterRequest(request); err != nil {
		return nil, err
	}

	var (
		result []byte
		err    error
	)
	// A filter.process command takes precedence over clean and smudge commands.
	// In particular, a process failure does not fall through to those commands.
	if f.process != "" {
		result, err = f.applyProcess(ctx, request)
	} else {
		command := f.clean
		if request.Direction == FilterSmudge {
			command = f.smudge
		}
		if command == "" {
			err = errFilterCommandMissing
		} else {
			result, err = f.applyCommand(ctx, command, request)
		}
	}
	if err == nil {
		return result, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if !f.required {
		return bytes.Clone(request.Content), nil
	}
	return nil, fmt.Errorf("%w: filter %q: %v", ErrFilterRequired, f.name, err)
}

func validateFilterRequest(request FilterRequest) error {
	if request.Direction != FilterClean && request.Direction != FilterSmudge {
		return fmt.Errorf("%w: unknown conversion direction", ErrInvalidFilterRequest)
	}
	if strings.ContainsAny(request.Path, "\x00\r\n") {
		return fmt.Errorf("%w: pathname contains a protocol control character", ErrInvalidFilterRequest)
	}
	return nil
}

func (f *osFilter) applyCommand(ctx context.Context, command string, request FilterRequest) ([]byte, error) {
	argv, err := parseFilterCommand(command)
	if err != nil {
		return nil, err
	}
	argv = expandFilterPath(argv, request.Path)

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = f.dir
	cmd.Stdin = bytes.NewReader(request.Content)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, filterCommandError(err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func (f *osFilter) applyProcess(ctx context.Context, request FilterRequest) (result []byte, retErr error) {
	argv, err := parseFilterCommand(f.process)
	if err != nil {
		return nil, err
	}
	argv = expandFilterPath(argv, request.Path)

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = f.dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, filterCommandError(err, "")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, filterCommandError(err, "")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, filterCommandError(err, stderr.String())
	}

	waited := false
	defer func() {
		_ = stdin.Close()
		if !waited {
			// An invalid protocol must not leave an untrusted filter process
			// alive waiting for another request.
			if retErr != nil {
				_ = cmd.Process.Kill()
			}
			if err := cmd.Wait(); retErr == nil && err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					retErr = contextErr
				} else {
					retErr = filterCommandError(err, stderr.String())
				}
			}
		}
	}()

	if retErr = writeFilterList(stdin, "git-filter-client", "version=2"); retErr != nil {
		return nil, retErr
	}
	var greeting []string
	if greeting, retErr = readFilterList(stdout); retErr != nil {
		return nil, retErr
	}
	if retErr = validateFilterGreeting(greeting); retErr != nil {
		return nil, retErr
	}

	if retErr = writeFilterList(stdin, "capability=clean", "capability=smudge"); retErr != nil {
		return nil, retErr
	}
	var capabilities []string
	if capabilities, retErr = readFilterList(stdout); retErr != nil {
		return nil, retErr
	}
	command := "clean"
	if request.Direction == FilterSmudge {
		command = "smudge"
	}
	if retErr = validateFilterCapabilities(capabilities, command); retErr != nil {
		return nil, retErr
	}

	if retErr = writeFilterList(stdin, "command="+command, "pathname="+request.Path); retErr != nil {
		return nil, retErr
	}
	if retErr = writeFilterData(stdin, request.Content); retErr != nil {
		return nil, retErr
	}

	var initialStatus []string
	if initialStatus, retErr = readFilterList(stdout); retErr != nil {
		return nil, retErr
	}
	var status string
	if status, retErr = filterStatus(initialStatus, true); retErr != nil {
		return nil, retErr
	}
	if status != "success" {
		return nil, filterStatusError(status)
	}
	if result, retErr = readFilterData(stdout); retErr != nil {
		return nil, retErr
	}
	var finalStatus []string
	if finalStatus, retErr = readFilterList(stdout); retErr != nil {
		return nil, retErr
	}
	if status, retErr = filterStatus(finalStatus, false); retErr != nil {
		return nil, retErr
	}
	if status != "" && status != "success" {
		return nil, filterStatusError(status)
	}

	if retErr = stdin.Close(); retErr != nil {
		return nil, retErr
	}
	if retErr = cmd.Wait(); retErr != nil {
		waited = true
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, filterCommandError(retErr, stderr.String())
	}
	waited = true
	return result, nil
}

func filterCommandError(err error, stderr string) error {
	if stderr = strings.TrimSpace(stderr); stderr != "" {
		return fmt.Errorf("%w: %v: %s", ErrFilterProcess, err, stderr)
	}
	return fmt.Errorf("%w: %v", ErrFilterProcess, err)
}

func filterContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

type filterCommand struct {
	argv []string
}

// parseFilterCommand accepts only the quoting needed to turn a Git filter
// configuration value into a program and an argv slice. It intentionally does
// not interpret shell expressions: every non-quoting byte becomes a literal
// argv byte passed to exec.CommandContext.
func parseFilterCommand(command string) ([]string, error) {
	var args []string
	var arg strings.Builder
	quote := byte(0)
	started := false

	for i := 0; i < len(command); i++ {
		ch := command[i]
		if ch == 0 || ch == '\r' || ch == '\n' {
			return nil, fmt.Errorf("%w: command contains a control character", ErrInvalidFilterConfig)
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
				continue
			}
			if ch == '\\' && quote == '"' && i+1 < len(command) {
				next := command[i+1]
				if next == '"' || next == '\\' {
					arg.WriteByte(next)
					i++
					continue
				}
			}
			arg.WriteByte(ch)
			started = true
			continue
		}

		if unicode.IsSpace(rune(ch)) {
			if started {
				args = append(args, arg.String())
				arg.Reset()
				started = false
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
			started = true
		case '\\':
			if i+1 == len(command) {
				return nil, fmt.Errorf("%w: command ends in an escape", ErrInvalidFilterConfig)
			}
			next := command[i+1]
			if unicode.IsSpace(rune(next)) || next == '\\' || next == '\'' || next == '"' {
				arg.WriteByte(next)
				i++
			} else {
				arg.WriteByte(ch)
			}
			started = true
		default:
			arg.WriteByte(ch)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("%w: command has an unterminated quote", ErrInvalidFilterConfig)
	}
	if started {
		args = append(args, arg.String())
	}
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("%w: empty command", ErrInvalidFilterConfig)
	}
	return args, nil
}

func expandFilterPath(argv []string, path string) []string {
	result := make([]string, len(argv))
	for i, arg := range argv {
		result[i] = strings.ReplaceAll(arg, "%f", path)
	}
	return result
}

func writeFilterList(w io.Writer, values ...string) error {
	for _, value := range values {
		if strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%w: invalid protocol value", ErrInvalidFilterRequest)
		}
		if _, err := pktline.Writeln(w, value); err != nil {
			return err
		}
	}
	return pktline.WriteFlush(w)
}

func readFilterList(r io.Reader) ([]string, error) {
	var values []string
	for {
		length, payload, err := pktline.ReadLine(r)
		if err != nil {
			return nil, err
		}
		if length == pktline.Flush {
			return values, nil
		}
		if length < pktline.LenSize || len(payload) == 0 || payload[len(payload)-1] != '\n' {
			return nil, fmt.Errorf("%w: expected a newline-terminated pkt-line list", ErrFilterProtocol)
		}
		value := string(payload[:len(payload)-1])
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("%w: control character in pkt-line list", ErrFilterProtocol)
		}
		values = append(values, value)
	}
}

func writeFilterData(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), pktline.MaxPayloadSize)
		if _, err := pktline.Write(w, data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return pktline.WriteFlush(w)
}

func readFilterData(r io.Reader) ([]byte, error) {
	var data []byte
	for {
		length, payload, err := pktline.ReadLine(r)
		if err != nil {
			return nil, err
		}
		if length == pktline.Flush {
			return data, nil
		}
		if length < pktline.LenSize {
			return nil, fmt.Errorf("%w: unexpected pkt-line delimiter in content", ErrFilterProtocol)
		}
		data = append(data, payload...)
	}
}

func validateFilterGreeting(values []string) error {
	if len(values) != 2 || values[0] != "git-filter-server" || values[1] != "version=2" {
		return fmt.Errorf("%w: expected git-filter-server version=2 greeting", ErrFilterProtocol)
	}
	return nil
}

func validateFilterCapabilities(values []string, command string) error {
	found := false
	for _, value := range values {
		if !strings.HasPrefix(value, "capability=") {
			return fmt.Errorf("%w: unexpected capability response %q", ErrFilterProtocol, value)
		}
		capability := strings.TrimPrefix(value, "capability=")
		if capability != "clean" && capability != "smudge" {
			return fmt.Errorf("%w: unsupported capability %q", ErrFilterProtocol, capability)
		}
		if capability == command {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%w: filter does not support %s", ErrFilterProcess, command)
	}
	return nil
}

func filterStatus(values []string, required bool) (string, error) {
	status := ""
	for _, value := range values {
		if !strings.HasPrefix(value, "status=") {
			return "", fmt.Errorf("%w: unexpected response field %q", ErrFilterProtocol, value)
		}
		if status != "" {
			return "", fmt.Errorf("%w: more than one status field", ErrFilterProtocol)
		}
		status = strings.TrimPrefix(value, "status=")
	}
	if required && status == "" {
		return "", fmt.Errorf("%w: missing status field", ErrFilterProtocol)
	}
	return status, nil
}

func filterStatusError(status string) error {
	return fmt.Errorf("%w: filter reported status=%s", ErrFilterProcess, status)
}
