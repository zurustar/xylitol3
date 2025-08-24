package sip

import (
	"fmt"
	"io"
	"net"
)

// Transport defines an interface for a SIP transport layer, which can be backed
// by different network protocols like UDP, TCP, etc.
type Transport interface {
	// Writer is used to send a complete SIP message to the remote peer.
	io.Writer
	// GetProto returns the transport protocol name (e.g., "UDP", "TCP").
	GetProto() string
	// GetRemoteAddr returns the network address of the remote peer.
	GetRemoteAddr() net.Addr
	// Close terminates the transport connection if applicable.
	Close() error
}

// --- UDP Transport ---

// UDPTransport is a transport implementation for UDP.
type UDPTransport struct {
	conn     net.PacketConn
	destAddr net.Addr
}

// NewUDPTransport creates a new UDP transport instance.
func NewUDPTransport(conn net.PacketConn, destAddr net.Addr) *UDPTransport {
	return &UDPTransport{
		conn:     conn,
		destAddr: destAddr,
	}
}

// Write sends data to the destination address over the UDP connection.
func (t *UDPTransport) Write(p []byte) (n int, err error) {
	return t.conn.WriteTo(p, t.destAddr)
}

// GetProto returns "UDP".
func (t *UDPTransport) GetProto() string {
	return "UDP"
}

// GetRemoteAddr returns the destination network address.
func (t *UDPTransport) GetRemoteAddr() net.Addr {
	return t.destAddr
}

// Close for UDP is a no-op from the perspective of a single "transaction",
// as the underlying PacketConn is shared and managed by the server listener.
func (t *UDPTransport) Close() error {
	return nil
}

// --- TCP Transport ---

// TCPTransport is a transport implementation for TCP.
type TCPTransport struct {
	conn net.Conn
}

// NewTCPTransport creates a new TCP transport instance.
func NewTCPTransport(conn net.Conn) *TCPTransport {
	return &TCPTransport{
		conn: conn,
	}
}

// Write sends data over the TCP connection.
// The provided byte slice should be a fully-formed SIP message.
func (t *TCPTransport) Write(p []byte) (n int, err error) {
	n, err = t.conn.Write(p)
	if err != nil {
		return n, err
	}
	if n < len(p) {
		return n, fmt.Errorf("short write on tcp transport, wrote %d of %d bytes", n, len(p))
	}
	return n, nil
}

// GetProto returns "TCP".
func (t *TCPTransport) GetProto() string {
	return "TCP"
}

// GetRemoteAddr returns the remote network address of the connection.
func (t *TCPTransport) GetRemoteAddr() net.Addr {
	return t.conn.RemoteAddr()
}

// Close terminates the TCP connection.
func (t *TCPTransport) Close() error {
	return t.conn.Close()
}
