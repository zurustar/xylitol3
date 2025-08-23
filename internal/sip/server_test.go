package sip

import (
	"context"
	"fmt"
	"net"
	"sip-server/internal/storage"
	"strings"
	"testing"
	"time"
)

// mustReadFrom is a helper that reads from a packet connection with a timeout.
func mustReadFrom(t *testing.T, conn net.PacketConn, timeout time.Duration) (string, net.Addr) {
	t.Helper()
	buf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(timeout))
	n, addr, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("Failed to read from %s: %v", conn.LocalAddr(), err)
	}
	return string(buf[:n]), addr
}

func TestFullCallFlow(t *testing.T) {
	// 1. --- SETUP ---
	// Use short timers for testing to speed up failure detection
	T1 = 50 * time.Millisecond
	T2 = 100 * time.Millisecond
	T4 = 100 * time.Millisecond

	// Setup Server
	s, _ := storage.NewStorage(":memory:")
	defer s.Close()
	realm := "go-sip.test"
	server := NewSIPServer(s, realm)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server on a real UDP port
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start server listener: %v", err)
	}
	defer serverConn.Close()
	server.listenAddr = serverConn.LocalAddr().String()

	// Run the server's request handler loop in the background
	go func() {
		buf := make([]byte, 4096)
		for {
			if ctx.Err() != nil {
				return
			}
			n, clientAddr, err := serverConn.ReadFrom(buf)
			if err != nil {
				continue
			}
			message := string(buf[:n])
			go server.dispatchMessage(serverConn, message, clientAddr)
		}
	}()

	// Setup Mock UAs (Alice and Bob)
	alice, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create alice UA: %v", err)
	}
	defer alice.Close()

	bob, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create bob UA: %v", err)
	}
	defer bob.Close()

	// Manually register Bob so the proxy knows his location
	bobContactURI := fmt.Sprintf("sip:bob@%s", bob.LocalAddr().String())
	server.registrations["bob"] = Registration{
		ContactURI: bobContactURI,
		ExpiresAt:  time.Now().Add(1 * time.Hour),
	}

	// 2. --- INVITE Transaction ---
	t.Run("INVITE Transaction", func(t *testing.T) {
		// Alice sends INVITE to Bob via the proxy
		inviteBranch := "z9hG4bK-alice-invite"
		inviteCallID := "call-id-1234"
		inviteReq := fmt.Sprintf(
			"INVITE sip:bob@%s SIP/2.0\r\n"+
				"Via: SIP/2.0/UDP %s;branch=%s\r\n"+
				"From: <sip:alice@%s>;tag=alice-tag\r\n"+
				"To: <sip:bob@%s>\r\n"+
				"Call-ID: %s\r\n"+
				"CSeq: 1 INVITE\r\n"+
				"Max-Forwards: 70\r\n"+
				"Record-Route: <sip:ignored@someotherproxy.com;lr>\r\n"+ // Test that we prepend our own
				"Content-Length: 0\r\n\r\n",
			realm, alice.LocalAddr().String(), inviteBranch, realm, realm, inviteCallID,
		)

		_, err = alice.WriteTo([]byte(inviteReq), serverConn.LocalAddr())
		if err != nil {
			t.Fatalf("Alice failed to send INVITE: %v", err)
		}

		// Read the forwarded INVITE at Bob's UA
		fwdInvite, _ := mustReadFrom(t, bob, 500*time.Millisecond)

		// Assertions for the forwarded INVITE
		if !strings.Contains(fwdInvite, "INVITE "+bobContactURI) {
			t.Errorf("Request-URI was not rewritten. Expected '%s', got request line: %s", bobContactURI, strings.Split(fwdInvite, "\r\n")[0])
		}
		if !strings.Contains(fwdInvite, "Max-Forwards: 69") {
			t.Errorf("Max-Forwards was not decremented. Full message:\n%s", fwdInvite)
		}
		if !strings.Contains(fwdInvite, "Record-Route: <sip:"+server.listenAddr+";lr>") {
			t.Errorf("Proxy did not add its Record-Route header. Full message:\n%s", fwdInvite)
		}
		if !strings.Contains(fwdInvite, "Via: SIP/2.0/UDP "+server.listenAddr) {
			t.Errorf("Proxy did not add its Via header. Full message:\n%s", fwdInvite)
		}

		// Bob sends 200 OK back to the proxy
		fwdInviteReq, _ := ParseSIPRequest(fwdInvite)
		bobToTag := "bob-tag-9876"
		okResp := fmt.Sprintf(
			"SIP/2.0 200 OK\r\n"+
				"Via: %s\r\n"+
				"From: %s\r\n"+
				"To: %s;tag=%s\r\n"+
				"Call-ID: %s\r\n"+
				"CSeq: 1 INVITE\r\n"+
				"Contact: <sip:bob@%s>\r\n"+
				"Content-Length: 0\r\n\r\n",
			fwdInviteReq.GetHeader("Via"), fwdInviteReq.GetHeader("From"), fwdInviteReq.GetHeader("To"), bobToTag, inviteCallID, bob.LocalAddr().String(),
		)

		_, err = bob.WriteTo([]byte(okResp), serverConn.LocalAddr())
		if err != nil {
			t.Fatalf("Bob failed to send 200 OK: %v", err)
		}

		// Read the 100 Trying at Alice's UA, which is sent immediately by the InviteServerTx.
		trying, _ := mustReadFrom(t, alice, 500*time.Millisecond)
		if !strings.HasPrefix(trying, "SIP/2.0 100 Trying") {
			t.Fatalf("Expected 100 Trying, but got:\n%s", trying)
		}

		// Now, read the forwarded 200 OK from Bob.
		fwdOK, _ := mustReadFrom(t, alice, 500*time.Millisecond)

		// Assertions for the forwarded 200 OK
		if !strings.HasPrefix(fwdOK, "SIP/2.0 200 OK") {
			t.Fatalf("Expected 200 OK, got:\n%s", fwdOK)
		}
		// Check that the proxy's Via was removed
		if strings.Contains(fwdOK, server.listenAddr) {
			t.Errorf("Proxy did not remove its Via header from response. Full message:\n%s", fwdOK)
		}
		if !strings.Contains(fwdOK, "Via: SIP/2.0/UDP "+alice.LocalAddr().String()) {
			t.Errorf("Original Via header is missing. Full message:\n%s", fwdOK)
		}

		// 3. --- ACK Transaction ---
		// Alice sends ACK to Bob via the proxy
		ackReq := fmt.Sprintf(
			"ACK %s SIP/2.0\r\n"+
				"Via: SIP/2.0/UDP %s;branch=z9hG4bK-alice-ack\r\n"+
				"Route: <sip:%s;lr>, <sip:ignored@someotherproxy.com;lr>\r\n"+ // Route based on Record-Route
				"From: <sip:alice@%s>;tag=alice-tag\r\n"+
				"To: <sip:bob@%s>;tag=%s\r\n"+
				"Call-ID: %s\r\n"+
				"CSeq: 1 ACK\r\n"+
				"Max-Forwards: 70\r\n"+
				"Content-Length: 0\r\n\r\n",
			bobContactURI, alice.LocalAddr().String(), server.listenAddr, realm, realm, bobToTag, inviteCallID,
		)

		_, err = alice.WriteTo([]byte(ackReq), serverConn.LocalAddr())
		if err != nil {
			t.Fatalf("Alice failed to send ACK: %v", err)
		}

		// Read the forwarded ACK at Bob's UA
		fwdAck, _ := mustReadFrom(t, bob, 500*time.Millisecond)

		// Assertions for forwarded ACK
		if !strings.Contains(fwdAck, "Route: <sip:ignored@someotherproxy.com;lr>") {
			t.Errorf("Proxy did not strip its Route header from ACK. Full message:\n%s", fwdAck)
		}
		if !strings.Contains(fwdAck, "Via: SIP/2.0/UDP "+server.listenAddr) {
			t.Errorf("Proxy did not add its Via header to ACK. Full message:\n%s", fwdAck)
		}

		// 4. --- BYE Transaction ---
		// Alice sends BYE to Bob via the proxy
		byeReq := fmt.Sprintf(
			"BYE %s SIP/2.0\r\n"+
				"Via: SIP/2.0/UDP %s;branch=z9hG4bK-alice-bye\r\n"+
				"Route: <sip:%s;lr>, <sip:ignored@someotherproxy.com;lr>\r\n"+ // Route based on Record-Route
				"From: <sip:alice@%s>;tag=alice-tag\r\n"+
				"To: <sip:bob@%s>;tag=%s\r\n"+
				"Call-ID: %s\r\n"+
				"CSeq: 2 BYE\r\n"+
				"Max-Forwards: 70\r\n"+
				"Content-Length: 0\r\n\r\n",
			bobContactURI, alice.LocalAddr().String(), server.listenAddr, realm, realm, bobToTag, inviteCallID,
		)
		_, err = alice.WriteTo([]byte(byeReq), serverConn.LocalAddr())
		if err != nil {
			t.Fatalf("Alice failed to send BYE: %v", err)
		}

		// Read the forwarded BYE at Bob's UA
		fwdBye, _ := mustReadFrom(t, bob, 500*time.Millisecond)

		// Assert forwarded BYE
		if !strings.Contains(fwdBye, "BYE "+bobContactURI) {
			t.Errorf("BYE was not forwarded correctly. Full message:\n%s", fwdBye)
		}
		if !strings.Contains(fwdBye, "Route: <sip:ignored@someotherproxy.com;lr>") {
			t.Errorf("Proxy did not strip its Route header from BYE. Full message:\n%s", fwdBye)
		}

		// Bob sends 200 OK for BYE
		fwdByeReq, _ := ParseSIPRequest(fwdBye)
		byeOKResp := fmt.Sprintf(
			"SIP/2.0 200 OK\r\n"+
				"Via: %s\r\n"+
				"From: %s\r\n"+
				"To: %s\r\n"+
				"Call-ID: %s\r\n"+
				"CSeq: 2 BYE\r\n"+
				"Content-Length: 0\r\n\r\n",
			fwdByeReq.GetHeader("Via"), fwdByeReq.GetHeader("From"), fwdByeReq.GetHeader("To"), inviteCallID,
		)
		_, err = bob.WriteTo([]byte(byeOKResp), serverConn.LocalAddr())
		if err != nil {
			t.Fatalf("Bob failed to send 200 OK for BYE: %v", err)
		}

		// Read the 200 OK at Alice's UA
		fwdByeOK, _ := mustReadFrom(t, alice, 500*time.Millisecond)
		if !strings.HasPrefix(fwdByeOK, "SIP/2.0 200 OK") {
			t.Fatalf("Expected 200 OK for BYE, got:\n%s", fwdByeOK)
		}
		if strings.Contains(fwdByeOK, server.listenAddr) {
			t.Errorf("Proxy did not remove its Via header from BYE response. Full message:\n%s", fwdByeOK)
		}
	})
}
