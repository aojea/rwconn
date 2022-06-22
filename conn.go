package rwconn

import (
	"bytes"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

var _ net.Conn = (*RWConn)(nil)

type RWConn struct {
	wrMu sync.Mutex // guard Write operations
	rdMu sync.Mutex // guard Read operation
	r    io.Reader
	w    io.Writer

	rx chan []byte // channel to read asynchronous

	once sync.Once // Protects closing the connection

	delayMu   sync.Mutex // guard delay operations
	lastWrite time.Time  // timestamp last write was done
	delay     time.Duration

	done chan struct{}

	closeFn func() // called only once when closing the connection

	readDeadline  *connDeadline
	writeDeadline *connDeadline
}

func SetWriteDelay(t time.Duration) func(*RWConn) {
	return func(c *RWConn) {
		c.delay = t
	}
}

func SetCloseHook(f func()) func(*RWConn) {
	return func(c *RWConn) {
		c.closeFn = f
	}
}

func NewConn(r io.Reader, w io.Writer, options ...func(*RWConn)) net.Conn {
	c := &RWConn{
		r:             r,
		w:             w,
		rx:            make(chan []byte),
		done:          make(chan struct{}),
		delay:         50 * time.Millisecond,
		readDeadline:  makeConnDeadline(),
		writeDeadline: makeConnDeadline(),
	}

	for _, o := range options {
		o(c)
	}

	go c.asyncRead()

	return c
}

// async reader to allow cancelling reads
// without closing the connection.
func (c *RWConn) asyncRead() {
	buf := make([]byte, 1024)
	for {
		n, err := c.r.Read(buf)
		if n > 0 {
			tmp := make([]byte, n)
			copy(tmp, buf[:n])
			select {
			case <-c.done:
				return
			case c.rx <- tmp:
			}
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

// Write data to the connection in fixed chunks of 1024
func (c *RWConn) Write(data []byte) (int, error) {
	c.wrMu.Lock()
	defer c.wrMu.Unlock()

	switch {
	case isClosedChan(c.done):
		return 0, io.ErrClosedPipe
	case isClosedChan(c.writeDeadline.wait()):
		return 0, os.ErrDeadlineExceeded
	}
	// write in chunks of 1024 bytes to avoid exceeding the maximum packet size
	// on the underline writer.
	buf := make([]byte, len(data))
	copy(buf, data)
	r := bytes.NewReader(buf)
	var err error
	var written int64
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		written, err = io.Copy(c.w, r)
	}()

	select {
	case <-c.done:
		return 0, io.ErrClosedPipe
	case <-c.writeDeadline.wait():
		return 0, os.ErrDeadlineExceeded
	case <-doneCh:
	}

	c.delayMu.Lock()
	if c.delay > 0 {
		c.lastWrite = time.Now()
	}
	c.delayMu.Unlock()
	return int(written), err
}

// Read reads data from the connection
func (c *RWConn) Read(data []byte) (int, error) {
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
func (c *RWConn) Close() error {
	c.once.Do(c.close)
	return nil
}

func (c *RWConn) close() {
	// wait for the write delay interval set
	c.delayMu.Lock()
	if c.delay > 0 {
		// wait the configured delay - the time since last writes
		if wait := c.delay - time.Now().Sub(c.lastWrite); wait > 0 {
			time.Sleep(wait)
		}
	}
	c.delayMu.Unlock()

	if readerCloser, ok := c.r.(io.Closer); ok {
		readerCloser.Close()
	}

	if writerCloser, ok := c.w.(io.Closer); ok {
		writerCloser.Close()
	}

	// execute configured hook
	if c.closeFn != nil {
		c.closeFn()
	}
	close(c.done)
}

func (c *RWConn) Done() <-chan struct{} {
	return c.done
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

func (c *RWConn) SetDeadline(t time.Time) error {
	if isClosedChan(c.done) {
		return io.ErrClosedPipe
	}
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return nil
}

func (c *RWConn) SetWriteDeadline(t time.Time) error {
	if isClosedChan(c.done) {
		return io.ErrClosedPipe
	}
	if c.writeDeadline == nil {
		c.writeDeadline = makeConnDeadline()
	}
	c.writeDeadline.set(t)
	return nil
}

func (c *RWConn) SetReadDeadline(t time.Time) error {
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

func (c *RWConn) LocalAddr() net.Addr {
	return connAddr{}
}

func (c *RWConn) RemoteAddr() net.Addr {
	return connAddr{}
}
