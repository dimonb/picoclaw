package providers

import (
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// errWSReadTimeout is what wsConn.next returns when no event arrives before
// the deadline. The wording keeps "timeout" so it reads like the socket
// deadline it replaced.
var errWSReadTimeout = errors.New("i/o timeout waiting for the next event")

// errWSClosedLocally ends the reader when we close the connection while it is
// handing over an event.
var errWSClosedLocally = errors.New("websocket closed")

// wsFrameBuffer bounds how many events the reader may queue ahead of the
// consumer. A streamed response is consumed as fast as it arrives; the buffer
// only has to absorb scheduling jitter.
const wsFrameBuffer = 256

// wsConnLostError is the error that stopped the reader: the server closed the
// connection or the socket broke. The connection is gone for good, so it is
// replaced at once rather than after a backoff.
type wsConnLostError struct{ err error }

func (e *wsConnLostError) Error() string { return e.err.Error() }
func (e *wsConnLostError) Unwrap() error { return e.err }

// connLost reports whether err means the connection itself is gone, as
// opposed to the server being slow or rejecting the request.
func connLost(err error) bool {
	var lost *wsConnLostError
	return errors.As(err, &lost)
}

// wsConn owns one WebSocket and the only goroutine that reads it.
//
// Reading continuously is what keeps the connection alive between turns:
// gorilla answers the server's pings only from inside a read, and the Codex
// backend closes a connection whose pings go unanswered ("close 1011:
// keepalive ping timeout"). Losing the connection loses the server-side state
// of the conversation, so the next turn replays the whole history with no
// prompt cache — seconds of extra latency on the first call of every turn
// after a pause. The reader keeps answering pings until the session is closed,
// which the idle cleanup does wsSessionIdleTimeout after the last use.
type wsConn struct {
	*websocket.Conn

	frames    chan []byte
	done      chan struct{} // closed when the reader stops; err is set first
	err       error
	closing   chan struct{}
	closeOnce sync.Once
}

func newWSConn(c *websocket.Conn) *wsConn {
	wc := &wsConn{
		Conn:    c,
		frames:  make(chan []byte, wsFrameBuffer),
		done:    make(chan struct{}),
		closing: make(chan struct{}),
	}
	go wc.readLoop()
	return wc
}

func (c *wsConn) readLoop() {
	defer close(c.done)
	for {
		_, msg, err := c.Conn.ReadMessage()
		if err != nil {
			c.err = &wsConnLostError{err: err}
			return
		}
		select {
		case c.frames <- msg:
		case <-c.closing:
			c.err = &wsConnLostError{err: errWSClosedLocally}
			return
		}
	}
}

// next returns the next event, the error that ended the connection, or
// errWSReadTimeout once deadline passes. Events received before the
// connection ended are still delivered first.
func (c *wsConn) next(deadline time.Time) ([]byte, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case msg := <-c.frames:
		return msg, nil
	case <-c.done:
		select {
		case msg := <-c.frames:
			return msg, nil
		default:
			return nil, c.err
		}
	case <-timer.C:
		return nil, errWSReadTimeout
	}
}

// lost returns the error that ended the connection while it sat idle, or nil
// while the reader is still running.
func (c *wsConn) lost() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// Close closes the socket and releases the reader.
func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closing)
		err = c.Conn.Close()
	})
	return err
}
