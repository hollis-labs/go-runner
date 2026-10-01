package runner

import (
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	llmtypes "github.com/hollis-labs/go-llm-types"
)

func TestReadLines(t *testing.T) {
	defer func(old int) { maxLineBytes = old }(maxLineBytes)
	maxLineBytes = 16

	var lines []string
	var over []int
	in := "short\r\n" + strings.Repeat("y", 40) + "\nexactly-16-bytes\nno-newline"
	err := readLines(strings.NewReader(in), func(l []byte) { lines = append(lines, string(l)) }, func(n int) { over = append(over, n) })
	if err != nil {
		t.Fatalf("readLines: %v", err)
	}
	if want := []string{"short", "exactly-16-bytes", "no-newline"}; !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	if len(over) != 1 || over[0] != 41 {
		t.Errorf("oversize = %v, want one 41-byte line", over)
	}

	boom := errors.New("boom")
	err = readLines(io.MultiReader(strings.NewReader("a\n"), &errReader{boom}), func([]byte) {}, nil)
	if !errors.Is(err, boom) || !readerFailed(err) {
		t.Errorf("err = %v, want the read error, classified as a failure", err)
	}
	for _, end := range []error{nil, io.EOF, os.ErrClosed, io.ErrClosedPipe} {
		if readerFailed(end) {
			t.Errorf("readerFailed(%v) = true; that is how a reader normally stops", end)
		}
	}
}

// A line well over the old 1 MiB scanner limit is delivered whole.
func TestReadLines_LineOverOneMiB(t *testing.T) {
	big := strings.Repeat("x", 1536*1024)
	var got []int
	if err := readLines(strings.NewReader(big+"\n{}\n"), func(l []byte) { got = append(got, len(l)) }, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != len(big) || got[1] != 2 {
		t.Errorf("line lengths = %v, want [%d 2]", got, len(big))
	}
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

type lineAdapter struct{}

func (lineAdapter) Name() string                      { return "lines" }
func (lineAdapter) BuildArgs(_, _, _ string) []string { return nil }
func (lineAdapter) Detect() (string, bool)            { return "", false }
func (lineAdapter) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	return []llmtypes.StreamEvent{{Type: llmtypes.EventDelta, Content: string(line)}}, nil
}

// A line over maxLineBytes is skipped, not fatal: the lines after it are
// still parsed and emitted.
func TestStreamProviderEvents_OversizeLineIsSkipped(t *testing.T) {
	defer func(old int) { maxLineBytes = old }(maxLineBytes)
	maxLineBytes = 64

	var got []string
	cfg := Config{Provider: lineAdapter{}, OnEvent: func(ev Event) {
		got = append(got, ev.Payload["event"].(llmtypes.StreamEvent).Content)
	}}
	in := "before\n" + strings.Repeat("z", 1000) + "\nafter\n"
	streamProviderEvents(io.NopCloser(strings.NewReader(in)), cfg, nil)
	if want := []string{"before", "after"}; !slices.Equal(got, want) {
		t.Errorf("emitted %q, want %q", got, want)
	}
}
