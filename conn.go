package rwconn

import (
	"io"
	"net"
	"os"
	"sync"
	"time"
)

var _ net.Conn = (*conn)(nil)

type conn struct {
	wrMu sync.Mutex // guard Write operations
	rdMu sync.Mutex // guard Read operation
	r    io.Reader
	w    io.Writer

	rx    chan []byte // channel to read asynchronous
	tx    chan []byte // channel to write asynchronous
	txErr chan error

	once  sync.Once   // Protects closing the connection
	timer *time.Timer // delays closing the connection too fast (give time to the writer to flush)
	done  chan struct{}

	readDeadline  *connDeadline
	writeDeadline *connDeadline
}

func NewConn(r io.Reader, w io.Writer) net.Conn {
	c := &conn{
		r: r,
		w: w,

		rx: make(chan []byte),
		tx: make(chan []byte),

		done:          make(chan struct{}),
		readDeadline:  makeConnDeadline(),
		writeDeadline: makeConnDeadline(),
	}
	go c.asyncRead()
	go c.asyncWrite()

	return c
}

// async reader to allow cancelling reads
// without closing the connection.
func (c *conn) asyncRead() {
	buf := make([]byte, 1024)
	for {
		n, err := c.r.Read(buf)
		if n > 0 {
			tmp := make([]byte, n)
			copy(tmp, buf[:n])
			c.rx <- tmp
		}
		if err != nil {
			c.Close()
			return
		}
		if isClosedChan(c.done) {
			return
		}
	}
}

// async writer to allow cancelling writes
// without closing the connection.
func (c *conn) asyncWrite() {
	for {
		select {
		case <-c.done:
			return
		case d, ok := <-c.tx:
			if !ok {
				c.Close()
				return
			}
			_, err := c.w.Write(d)
			if err != nil {
				c.Close()
				return
			}
		}
	}
}

// connection parameters (obtained from net.Pipe)
// https://cs.opensource.google/go/go/+/refs/tags/go1.17:src/net/pipe.go;bpv=0;bpt=1

// connDeadline is an abstraction for handling timeouts.
type connDeadline struct {
	mu     sync.Mutex // Guards timer and cancel
	timer  *time.Timer
	cancel chan struct{} // Must be non-nil
}

func makeConnDeadline() *connDeadline {
	return &connDeadline{cancel: make(chan struct{})}
}

// wait returns a channel that is closed when the deadline is exceeded.
func (c *connDeadline) wait() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancel
}

// set sets the point in time when the deadline will time out.
// A timeout event is signaled by closing the channel returned by waiter.
// Once a timeout has occurred, the deadline can be refreshed by specifying a
// t value in the future.
//
// A zero value for t prevents timeout.
func (c *connDeadline) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.timer != nil && !c.timer.Stop() {
		<-c.cancel // Wait for the timer callback to finish and close cancel
	}
	c.timer = nil

	// Time is zero, then there is no deadline.
	closed := isClosedChan(c.cancel)
	if t.IsZero() {
		if closed {
			c.cancel = make(chan struct{})
		}
		return
	}

	// Time in the future, setup a timer to cancel in the future.
	if dur := time.Until(t); dur > 0 {
		if closed {
			c.cancel = make(chan struct{})
		}
		c.timer = time.AfterFunc(dur, func() {
			close(c.cancel)
		})
		return
	}

	// Time in the past, so close immediately.
	if !closed {
		close(c.cancel)
	}
}

func isClosedChan(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// Write writes data to the connection
func (c *conn) Write(data []byte) (int, error) {
	c.wrMu.Lock()
	defer c.wrMu.Unlock()

	switch {
	case isClosedChan(c.done):
		return 0, io.ErrClosedPipe
	case isClosedChan(c.writeDeadline.wait()):
		return 0, os.ErrDeadlineExceeded
	}

	buf := make([]byte, len(data))
	n := copy(buf, data)
	select {
	case <-c.done:
		return 0, io.ErrClosedPipe
	case <-c.writeDeadline.wait():
		return 0, os.ErrDeadlineExceeded
	case c.tx <- buf:
	}
	c.timer = time.NewTimer(500 * time.Millisecond)
	return n, nil
}

// Read reads data from the connection
func (c *conn) Read(data []byte) (int, error) {
	c.rdMu.Lock()
	defer c.rdMu.Unlock()

	if data == nil {
		return 0, io.ErrClosedPipe
	}

	switch {
	case isClosedChan(c.done):
		return 0, io.ErrClosedPipe
	case isClosedChan(c.readDeadline.wait()):
		return 0, os.ErrDeadlineExceeded
	}

	select {
	case <-c.done:
		// TODO: TestConn/BasicIO the other end stops writing and the http connection is closed
		// closing this connection that is blocked on read.
		return 0, io.EOF
	case <-c.readDeadline.wait():
		return 0, os.ErrDeadlineExceeded
	case d, ok := <-c.rx:
		if !ok {
			return 0, io.EOF
		}
		n := copy(data, d)
		return n, nil
	}
}

// Close closes the connection
func (c *conn) Close() error {
	c.once.Do(c.close)
	return nil
}

func (c *conn) close() {
	if c.timer != nil {
		<-c.timer.C
	}
	if readerCloser, ok := c.r.(io.Closer); ok {
		readerCloser.Close()
	}

	if writerCloser, ok := c.w.(io.Closer); ok {
		writerCloser.Close()
	}

	close(c.done)
}

func (c *conn) Done() <-chan struct{} {
	return c.done
}

func (c *conn) LocalAddr() net.Addr {
	return connAddr{}
}

func (c *conn) RemoteAddr() net.Addr {
	return connAddr{}
}

func (c *conn) SetDeadline(t time.Time) error {
	if isClosedChan(c.done) {
		return io.ErrClosedPipe
	}
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return nil
}

func (c *conn) SetWriteDeadline(t time.Time) error {
	if isClosedChan(c.done) {
		return io.ErrClosedPipe
	}
	if c.writeDeadline == nil {
		c.writeDeadline = makeConnDeadline()
	}
	c.writeDeadline.set(t)
	return nil
}

func (c *conn) SetReadDeadline(t time.Time) error {
	if isClosedChan(c.done) {
		return io.ErrClosedPipe
	}
	if c.readDeadline == nil {
		c.readDeadline = makeConnDeadline()
	}
	c.readDeadline.set(t)
	return nil
}

type connAddr struct{}

func (connAddr) Network() string { return "rwconn" }
func (connAddr) String() string  { return "rwconn" }
