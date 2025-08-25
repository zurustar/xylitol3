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

// tryReadFrom is a helper that reads from a packet connection with a timeout.
func tryReadFrom(t *testing.T, conn net.PacketConn, timeout time.Duration) (string, net.Addr, error) {
	t.Helper()
	buf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(timeout))
	n, addr, err := conn.ReadFrom(buf)
	if err != nil {
		return "", nil, err
	}
	return string(buf[:n]), addr, nil
}

func TestSipProxy_InviteCancelFlow(t *testing.T) {
	// 1. --- SETUP ---
	// Use short timers for testing to speed up failure detection
	T1 = 50 * time.Millisecond
	T2 = 100 * time.Millisecond
	T4 = 100 * time.Millisecond

	// Setup Server
	s, _ := storage.NewStorage(":memory:")
	defer s.Close()
	realm := "go-sip.test"
	server := NewSIPServer(s, realm, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server on a real UDP port
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start server listener: %v", err)
	}
	defer serverConn.Close()
	server.listenAddr = serverConn.LocalAddr().String()
	server.udpConn = serverConn // Set the connection for the proxy to use

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
			transport := NewUDPTransport(serverConn, clientAddr)
			go server.dispatchMessage(transport, message)
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
	server.registrations["bob"] = []Registration{{
		ContactURI: bobContactURI,
		ExpiresAt:  time.Now().Add(1 * time.Hour),
	}}

	// 2. --- INVITE and CANCEL Transaction ---
	inviteBranch := "z9hG4bK-invite-cancel"
	inviteCallID := "call-id-cancel-1234"
	inviteFrom := fmt.Sprintf("<sip:alice@%s>;tag=alice-tag", realm)
	inviteTo := fmt.Sprintf("<sip:bob@%s>", realm)

	// Alice sends INVITE to Bob via the proxy
	inviteReq := fmt.Sprintf(
		"INVITE sip:bob@%s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP %s;branch=%s\r\n"+
			"From: %s\r\n"+
			"To: %s\r\n"+
			"Call-ID: %s\r\n"+
			"CSeq: 1 INVITE\r\n"+
			"Max-Forwards: 70\r\n"+
			"Content-Length: 0\r\n\r\n",
		realm, alice.LocalAddr().String(), inviteBranch, inviteFrom, inviteTo, inviteCallID,
	)

	_, err = alice.WriteTo([]byte(inviteReq), serverConn.LocalAddr())
	if err != nil {
		t.Fatalf("Alice failed to send INVITE: %v", err)
	}

	// Read the forwarded INVITE at Bob's UA
	fwdInvite, _, err := tryReadFrom(t, bob, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Bob failed to receive forwarded INVITE: %v", err)
	}

	// Assertions for the forwarded INVITE
	if !strings.Contains(fwdInvite, "INVITE "+bobContactURI) {
		t.Errorf("Request-URI was not rewritten. Expected '%s', got request line: %s", bobContactURI, strings.Split(fwdInvite, "\r\n")[0])
	}

	// Read the 100 Trying at Alice's UA, which is sent immediately by the InviteServerTx.
	trying, _, err := tryReadFrom(t, alice, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Alice failed to receive 100 Trying: %v", err)
	}
	if !strings.HasPrefix(trying, "SIP/2.0 100 Trying") {
		t.Fatalf("Expected 100 Trying, but got:\n%s", trying)
	}

	// Bob does NOT answer. Instead, Alice sends a CANCEL.
	cancelReq := fmt.Sprintf(
		"CANCEL sip:bob@%s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP %s;branch=%s\r\n"+ // Must be identical to INVITE's Via
			"From: %s\r\n"+
			"To: %s\r\n"+
			"Call-ID: %s\r\n"+
			"CSeq: 1 CANCEL\r\n"+ // Same sequence number, different method
			"Content-Length: 0\r\n\r\n",
		realm, alice.LocalAddr().String(), inviteBranch, inviteFrom, inviteTo, inviteCallID,
	)
	_, err = alice.WriteTo([]byte(cancelReq), serverConn.LocalAddr())
	if err != nil {
		t.Fatalf("Alice failed to send CANCEL: %v", err)
	}

	// Bob should receive the proxied CANCEL request.
	fwdCancel, _, err := tryReadFrom(t, bob, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Bob failed to receive forwarded CANCEL: %v", err)
	}
	fwdCancelReq, err := ParseSIPRequest(fwdCancel)
	if err != nil {
		t.Fatalf("Failed to parse forwarded CANCEL: %v", err)
	}
	if fwdCancelReq.Method != "CANCEL" {
		t.Errorf("Expected forwarded request to be CANCEL, got %s", fwdCancelReq.Method)
	}

	// Alice should receive two responses: 200 OK for the CANCEL, and 487 for the INVITE.
	// The order is not guaranteed.
	var got200, got487 bool
	for i := 0; i < 2; i++ {
		res, _, err := tryReadFrom(t, alice, 500*time.Millisecond)
		if err != nil {
			t.Logf("Reading from alice connection failed, assuming no more messages: %v", err)
			break
		}
		t.Logf("Alice received: \n%s", res)
		if strings.HasPrefix(res, "SIP/2.0 200 OK") {
			resParsed, _ := ParseSIPResponse(res)
			if cseq := resParsed.GetHeader("CSeq"); strings.Contains(cseq, "CANCEL") {
				got200 = true
			}
		} else if strings.HasPrefix(res, "SIP/2.0 487 Request Terminated") {
			resParsed, _ := ParseSIPResponse(res)
			if cseq := resParsed.GetHeader("CSeq"); strings.Contains(cseq, "INVITE") {
				got487 = true
			}
		}
	}

	if !got200 {
		t.Error("Did not receive 200 OK for CANCEL")
	}
	if !got487 {
		t.Error("Did not receive 487 Request Terminated for INVITE")
	}

	// Allow time for server-side transactions to terminate gracefully.
	time.Sleep(200 * time.Millisecond)
}
