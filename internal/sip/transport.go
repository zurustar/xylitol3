package sip

import (
	"fmt"
	"io"
	"net"
)

// Transport は、UDP、TCPなどのさまざまなネットワークプロトコルでバックアップできる
// SIPトランスポート層のインターフェースを定義します。
type Transport interface {
	// Writer は、完全なSIPメッセージをリモートピアに送信するために使用されます。
	io.Writer
	// GetProto は、トランスポートプロトコル名（例：「UDP」、「TCP」）を返します。
	GetProto() string
	// GetRemoteAddr は、リモートピアのネットワークアドレスを返します。
	GetRemoteAddr() net.Addr
	// Close は、該当する場合にトランスポート接続を終了します。
	Close() error
}

// --- UDPトランスポート ---

// UDPTransport は、UDPのトランスポート実装です。
type UDPTransport struct {
	conn     net.PacketConn
	destAddr net.Addr
}

// NewUDPTransport は、新しいUDPトランスポートインスタンスを作成します。
func NewUDPTransport(conn net.PacketConn, destAddr net.Addr) *UDPTransport {
	return &UDPTransport{
		conn:     conn,
		destAddr: destAddr,
	}
}

// Write は、UDP接続を介して宛先アドレスにデータを送信します。
func (t *UDPTransport) Write(p []byte) (n int, err error) {
	return t.conn.WriteTo(p, t.destAddr)
}

// GetProto は「UDP」を返します。
func (t *UDPTransport) GetProto() string {
	return "UDP"
}

// GetRemoteAddr は、宛先ネットワークアドレスを返します。
func (t *UDPTransport) GetRemoteAddr() net.Addr {
	return t.destAddr
}

// Close は、単一の「トランザクション」の観点からは何もしません。
// 基になるPacketConnはサーバーリスナーによって共有および管理されるためです。
func (t *UDPTransport) Close() error {
	return nil
}

// --- TCPトランスポート ---

// TCPTransport は、TCPのトランスポート実装です。
type TCPTransport struct {
	conn net.Conn
}

// NewTCPTransport は、新しいTCPトランスポートインスタンスを作成します。
func NewTCPTransport(conn net.Conn) *TCPTransport {
	return &TCPTransport{
		conn: conn,
	}
}

// Write は、TCP接続を介してデータを送信します。
// 提供されるバイトスライスは、完全に形成されたSIPメッセージである必要があります。
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

// GetProto は「TCP」を返します。
func (t *TCPTransport) GetProto() string {
	return "TCP"
}

// GetRemoteAddr は、接続のリモートネットワークアドレスを返します。
func (t *TCPTransport) GetRemoteAddr() net.Addr {
	return t.conn.RemoteAddr()
}

// Close は、TCP接続を終了します。
func (t *TCPTransport) Close() error {
	return t.conn.Close()
}
