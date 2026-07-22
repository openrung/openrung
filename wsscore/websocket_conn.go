package wsscore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var ErrNonBinaryMessage = errors.New("WSS transport accepts binary WebSocket messages only")

// WebSocketConn presents consecutive binary WebSocket messages as one opaque
// net.Conn byte stream. Text messages fail closed and payload bytes are never
// parsed or logged.
type WebSocketConn struct {
	ws *websocket.Conn

	readMu  sync.Mutex
	reader  io.Reader
	writeMu sync.Mutex

	pingOnce  sync.Once
	closeOnce sync.Once
	closed    chan struct{}
}

func NewWebSocketConn(ws *websocket.Conn, readLimit int64) (*WebSocketConn, error) {
	if ws == nil {
		return nil, errors.New("WebSocket connection is required")
	}
	if readLimit == 0 {
		readLimit = DefaultWebSocketReadMax
	}
	if readLimit < 1 || readLimit > MaxWebSocketReadMax {
		return nil, fmt.Errorf("WebSocket message read limit must be within [1, %d]", MaxWebSocketReadMax)
	}
	ws.EnableWriteCompression(false)
	ws.SetReadLimit(readLimit)
	return &WebSocketConn{ws: ws, closed: make(chan struct{})}, nil
}

func (c *WebSocketConn) Read(p []byte) (int, error) {
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

func (c *WebSocketConn) Write(p []byte) (int, error) {
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
	n, writeErr := writeAll(writer, p)
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

func (c *WebSocketConn) Close() error {
	if c == nil {
		return nil
	}
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

func (c *WebSocketConn) abort() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.ws.Close()
	})
}

func (c *WebSocketConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *WebSocketConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *WebSocketConn) SetDeadline(deadline time.Time) error {
	if err := c.ws.SetReadDeadline(deadline); err != nil {
		return err
	}
	return c.SetWriteDeadline(deadline)
}

func (c *WebSocketConn) SetReadDeadline(deadline time.Time) error {
	return c.ws.SetReadDeadline(deadline)
}

func (c *WebSocketConn) SetWriteDeadline(deadline time.Time) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.SetWriteDeadline(deadline)
}

// StartPings starts at most one bounded WebSocket ping loop. A non-positive
// interval disables pings, which is useful only when another layer supplies a
// keepalive or in deterministic tests.
func (c *WebSocketConn) StartPings(ctx context.Context, interval, writeTimeout time.Duration) error {
	if c == nil || c.ws == nil {
		return errors.New("WebSocket connection is not initialized")
	}
	if interval <= 0 {
		return nil
	}
	if interval > MaxPingInterval {
		return fmt.Errorf("ping interval must not exceed %s", MaxPingInterval)
	}
	if writeTimeout <= 0 {
		writeTimeout = DefaultPingWriteTimeout
	}
	if writeTimeout > MaxPingWriteTimeout || writeTimeout >= interval {
		return fmt.Errorf("ping write timeout must be at most %s and shorter than the ping interval", MaxPingWriteTimeout)
	}
	c.pingOnce.Do(func() {
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
	})
	return nil
}
