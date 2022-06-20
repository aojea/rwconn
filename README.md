# rwconn

Create a golang net.Conn using a Reader and a Writer.

The connection created implements Deadlines, that are used to stop a Read on a net.Conn without closing the connection.

This is really useful when you want to use the connection to send http traffic, since the net/http library [uses the deadline
the cancel reads without closing the connections](https://groups.google.com/g/golang-nuts/c/VPVWFrpIEyo/m/d5CdnIsPAwAJ), per example, for [http.Hijacking](https://github.com/golang/go/blob/527ace0ffa81d59698d3a78ac3545de7295ea76b/src/net/http/server.go#L315-L334
)

```go
// c.mu must be held.
func (c *conn) hijackLocked() (rwc net.Conn, buf *bufio.ReadWriter, err error) {
	if c.hijackedv {
		return nil, nil, ErrHijacked
	}
	c.r.abortPendingRead()

	c.hijackedv = true
	rwc = c.rwc
	rwc.SetDeadline(time.Time{})

	buf = bufio.NewReadWriter(c.bufr, bufio.NewWriter(rwc))
	if c.r.hasByte {
		if _, err := c.bufr.Peek(c.bufr.Buffered() + 1); err != nil {
			return nil, nil, fmt.Errorf("unexpected Peek failure reading buffered byte: %v", err)
		}
	}
	c.setState(rwc, StateHijacked, runHooks)
	return
}
```

## Example

This module was created specifically to create connections over HTTP, those can achieved using the Request and Response bodies.

### Server

```go
// flushWriter
type flushWrite struct {
	w    io.Writer
	f    http.Flusher
	done chan struct{}
}

func (w *flushWrite) Write(data []byte) (int, error) {
	select {
	case <-w.done:
	default:
	}
	n, err := w.w.Write(data)
	w.f.Flush()
	return n, err
}

func (w *flushWrite) Close() error {
	select {
	case <-w.done:
	default:
		close(w.done)
	}
	return nil
}
func (w *flushWrite) Done() chan struct{} {
	select {
	case <-w.done:
	default:
		w.done = make(chan struct{})
	}
	return w.done
}

// handler to create a connection
func connHandler(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				panic("flusher not supported")
			}

			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			fw := &flushWrite{w: w, f: flusher}
			c := NewConn(r.Body, fw, SetWriteDelay(500*time.Millisecond))
      
      // handle the connection
      // go handleConn(c)

      // block until the connection is closed
			select {
			case <-r.Context().Done():
				c.Close()
			case <-fw.Done():
			}
}
```

### Client

```go
pr, pw := io.Pipe()
client := srv.Client()

// Create a request object to send to the server
req, err := http.NewRequest(http.MethodGet, srv.URL, pr)
if err != nil {
	return nil, nil, nil, err
}

// Perform the request
resp, err := client.Do(req)
if err != nil {
	return nil, nil, nil, err
}
if resp.StatusCode != 200 {
	return nil, nil, nil, fmt.Errorf("wrong status code")
}
c := NewConn(resp.Body, pw)
// manage connection
// ...
```




