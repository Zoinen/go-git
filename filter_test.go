package git

import (
	"bytes"
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing/format/pktline"
)

func TestOSFilterResolverResolveDoesNotMutateConfig(t *testing.T) {
	cfg := config.NewConfig()
	before := len(cfg.Raw.Sections)

	_, err := NewOSFilterResolver(cfg, "").Resolve(context.Background(), "missing")
	require.ErrorIs(t, err, ErrFilterNotFound)
	assert.Len(t, cfg.Raw.Sections, before)
}

func TestOSFilterResolverRejectsInvalidRequiredValue(t *testing.T) {
	cfg := testFilterConfig("broken", map[string]string{"required": "perhaps"})

	_, err := NewOSFilterResolver(cfg, "").Resolve(context.Background(), "broken")
	require.ErrorIs(t, err, ErrInvalidFilterConfig)
}

func TestOSFilterResolverSingleBlobCleanAndOptionalFallback(t *testing.T) {
	t.Setenv("GO_GIT_FILTER_HELPER_MODE", "simple")
	cfg := testFilterConfig("single", map[string]string{"clean": filterHelperCommand()})
	filter, err := NewOSFilterResolver(cfg, "").Resolve(context.Background(), "single")
	require.NoError(t, err)

	result, err := filter.Apply(context.Background(), FilterRequest{
		Direction: FilterClean,
		Path:      "dir/note.txt",
		Content:   []byte("payload"),
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("simple:payload"), result)

	t.Setenv("GO_GIT_FILTER_HELPER_MODE", "fail")
	result, err = filter.Apply(context.Background(), FilterRequest{
		Direction: FilterClean,
		Path:      "dir/note.txt",
		Content:   []byte("payload"),
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("payload"), result)
}

func TestOSFilterResolverRequiredFilterFailureIsAnError(t *testing.T) {
	t.Setenv("GO_GIT_FILTER_HELPER_MODE", "fail")
	cfg := testFilterConfig("required", map[string]string{
		"clean":    filterHelperCommand(),
		"required": "true",
	})
	filter, err := NewOSFilterResolver(cfg, "").Resolve(context.Background(), "required")
	require.NoError(t, err)
	assert.True(t, filter.Required())

	_, err = filter.Apply(context.Background(), FilterRequest{
		Direction: FilterClean,
		Path:      "note.txt",
		Content:   []byte("payload"),
	})
	require.ErrorIs(t, err, ErrFilterRequired)
}

func TestOSFilterResolverProcessTakesPrecedenceAndUsesProtocol(t *testing.T) {
	t.Setenv("GO_GIT_FILTER_HELPER_MODE", "process")
	cfg := testFilterConfig("process", map[string]string{
		"clean":   filterHelperCommand(),
		"process": filterHelperCommand(),
	})
	filter, err := NewOSFilterResolver(cfg, "").Resolve(context.Background(), "process")
	require.NoError(t, err)

	result, err := filter.Apply(context.Background(), FilterRequest{
		Direction: FilterClean,
		Path:      "dir/note.txt",
		Content:   []byte("payload"),
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("process:clean:dir/note.txt:payload"), result)
}

func TestOSFilterResolverProcessFailureFollowsRequiredSemantics(t *testing.T) {
	t.Run("optional", func(t *testing.T) {
		t.Setenv("GO_GIT_FILTER_HELPER_MODE", "process-error")
		cfg := testFilterConfig("process", map[string]string{"process": filterHelperCommand()})
		filter, err := NewOSFilterResolver(cfg, "").Resolve(context.Background(), "process")
		require.NoError(t, err)

		result, err := filter.Apply(context.Background(), FilterRequest{
			Direction: FilterSmudge,
			Path:      "note.txt",
			Content:   []byte("pointer"),
		})
		require.NoError(t, err)
		assert.Equal(t, []byte("pointer"), result)
	})

	t.Run("required", func(t *testing.T) {
		t.Setenv("GO_GIT_FILTER_HELPER_MODE", "process-error")
		cfg := testFilterConfig("process", map[string]string{
			"process":  filterHelperCommand(),
			"required": "true",
		})
		filter, err := NewOSFilterResolver(cfg, "").Resolve(context.Background(), "process")
		require.NoError(t, err)

		_, err = filter.Apply(context.Background(), FilterRequest{
			Direction: FilterSmudge,
			Path:      "note.txt",
			Content:   []byte("pointer"),
		})
		require.ErrorIs(t, err, ErrFilterRequired)
	})
}

func TestOSFilterResolverCancellationIsNotConvertedToOptionalPassthrough(t *testing.T) {
	t.Setenv("GO_GIT_FILTER_HELPER_MODE", "sleep")
	cfg := testFilterConfig("slow", map[string]string{"clean": filterHelperCommand()})
	filter, err := NewOSFilterResolver(cfg, "").Resolve(context.Background(), "slow")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = filter.Apply(ctx, FilterRequest{Direction: FilterClean, Path: "note.txt", Content: []byte("payload")})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second)
}

func TestOSFilterResolverRejectsUnsafeProtocolPathBeforeStartingAProcess(t *testing.T) {
	t.Setenv("GO_GIT_FILTER_HELPER_MODE", "fail")
	cfg := testFilterConfig("single", map[string]string{"clean": filterHelperCommand()})
	filter, err := NewOSFilterResolver(cfg, "").Resolve(context.Background(), "single")
	require.NoError(t, err)

	_, err = filter.Apply(context.Background(), FilterRequest{
		Direction: FilterClean,
		Path:      "bad\npath",
		Content:   []byte("payload"),
	})
	require.ErrorIs(t, err, ErrInvalidFilterRequest)
}

func TestParseFilterCommandDoesNotInvokeShellSyntax(t *testing.T) {
	argv, err := parseFilterCommand(`"program name" 'two words' three\ four "C:\\tools\\filter.exe" '>' destination`)
	require.NoError(t, err)
	assert.Equal(t, []string{"program name", "two words", "three four", `C:\tools\filter.exe`, ">", "destination"}, argv)

	_, err = parseFilterCommand(`program "unterminated`)
	require.ErrorIs(t, err, ErrInvalidFilterConfig)
}

func testFilterConfig(name string, options map[string]string) *config.Config {
	cfg := config.NewConfig()
	for key, value := range options {
		cfg.Raw.AddOption("filter", name, key, value)
	}
	return cfg
}

func filterHelperCommand() string {
	return strconv.Quote(os.Args[0]) + " -test.run=^TestFilterProcessHelper$"
}

// TestFilterProcessHelper is an external program for the OS resolver tests.
// It only runs in child processes selected by filterHelperCommand.
func TestFilterProcessHelper(t *testing.T) {
	switch os.Getenv("GO_GIT_FILTER_HELPER_MODE") {
	case "":
		return
	case "simple":
		data, err := io.ReadAll(os.Stdin)
		require.NoError(t, err)
		_, err = os.Stdout.Write(append([]byte("simple:"), data...))
		require.NoError(t, err)
		os.Exit(0)
	case "fail":
		os.Exit(42)
	case "sleep":
		time.Sleep(5 * time.Second)
		os.Exit(0)
	case "process", "process-error":
		runFilterProcessHelper(t, os.Getenv("GO_GIT_FILTER_HELPER_MODE"))
		os.Exit(0)
	default:
		t.Fatalf("unknown filter helper mode %q", os.Getenv("GO_GIT_FILTER_HELPER_MODE"))
	}
}

func runFilterProcessHelper(t *testing.T, mode string) {
	greeting := helperReadFilterList(t, os.Stdin)
	require.Equal(t, []string{"git-filter-client", "version=2"}, greeting)
	helperWriteFilterList(t, os.Stdout, "git-filter-server", "version=2")

	capabilities := helperReadFilterList(t, os.Stdin)
	require.Equal(t, []string{"capability=clean", "capability=smudge"}, capabilities)
	helperWriteFilterList(t, os.Stdout, "capability=clean", "capability=smudge")

	request := helperReadFilterList(t, os.Stdin)
	require.Len(t, request, 2)
	require.True(t, strings.HasPrefix(request[0], "command="))
	require.True(t, strings.HasPrefix(request[1], "pathname="))
	data := helperReadFilterData(t, os.Stdin)

	if mode == "process-error" {
		helperWriteFilterList(t, os.Stdout, "status=error")
		return
	}

	helperWriteFilterList(t, os.Stdout, "status=success")
	result := []byte("process:" + strings.TrimPrefix(request[0], "command=") + ":" +
		strings.TrimPrefix(request[1], "pathname=") + ":" + string(data))
	helperWriteFilterData(t, os.Stdout, result)
	helperWriteFilterList(t, os.Stdout)
}

func helperReadFilterList(t *testing.T, reader io.Reader) []string {
	t.Helper()
	var result []string
	for {
		length, payload, err := pktline.ReadLine(reader)
		require.NoError(t, err)
		if length == pktline.Flush {
			return result
		}
		require.GreaterOrEqual(t, length, pktline.LenSize)
		require.NotEmpty(t, payload)
		require.Equal(t, byte('\n'), payload[len(payload)-1])
		result = append(result, string(payload[:len(payload)-1]))
	}
}

func helperWriteFilterList(t *testing.T, writer io.Writer, values ...string) {
	t.Helper()
	for _, value := range values {
		_, err := pktline.Writeln(writer, value)
		require.NoError(t, err)
	}
	require.NoError(t, pktline.WriteFlush(writer))
}

func helperReadFilterData(t *testing.T, reader io.Reader) []byte {
	t.Helper()
	var result bytes.Buffer
	for {
		length, payload, err := pktline.ReadLine(reader)
		require.NoError(t, err)
		if length == pktline.Flush {
			return result.Bytes()
		}
		require.GreaterOrEqual(t, length, pktline.LenSize)
		_, err = result.Write(payload)
		require.NoError(t, err)
	}
}

func helperWriteFilterData(t *testing.T, writer io.Writer, data []byte) {
	t.Helper()
	for len(data) > 0 {
		n := min(len(data), pktline.MaxPayloadSize)
		_, err := pktline.Write(writer, data[:n])
		require.NoError(t, err)
		data = data[n:]
	}
	require.NoError(t, pktline.WriteFlush(writer))
}
