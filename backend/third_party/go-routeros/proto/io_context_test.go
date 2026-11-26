package proto

import (
	"bufio"
	"bytes"
	"io"
	"testing"
	"time"
)

// Reading what the buffer already holds must not start a goroutine: the
// reader asks for every length byte and every word separately, so a reply of
// 50,000 rows is some 400,000 reads (local patch; upstream started a
// goroutine, a channel and a buffer for each of them).
func TestBufferedReadsAllocateNothing(t *testing.T) {
	data := bytes.Repeat([]byte{'x'}, 64<<10)
	br := bufio.NewReaderSize(bytes.NewReader(data), 32<<10)
	c := &ctxReader{Reader: br, done: make(chan struct{})}
	if _, err := br.Peek(16 << 10); err != nil { // fill the buffer
		t.Fatal(err)
	}
	p := make([]byte, 8)
	allocs := testing.AllocsPerRun(1000, func() {
		if n, err := c.Read(p); n != len(p) || err != nil {
			t.Fatalf("Read = %d, %v", n, err)
		}
	})
	if allocs != 0 {
		t.Errorf("a read of buffered data allocates %.1f objects per call, want 0", allocs)
	}
}

// The data read on the fast path is the stream's, in order, and a read the
// buffer cannot satisfy still goes to the underlying reader.
func TestBufferedAndUnbufferedReadsAgree(t *testing.T) {
	data := make([]byte, 20000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	c := &ctxReader{Reader: bufio.NewReaderSize(bytes.NewReader(data), 16), done: make(chan struct{})}
	var got []byte
	for _, size := range []int{1, 3, 16, 17, 4096, 1, 9000} {
		p := make([]byte, size)
		n, err := io.ReadFull(c, p)
		if err != nil {
			t.Fatalf("read %d: %v", size, err)
		}
		got = append(got, p[:n]...)
	}
	if !bytes.Equal(got, data[:len(got)]) {
		t.Error("the bytes read differ from the stream")
	}
}

// A read that has to wait can still be cancelled and closed, as before.
func TestBlockingReadIsStillCancellable(t *testing.T) {
	for name, stop := range map[string]func(*ctxReader){"cancel": (*ctxReader).Cancel, "close": (*ctxReader).Close} {
		pr, pw := io.Pipe()
		c := &ctxReader{Reader: bufio.NewReader(pr), done: make(chan struct{})}
		done := make(chan error, 1)
		go func() {
			_, err := c.Read(make([]byte, 4))
			done <- err
		}()
		// Wait until the read is parked in its select, then stop it.
		deadline := time.After(5 * time.Second)
	wait:
		for {
			select {
			case err := <-done:
				t.Fatalf("%s: the read returned before it was stopped: %v", name, err)
			case <-deadline:
				t.Fatalf("%s: the read never blocked", name)
			default:
				if name == "close" {
					stop(c)
					break wait
				}
				// Cancel only reaches a read that is waiting: try until it does.
				select {
				case c.done <- struct{}{}:
					break wait
				default:
					time.Sleep(time.Millisecond)
				}
			}
		}
		select {
		case err := <-done:
			if err != io.EOF {
				t.Errorf("%s: err = %v, want io.EOF", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: the blocked read did not return", name)
		}
		_ = pw.Close()
	}
}
