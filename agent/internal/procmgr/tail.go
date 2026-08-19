package procmgr

import (
	"bufio"
	"io"
	"os"
	"strings"
	"time"

	"github.com/devcc/agent/internal/logbuf"
)

// tailFile follows a log file into a buffer, like `tail -F`. Polling is fine
// here: a dev tool refreshing logs a few times a second is imperceptible, and
// it avoids per-platform filesystem notification code.
func tailFile(path string, buf *logbuf.Buffer, stop <-chan struct{}) {
	var (
		f      *os.File
		reader *bufio.Reader
		offset int64
	)
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	open := func() bool {
		var err error
		f, err = os.Open(path)
		if err != nil {
			return false
		}
		reader = bufio.NewReaderSize(f, 64*1024)
		offset = 0
		return true
	}

	var partial strings.Builder
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		if f == nil && !open() {
			continue
		}

		// Detect truncation or replacement (log rotation on restart).
		if st, err := os.Stat(path); err == nil {
			if st.Size() < offset {
				_ = f.Close()
				f = nil
				partial.Reset()
				continue
			}
		} else {
			_ = f.Close()
			f = nil
			continue
		}

		for {
			line, err := reader.ReadString('\n')
			offset += int64(len(line))
			if err == io.EOF {
				if line != "" {
					partial.WriteString(line)
				}
				break
			}
			if err != nil {
				_ = f.Close()
				f = nil
				break
			}
			full := partial.String() + line
			partial.Reset()
			buf.Append("stdout", strings.TrimRight(full, "\r\n"))
		}
	}
}

// TailFileSnapshot returns the last n lines of a log file without following it.
func TailFileSnapshot(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read at most the last 1 MiB; dev logs can be large and the UI only wants
	// a tail.
	const maxRead = 1 << 20
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if st.Size() > maxRead {
		start = st.Size() - maxRead
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if start > 0 {
		sc.Scan() // discard the partial first line
	}
	var lines []string
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
		if n > 0 && len(lines) > n*2 {
			lines = lines[len(lines)-n:]
		}
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, sc.Err()
}
