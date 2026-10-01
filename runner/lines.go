package runner

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
)

// maxLineBytes is the longest stdout line Run parses. A CLI writes one event
// per line and an event can be large (a tool result carrying a whole file, a
// command's whole output), so the limit is generous. A longer line is read
// through and skipped, never left in the pipe. The 1 MiB scanner this
// replaced stopped reading at the first longer line and closed stdout, so the
// rest of the turn's events were lost and the child died on its next write
// (CW-20261001-0086). It is a variable so tests can exercise the oversize
// path without allocating 64 MiB.
var maxLineBytes = 64 << 20

// readLines calls fn with each line of r, without its line ending (as
// bufio.ScanLines strips "\n" and "\r\n"). The slice is valid only during the
// call. A line longer than maxLineBytes is read through and reported to
// oversize with its length instead; reading carries on. A last line without
// a newline is delivered at EOF. readLines returns nil at EOF and the read
// error otherwise.
func readLines(r io.Reader, fn func(line []byte), oversize func(n int)) error {
	limit := maxLineBytes
	br := bufio.NewReaderSize(r, 64*1024)
	var line []byte
	n, over := 0, false
	flush := func() {
		if over {
			if oversize != nil {
				oversize(n)
			}
		} else {
			l := bytes.TrimSuffix(line, []byte{'\n'})
			fn(bytes.TrimSuffix(l, []byte{'\r'}))
		}
		// Do not hold on to the buffer of an unusually long line.
		if cap(line) > 1<<20 {
			line = nil
		} else {
			line = line[:0]
		}
		n, over = 0, false
	}
	for {
		chunk, err := br.ReadSlice('\n')
		n += len(chunk)
		if !over {
			if len(line)+len(chunk) > limit+1 {
				over = true
				line = line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			flush()
		case errors.Is(err, bufio.ErrBufferFull):
			// The line continues past the read buffer; keep reading.
		case errors.Is(err, io.EOF):
			if n > 0 {
				flush()
			}
			return nil
		default:
			return err
		}
	}
}

// readerFailed reports whether err, returned by readLines, is a real read
// failure rather than the end of the stream. EOF and a closed read end are
// how the stdout reader normally stops.
func readerFailed(err error) bool {
	return err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.ErrClosedPipe)
}
