package logbuf

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRingBufferIsBounded(t *testing.T) {
	// A chatty dev server must not grow the agent's memory without limit.
	b := New(100)
	for i := 0; i < 5000; i++ {
		b.Append("stdout", "line")
	}
	if got := len(b.Tail(0)); got > 130 {
		t.Errorf("buffer kept %d lines for a max of 100", got)
	}
	tail := b.Tail(5)
	if len(tail) != 5 {
		t.Fatalf("Tail(5) = %d lines", len(tail))
	}
	if tail[4].Seq != 5000 {
		t.Errorf("Tail should return the newest lines, got seq %d", tail[4].Seq)
	}
}

func TestLongLinesAreTruncated(t *testing.T) {
	b := New(10)
	b.Append("stdout", strings.Repeat("x", 100000))
	line := b.Tail(1)[0]
	if len(line.Text) > 9000 {
		t.Errorf("line was not truncated: %d chars", len(line.Text))
	}
	if !strings.HasSuffix(line.Text, "[truncated]") {
		t.Error("truncation should be visible to the reader")
	}
}

func TestSubscribeReceivesNewLines(t *testing.T) {
	b := New(50)
	ch, cancel := b.Subscribe(10)
	defer cancel()

	b.Append("stdout", "hello")
	select {
	case line := <-ch:
		if line.Text != "hello" {
			t.Errorf("got %q", line.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber never received the line")
	}
}

func TestSlowSubscriberDoesNotBlockTheWriter(t *testing.T) {
	b := New(5000)
	_, cancel := b.Subscribe(1) // a subscriber that never reads
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			b.Append("stdout", "line")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Blocking here would stall the goroutine reading a process's output,
		// which in turn stalls the process itself.
		t.Fatal("Append blocked on a subscriber that stopped reading")
	}
}

func TestCancelIsIdempotentAndUnsubscribes(t *testing.T) {
	b := New(10)
	ch, cancel := b.Subscribe(4)
	cancel()
	cancel() // must not panic on a double close

	b.Append("stdout", "after cancel")
	for range ch { // the channel is closed, so this drains and exits
	}
}

func TestPumpSplitsLines(t *testing.T) {
	b := New(100)
	b.Pump("stdout", strings.NewReader("one\ntwo\r\nthree\n"))
	lines := b.Tail(0)
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %+v", len(lines), lines)
	}
	if lines[1].Text != "two" {
		t.Errorf("carriage returns should be stripped, got %q", lines[1].Text)
	}
}

func TestConcurrentAppendAndTail(t *testing.T) {
	b := New(500)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				b.Append("stdout", "x")
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				b.Tail(20)
			}
		}()
	}
	wg.Wait()
}
