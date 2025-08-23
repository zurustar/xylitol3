package sip

import (
	"net"
	"sip-server/internal/storage"
	"strings"
	"testing"
	"time"
)



func TestStatefulProxy(t *testing.T) {
	// Use short timers for testing
	T1 = 10 * time.Millisecond
	T2 = 20 * time.Millisecond
	T4 = 20 * time.Millisecond


	s, _ := storage.NewStorage(":memory:")
	defer s.Close()
	realm := "test.com"
	server := NewSIPServer(s, realm)
	server.listenAddr = "127.0.0.1:5060"

	// Add a registration for 'bob'
	server.registrations["bob"] = Registration{
		ContactURI: "<sip:bob@192.168.1.100:5061>",
		ExpiresAt:  time.Now().Add(1 * time.Hour),
	}

	clientAddr, _ := net.ResolveUDPAddr("udp", "192.168.0.1:5060")

	tests := []struct {
		name              string
		request           string
		expectForward     bool
		expectResponseCode string
		expectForwardTo   string
		expectForwardContains []string
	}{
		{
			name: "User Registered",
			request: "INVITE sip:bob@test.com SIP/2.0\r\n" +
				"Via: SIP/2.0/UDP 192.168.0.1:5060;branch=z9hG4bK-registered\r\n" +
				"From: <sip:alice@test.com>\r\n" +
				"To: <sip:bob@test.com>\r\n" +
				"Call-ID: registered-call\r\n" +
				"CSeq: 1 INVITE\r\n" +
				"Max-Forwards: 70\r\n" +
				"Content-Length: 0\r\n\r\n",
			expectForward:   true,
			expectForwardTo: "192.168.1.100:5061",
			expectForwardContains: []string{
				"Max-Forwards: 69",
				"Via: SIP/2.0/UDP 127.0.0.1:5060;", // Proxys via
				"Record-Route: <sip:127.0.0.1:5060;lr>",
			},
		},
		{
			name: "User Not Registered",
			request: "INVITE sip:charlie@test.com SIP/2.0\r\n" +
				"Via: SIP/2.0/UDP 192.168.0.1:5060;branch=z9hG4bK-not-registered\r\n" +
				"From: <sip:alice@test.com>\r\n" +
				"To: <sip:charlie@test.com>\r\n" +
				"Call-ID: not-registered-call\r\n" +
				"CSeq: 1 INVITE\r\n" +
				"Max-Forwards: 70\r\n" +
				"Content-Length: 0\r\n\r\n",
			expectForward:      false,
			expectResponseCode: "SIP/2.0 480",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := newMockPacketConn()

			// Simulate receiving the request
			server.handleRequest(transport, tt.request, clientAddr)

			// For INVITEs, a 100 Trying is sent first.
			tryingMsg, ok := transport.getLastWritten(100 * time.Millisecond)
			if !ok {
				t.Fatal("Server did not send 100 Trying")
			}
			if !strings.Contains(tryingMsg, "SIP/2.0 100 Trying") {
				t.Fatalf("Expected 100 Trying, but got: %s", tryingMsg)
			}

			// Now check for the real response/forwarded request
			writtenMsg, ok := transport.getLastWritten(200 * time.Millisecond)
			if !ok {
				t.Fatal("Server did not send a second message after 100 Trying")
			}

			if tt.expectForward {
				if !strings.Contains(writtenMsg, "INVITE") {
					t.Errorf("Expected a forwarded INVITE, but got:\n%s", writtenMsg)
				}
				for _, substr := range tt.expectForwardContains {
					if !strings.Contains(writtenMsg, substr) {
						t.Errorf("Forwarded message does not contain '%s'. Message:\n%s", substr, writtenMsg)
					}
				}
			} else {
				if !strings.HasPrefix(writtenMsg, tt.expectResponseCode) {
					t.Errorf("Expected response code '%s', but got:\n%s", tt.expectResponseCode, writtenMsg)
				}
			}
		})
	}
}
