package sip

import (
	"net"
	"strings"
	"testing"
	"time"
)


// TestNonInviteServerTransactionHappyPath tests the basic trying -> completed flow.
func TestNonInviteServerTransactionHappyPath(t *testing.T) {
	// Use short timers for testing
	T1 = 50 * time.Millisecond
	T4 = 100 * time.Millisecond

	reqStr := "REGISTER sip:test SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 1.1.1.1:5060;branch=z9hG4bK-abc\r\n" +
		"To: a\r\n" +
		"From: b\r\n" +
		"CSeq: 1 REGISTER\r\n" +
		"Call-ID: 1\r\n" +
		"Content-Length: 0\r\n\r\n"
	req, err := ParseSIPRequest(reqStr)
	if err != nil {
		t.Fatalf("Failed to parse request: %v", err)
	}

	transport := newMockPacketConn()
	remoteAddr := &net.UDPAddr{IP: net.ParseIP("1.1.1.1"), Port: 5060}

	tx, err := NewNonInviteServerTx(req, transport, remoteAddr)
	if err != nil {
		t.Fatalf("Failed to create transaction: %v", err)
	}

	// TU receives request
	tuReq := <-tx.Requests()
	if tuReq.Method != "REGISTER" {
		t.Errorf("Expected REGISTER method, got %s", tuReq.Method)
	}

	// TU sends response
	res := BuildResponse(200, "OK", tuReq, nil)
	err = tx.Respond(res)
	if err != nil {
		t.Fatalf("TU failed to send response: %v", err)
	}

	// Check that the response was "sent"
	sentData, ok := transport.getLastWritten(100 * time.Millisecond)
	if !ok {
		t.Fatal("Transport did not write any data")
	}
	if !strings.Contains(sentData, "SIP/2.0 200 OK") {
		t.Errorf("Expected 200 OK, but got: %s", sentData)
	}

	// Wait for transaction to terminate (Timer J)
	select {
	case <-tx.Done():
		// Success
	case <-time.After(5 * time.Second): // 64*T1 is 3.2s, so 5s should be enough
		t.Fatal("Transaction did not terminate after Timer J")
	}
}

// TestInviteServerTransactionAckFlow tests the proceeding -> completed -> confirmed flow.
func TestInviteServerTransactionAckFlow(t *testing.T) {
	// Use short timers for testing
	T1 = 50 * time.Millisecond
	T4 = 100 * time.Millisecond

	reqStr := "INVITE sip:test SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 1.1.1.1:5060;branch=z9hG4bK-def\r\n" +
		"To: a\r\n" +
		"From: b\r\n" +
		"CSeq: 1 INVITE\r\n" +
		"Call-ID: 2\r\n" +
		"Content-Length: 0\r\n\r\n"
	req, err := ParseSIPRequest(reqStr)
	if err != nil {
		t.Fatalf("Failed to parse request: %v", err)
	}

	transport := newMockPacketConn()
	remoteAddr := &net.UDPAddr{IP: net.ParseIP("1.1.1.1"), Port: 5060}

	// Create transaction
	tx, err := NewInviteServerTx(req, transport, remoteAddr)
	if err != nil {
		t.Fatalf("Failed to create transaction: %v", err)
	}

	// It should have sent 100 Trying immediately
	sentData, ok := transport.getLastWritten(100 * time.Millisecond)
	if !ok {
		t.Fatal("Transport did not write 100 Trying")
	}
	if !strings.Contains(sentData, "SIP/2.0 100 Trying") {
		t.Errorf("Expected 100 Trying, but got: %s", sentData)
	}

	// TU receives request
	tuReq := <-tx.Requests()

	// TU sends a final non-2xx response
	res := BuildResponse(401, "Unauthorized", tuReq, nil)
	err = tx.Respond(res)
	if err != nil {
		t.Fatalf("TU failed to send response: %v", err)
	}

	// Check that the 401 was "sent"
	sentData, ok = transport.getLastWritten(100 * time.Millisecond)
	if !ok {
		t.Fatal("Transport did not write 401 response")
	}
	if !strings.Contains(sentData, "SIP/2.0 401 Unauthorized") {
		t.Errorf("Expected 401, but got: %s", sentData)
	}

	// Now we simulate receiving an ACK
	// The server logic would parse this and match it to the transaction
	ackStr := "ACK sip:test SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 1.1.1.1:5060;branch=z9hG4bK-def\r\n" +
		"To: a;tag=z9hG4bK-response-tag\r\n" + // Tag from the response
		"From: b\r\n" +
		"CSeq: 1 ACK\r\n" +
		"Call-ID: 2\r\n" +
		"Content-Length: 0\r\n\r\n"
	ackReq, _ := ParseSIPRequest(ackStr)
	tx.Receive(ackReq)


	// Wait for transaction to terminate (Timer I)
	select {
	case <-tx.Done():
		// Success
	case <-time.After(200 * time.Millisecond): // T4 is 100ms, so 200ms should be enough
		t.Fatal("Transaction did not terminate after Timer I")
	}
}
