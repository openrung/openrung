package wssbridge

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var ErrNonBinaryMessage = errors.New("WSS bridge accepts binary WebSocket messages only")

// websocketStreamConn presents binary WebSocket messages as one opaque byte
// stream for yamux. Text messages fail closed.
type websocketStreamConn struct {
	ws *websocket.Conn

	readMu  sync.Mutex
	reader  io.Reader
	writeMu sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}
}

func newWebsocketStreamConn(ws *websocket.Conn, readLimit int64) *websocketStreamConn {
	if readLimit <= 0 {
		readLimit = 1 << 20
	}
	ws.EnableWriteCompression(false)
	ws.SetReadLimit(readLimit)
	return &websocketStreamConn{ws: ws, closed: make(chan struct{})}
}

func (c *websocketStreamConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if c.reader == nil {
			messageType, reader, err := c.ws.NextReader()
			if err != nil {
				c.abort()
				return 0, err
			}
			if messageType != websocket.BinaryMessage {
				_ = c.ws.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "binary messages required"),
					time.Now().Add(time.Second))
				c.abort()
				return 0, ErrNonBinaryMessage
			}
			c.reader = reader
		}
		n, err := c.reader.Read(p)
		if errors.Is(err, io.EOF) {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			c.abort()
		}
		return n, err
	}
}

func (c *websocketStreamConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	writer, err := c.ws.NextWriter(websocket.BinaryMessage)
	if err != nil {
		c.abort()
		return 0, err
	}
	n, writeErr := writer.Write(p)
	closeErr := writer.Close()
	if writeErr != nil {
		c.abort()
		return n, writeErr
	}
	if closeErr != nil {
		c.abort()
		return n, closeErr
	}
	return n, nil
}

func (c *websocketStreamConn) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
		closeErr = c.ws.Close()
	})
	return closeErr
}

func (c *websocketStreamConn) abort() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.ws.Close()
	})
}

func (c *websocketStreamConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *websocketStreamConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *websocketStreamConn) SetDeadline(deadline time.Time) error {
	if err := c.ws.SetReadDeadline(deadline); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(deadline)
}

func (c *websocketStreamConn) SetReadDeadline(deadline time.Time) error {
	return c.ws.SetReadDeadline(deadline)
}

func (c *websocketStreamConn) SetWriteDeadline(deadline time.Time) error {
	return c.ws.SetWriteDeadline(deadline)
}

func (c *websocketStreamConn) startPings(ctx context.Context, interval, writeTimeout time.Duration) {
	if interval <= 0 {
		return
	}
	if writeTimeout <= 0 {
		writeTimeout = 10 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.closed:
				return
			case <-ticker.C:
				if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
					c.abort()
					return
				}
			}
		}
	}()
}
