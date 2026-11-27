package proto

import (
	"bufio"
	"io"
	"sync/atomic"
)

type ctxResult struct {
	num int
	err error
}

type ctxReader struct {
	io.Reader
	close atomic.Bool
	// stale is set once a waiting Read was abandoned (cancelled or closed):
	// its goroutine may still be inside Reader, so the fast path below must
	// not touch Reader any more (local patch).
	stale atomic.Bool
	done  chan struct{}
}

func (c *ctxReader) Close() {
	if c.close.Load() {
		return
	}
	c.close.Store(true)
	close(c.done)
}

func (c *ctxReader) Cancel() {
	if c.close.Load() {
		return
	}

	select {
	case c.done <- struct{}{}:
	default:
	}
}

func (c *ctxReader) Read(p []byte) (int, error) {
	// Local patch: what the buffer already holds is returned at once. The
	// sentence reader asks for every length byte and every word separately;
	// upstream started a goroutine, a channel and a copy buffer for each of
	// those reads — some 400,000 for a reply of 50,000 rows. A read the
	// buffer can satisfy cannot block, so it needs no way to be cancelled.
	// The goroutine below is only for a read that has to wait for the peer.
	if br, ok := c.Reader.(*bufio.Reader); ok && len(p) > 0 && !c.close.Load() && !c.stale.Load() && br.Buffered() >= len(p) {
		return br.Read(p)
	}

	out := make(chan *ctxResult, 1)
	buf := make([]byte, len(p))

	go func() {
		res := new(ctxResult)
		res.num, res.err = c.Reader.Read(buf)
		out <- res
		close(out)
	}()

	select {
	case <-c.done:
		c.stale.Store(true)
		return 0, io.EOF
	case r := <-out:
		copy(p, buf)

		return r.num, r.err
	}
}

type ctxWriter struct {
	*bufio.Writer
	close atomic.Bool
	done  chan struct{}
}

func (c *ctxWriter) Close() {
	if c.close.Load() {
		return
	}
	c.close.Store(true)
	close(c.done)
}

func (c *ctxWriter) Cancel() {
	if c.close.Load() {
		return
	}

	select {
	case c.done <- struct{}{}:
	default:
	}
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	out := make(chan *ctxResult, 1)
	buf := make([]byte, len(p))
	copy(buf, p)

	go func() {
		res := new(ctxResult)
		res.num, res.err = c.Writer.Write(buf)
		out <- res
		close(out)
	}()

	select {
	case <-c.done:
		return 0, io.EOF
	case r := <-out:
		return r.num, r.err
	}
}
