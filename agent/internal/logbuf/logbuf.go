// Package logbuf provides a bounded in-memory log buffer with live fan-out.
// Bounded matters: a chatty dev server can produce megabytes a minute, and the
// agent is a long-running background process.
package logbuf

import (
	"bufio"
	"io"
	"strings"
	"sync"
	"time"
)

// Line is a single captured log line.
type Line struct {
	Seq    int64     `json:"seq"`
	Stream string    `json:"stream"` // "stdout", "stderr" or "agent"
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
}

// Buffer is a ring buffer of recent lines plus a set of live subscribers.
type Buffer struct {
	mu      sync.Mutex
	lines   []Line
	max     int
	next    int64
	subs    map[int]chan Line
	nextSub int
	dropped int64
	maxLine int
}

// New returns a buffer holding at most max lines.
func New(max int) *Buffer {
	if max <= 0 {
		max = 2000
	}
	return &Buffer{max: max, subs: map[int]chan Line{}, maxLine: 8192}
}

// Append records one line and fans it out to subscribers.
func (b *Buffer) Append(stream, text string) {
	b.mu.Lock()
	if len(text) > b.maxLine {
		text = text[:b.maxLine] + " …[truncated]"
	}
	b.next++
	ln := Line{Seq: b.next, Stream: stream, Text: text, At: time.Now()}
	b.lines = append(b.lines, ln)
	if len(b.lines) > b.max {
		// Drop from the front, reallocating occasionally rather than on every
		// line, so steady-state logging does not thrash the allocator.
		if len(b.lines) > b.max+b.max/4 {
			trimmed := make([]Line, b.max)
			copy(trimmed, b.lines[len(b.lines)-b.max:])
			b.lines = trimmed
		}
	}
	subs := make([]chan Line, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ln:
		default:
			// Slow consumer: drop rather than stall the process we are reading.
			b.mu.Lock()
			b.dropped++
			b.mu.Unlock()
		}
	}
}

// Tail returns the most recent n lines, oldest first.
func (b *Buffer) Tail(n int) []Line {
	b.mu.Lock()
	defer b.mu.Unlock()
	start := 0
	if len(b.lines) > b.max {
		start = len(b.lines) - b.max
	}
	live := b.lines[start:]
	if n > 0 && len(live) > n {
		live = live[len(live)-n:]
	}
	out := make([]Line, len(live))
	copy(out, live)
	return out
}

// Subscribe returns a channel of future lines and a cancel function that must
// be called to release the subscription.
func (b *Buffer) Subscribe(size int) (<-chan Line, func()) {
	if size <= 0 {
		size = 256
	}
	ch := make(chan Line, size)
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

// Reset clears the buffer, used when a project restarts.
func (b *Buffer) Reset() {
	b.mu.Lock()
	b.lines = nil
	b.mu.Unlock()
}

// Writer returns an io.Writer that splits input into lines and appends them.
func (b *Buffer) Writer(stream string) io.Writer {
	return &lineWriter{buf: b, stream: stream}
}

type lineWriter struct {
	buf     *Buffer
	stream  string
	partial strings.Builder
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for {
		i := strings.IndexByte(string(p), '\n')
		if i < 0 {
			w.partial.Write(p)
			// Guard against a process that never emits a newline.
			if w.partial.Len() > 1<<20 {
				w.buf.Append(w.stream, w.partial.String())
				w.partial.Reset()
			}
			return n, nil
		}
		w.partial.Write(p[:i])
		w.buf.Append(w.stream, strings.TrimRight(w.partial.String(), "\r"))
		w.partial.Reset()
		p = p[i+1:]
	}
}

// Pump copies a reader into the buffer line by line until EOF.
func (b *Buffer) Pump(stream string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		b.Append(stream, strings.TrimRight(sc.Text(), "\r"))
	}
}
