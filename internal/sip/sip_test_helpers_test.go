package sip

import (
	"net"
	"sync"
	"time"
)

// mockPacketConn is a mock implementation of net.PacketConn for testing.
type mockPacketConn struct {
	mu      sync.Mutex
	written chan []byte
}

func newMockPacketConn() *mockPacketConn {
	return &mockPacketConn{
		written: make(chan []byte, 10), // Buffer to avoid blocking
	}
}

func (c *mockPacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	return 0, nil, nil
}

func (c *mockPacketConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case c.written <- p:
	default:
	}
	return len(p), nil
}

func (c *mockPacketConn) Close() error { return nil }
func (c *mockPacketConn) LocalAddr() net.Addr { return nil }
func (c *mockPacketConn) SetDeadline(t time.Time) error { return nil }
func (c *mockPacketConn) SetReadDeadline(t time.Time) error { return nil }
func (c *mockPacketConn) SetWriteDeadline(t time.Time) error { return nil }

func (c *mockPacketConn) getLastWritten(timeout time.Duration) (string, bool) {
	select {
	case data := <-c.written:
		return string(data), true
	case <-time.After(timeout):
		return "", false
	}
}
