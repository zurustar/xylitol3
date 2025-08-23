package sip

import (
	"net"
	"sip-server/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestHandleProxy_UserRegistered(t *testing.T) {
	// --- Setup ---
	s, _ := storage.NewStorage(":memory:") // In-memory SQLite for testing
	defer s.Close()

	realm := "test.com"
	server := NewServer(s, realm)
	server.listenAddr = "127.0.0.1:5060"

	// Add a registration for the user we are calling
	registeredUser := "bob"
	contactURI := "<sip:bob@192.168.1.100:5061>"
	server.registrations[registeredUser] = Registration{
		ContactURI: contactURI,
		ExpiresAt:  time.Now().Add(1 * time.Hour),
	}

	// Create a sample INVITE request from alice to bob
	rawInvite := []string{
		"INVITE sip:bob@test.com SIP/2.0",
		"Via: SIP/2.0/UDP 192.168.0.1:5060;branch=z9hG4bK-abc",
		"From: <sip:alice@test.com>",
		"To: <sip:bob@test.com>",
		"Call-ID: 12345",
		"CSeq: 1 INVITE",
		"Max-Forwards: 70",
		"Content-Length: 0",
		"",
		"",
	}
	req, err := ParseSIPRequest(strings.Join(rawInvite, "\r\n"))
	if err != nil {
		t.Fatalf("Failed to parse test request: %v", err)
	}

	// --- Execute ---
	clientAddr, _ := net.ResolveUDPAddr("udp", "192.168.0.1:5060")
	forwardedMsg, destAddr, err := server.handleProxy(req, clientAddr)

	// --- Assert ---
	if err != nil {
		t.Fatalf("handleProxy returned an unexpected error: %v", err)
	}

	// Check destination address
	expectedDestAddr, _ := net.ResolveUDPAddr("udp", "192.168.1.100:5061")
	if destAddr.String() != expectedDestAddr.String() {
		t.Errorf("Expected destination address %s, but got %s", expectedDestAddr, destAddr)
	}

	// Check forwarded message content
	if !strings.Contains(forwardedMsg, "Max-Forwards: 69") {
		t.Errorf("Max-Forwards was not decremented. Message:\n%s", forwardedMsg)
	}
	if !strings.Contains(forwardedMsg, "Via: SIP/2.0/UDP 127.0.0.1:5060;") {
		t.Errorf("Proxy Via header was not added. Message:\n%s", forwardedMsg)
	}
	if !strings.Contains(forwardedMsg, "Record-Route: <sip:127.0.0.1:5060;lr>") {
		t.Errorf("Record-Route header was not added. Message:\n%s", forwardedMsg)
	}
}

func TestHandleProxy_UserNotRegistered(t *testing.T) {
	// --- Setup ---
	s, _ := storage.NewStorage(":memory:")
	defer s.Close()

	realm := "test.com"
	server := NewServer(s, realm)

	// No registration for "charlie"

	// Create a sample INVITE request from alice to charlie
	rawInvite := []string{
		"INVITE sip:charlie@test.com SIP/2.0",
		"Via: SIP/2.0/UDP 192.168.0.1:5060;branch=z9hG4bK-def",
		"From: <sip:alice@test.com>",
		"To: <sip:charlie@test.com>",
		"Call-ID: 67890",
		"CSeq: 1 INVITE",
		"Max-Forwards: 70",
		"Content-Length: 0",
		"",
		"",
	}
	req, err := ParseSIPRequest(strings.Join(rawInvite, "\r\n"))
	if err != nil {
		t.Fatalf("Failed to parse test request: %v", err)
	}

	// --- Execute ---
	clientAddr, _ := net.ResolveUDPAddr("udp", "192.168.0.1:5060")
	responseMsg, destAddr, err := server.handleProxy(req, clientAddr)

	// --- Assert ---
	if err != nil {
		t.Fatalf("handleProxy returned an unexpected error: %v", err)
	}

	// Check destination address (should be back to the original client)
	if destAddr.String() != clientAddr.String() {
		t.Errorf("Expected destination address %s, but got %s", clientAddr, destAddr)
	}

	// Check response message
	if !strings.HasPrefix(responseMsg, "SIP/2.0 480 Temporarily Unavailable") {
		t.Errorf("Expected 480 response, but got:\n%s", responseMsg)
	}
}
