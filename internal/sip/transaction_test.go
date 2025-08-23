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

	tx, err := NewNonInviteServerTx(req, transport, remoteAddr, "UDP")
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
	tx, err := NewInviteServerTx(req, transport, remoteAddr, "UDP")
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

// TestNonInviteServerTransactionTCP tests that the transaction terminates immediately for TCP.
func TestNonInviteServerTransactionTCP(t *testing.T) {
	reqStr := "REGISTER sip:test SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 1.1.1.1:5060;branch=z9hG4bK-tcp-1\r\n" +
		"To: a\r\n" +
		"From: b\r\n" +
		"CSeq: 1 REGISTER\r\n" +
		"Call-ID: 3\r\n" +
		"Content-Length: 0\r\n\r\n"
	req, err := ParseSIPRequest(reqStr)
	if err != nil {
		t.Fatalf("Failed to parse request: %v", err)
	}

	transport := newMockPacketConn()
	remoteAddr := &net.TCPAddr{IP: net.ParseIP("1.1.1.1"), Port: 5060}

	tx, err := NewNonInviteServerTx(req, transport, remoteAddr, "TCP")
	if err != nil {
		t.Fatalf("Failed to create transaction: %v", err)
	}

	tuReq := <-tx.Requests()
	res := BuildResponse(200, "OK", tuReq, nil)
	err = tx.Respond(res)
	if err != nil {
		t.Fatalf("TU failed to send response: %v", err)
	}

	// For TCP, transaction should terminate almost immediately.
	select {
	case <-tx.Done():
		// Success
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Transaction did not terminate immediately for TCP")
	}
}

// TestInviteServerTransactionNoRetransmissionTCP tests that final responses are not retransmitted for TCP.
func TestInviteServerTransactionNoRetransmissionTCP(t *testing.T) {
	T1 = 50 * time.Millisecond // Make timer G interval short if it were to run

	reqStr := "INVITE sip:test SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 1.1.1.1:5060;branch=z9hG4bK-tcp-2\r\n" +
		"To: a\r\n" +
		"From: b\r\n" +
		"CSeq: 1 INVITE\r\n" +
		"Call-ID: 4\r\n" +
		"Content-Length: 0\r\n\r\n"
	req, err := ParseSIPRequest(reqStr)
	if err != nil {
		t.Fatalf("Failed to parse request: %v", err)
	}

	transport := newMockPacketConn()
	remoteAddr := &net.TCPAddr{IP: net.ParseIP("1.1.1.1"), Port: 5060}

	tx, err := NewInviteServerTx(req, transport, remoteAddr, "TCP")
	if err != nil {
		t.Fatalf("Failed to create transaction: %v", err)
	}
	<-tx.Requests() // Consume request

	// It should have sent 100 Trying
	transport.getLastWritten(100 * time.Millisecond)

	// TU sends a final non-2xx response
	res := BuildResponse(503, "Service Unavailable", req, nil)
	err = tx.Respond(res)
	if err != nil {
		t.Fatalf("TU failed to send response: %v", err)
	}

	// Check that the 503 was sent once
	sentData, ok := transport.getLastWritten(100 * time.Millisecond)
	if !ok || !strings.Contains(sentData, "503 Service Unavailable") {
		t.Fatal("Transport did not write the initial 503 response")
	}

	// Now check that it is NOT retransmitted
	sentData, ok = transport.getLastWritten(100 * time.Millisecond) // T1*2 would be 100ms
	if ok {
		t.Fatalf("Transport retransmitted response over TCP: %s", sentData)
	}
}

// TestInviteClientTransactionSendsAckForNon2xx verifies that the client transaction
// sends an ACK for a non-2xx final response, as per RFC 3261.
func TestInviteClientTransactionSendsAckForNon2xx(t *testing.T) {
	// Use TCP so that Timer D is 0, allowing the transaction to terminate quickly.
	T1 = 50 * time.Millisecond

	reqStr := "INVITE sip:test SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 1.1.1.1:5060;branch=z9hG4bK-client-ack\r\n" +
		"To: a\r\n" +
		"From: b;tag=from-tag\r\n" +
		"CSeq: 1 INVITE\r\n" +
		"Call-ID: 5\r\n" +
		"Content-Length: 0\r\n\r\n"
	req, err := ParseSIPRequest(reqStr)
	if err != nil {
		t.Fatalf("Failed to parse request: %v", err)
	}

	transport := newMockPacketConn()
	remoteAddr := &net.TCPAddr{IP: net.ParseIP("2.2.2.2"), Port: 5060}

	// Create the client transaction
	tx, err := NewInviteClientTx(req, transport, remoteAddr, "TCP")
	if err != nil {
		t.Fatalf("Failed to create transaction: %v", err)
	}

	// 1. Verify the initial INVITE was sent
	sentData, ok := transport.getLastWritten(100 * time.Millisecond)
	if !ok {
		t.Fatal("Transport did not write the initial INVITE")
	}
	if !strings.Contains(sentData, "INVITE sip:test") {
		t.Errorf("Expected INVITE, but got: %s", sentData)
	}

	// 2. Simulate receiving a 401 Unauthorized response
	res := &SIPResponse{
		Proto:      "SIP/2.0",
		StatusCode: 401,
		Reason:     "Unauthorized",
		Headers: map[string]string{
			"Via":     "SIP/2.0/TCP 1.1.1.1:5060;branch=z9hG4bK-client-ack",
			"To":      "a;tag=to-tag",
			"From":    "b;tag=from-tag",
			"CSeq":    "1 INVITE",
			"Call-ID": "5",
		},
		Body: []byte{},
	}
	tx.ReceiveResponse(res)

	// 3. Verify the response was passed to the TU
	select {
	case tuRes := <-tx.Responses():
		if tuRes.StatusCode != 401 {
			t.Errorf("Expected status 401 from TU, got %d", tuRes.StatusCode)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Transaction did not pass response to TU")
	}

	// 4. Verify that an ACK was sent by the transaction
	sentData, ok = transport.getLastWritten(50 * time.Millisecond)
	if !ok {
		t.Fatal("Transaction did not send ACK for non-2xx final response")
	}
	if !strings.Contains(sentData, "ACK sip:test") {
		t.Fatalf("Transaction sent incorrect message instead of ACK: %s", sentData)
	}
	parsedAck, err := ParseSIPRequest(sentData)
	if err != nil {
		t.Fatalf("Failed to parse sent ACK: %v", err)
	}
	if parsedAck.GetHeader("CSeq") != "1 ACK" {
		t.Errorf("ACK has wrong CSeq: %s", parsedAck.GetHeader("CSeq"))
	}
	if parsedAck.GetHeader("To") != "a;tag=to-tag" {
		t.Errorf("ACK has wrong To header: %s", parsedAck.GetHeader("To"))
	}

	// 5. For TCP, Timer D is 0, so it should terminate very quickly.
	select {
	case <-tx.Done():
		// Success
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Transaction did not terminate after non-2xx final response")
	}
}

// TestInviteClientTimerAStop tests that Timer A stops firing after a
// provisional response is received, as per RFC 3261.
func TestInviteClientTimerAStop(t *testing.T) {
	T1 = 50 * time.Millisecond

	reqStr := "INVITE sip:test SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 1.1.1.1:5060;branch=z9hG4bK-timer-a-test\r\n" +
		"To: a\r\n" +
		"From: b\r\n" +
		"CSeq: 1 INVITE\r\n" +
		"Call-ID: 7\r\n" +
		"Content-Length: 0\r\n\r\n"
	req, err := ParseSIPRequest(reqStr)
	if err != nil {
		t.Fatalf("Failed to parse request: %v", err)
	}

	transport := newMockPacketConn()
	remoteAddr := &net.UDPAddr{IP: net.ParseIP("2.2.2.2"), Port: 5060}

	// Create the client transaction
	tx, err := NewInviteClientTx(req, transport, remoteAddr, "UDP")
	if err != nil {
		t.Fatalf("Failed to create transaction: %v", err)
	}
	defer tx.Terminate()

	// 1. Verify the initial INVITE was sent and consume it
	_, ok := transport.getLastWritten(100 * time.Millisecond)
	if !ok {
		t.Fatal("Transport did not write the initial INVITE")
	}

	// 2. Simulate receiving a 180 Ringing response
	res := BuildResponse(180, "Ringing", req, nil)
	tx.ReceiveResponse(res)

	// 3. Verify the provisional response was passed to the TU
	select {
	case tuRes := <-tx.Responses():
		if tuRes.StatusCode != 180 {
			t.Errorf("Expected status 180 from TU, got %d", tuRes.StatusCode)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Transaction did not pass provisional response to TU")
	}

	// 4. Verify that the INVITE is NOT retransmitted
	// The first retransmission would happen after T1 (50ms). We wait a bit longer.
	sentData, ok := transport.getLastWritten(100 * time.Millisecond)
	if ok {
		t.Fatalf("Transport incorrectly retransmitted INVITE after provisional response: %s", sentData)
	}
}

// TestInviteServerTransactionTerminatesOn2xx tests that after sending a 2xx
// response, the transaction terminates immediately for any transport.
func TestInviteServerTransactionTerminatesOn2xx(t *testing.T) {
	tests := []struct {
		name  string
		proto string
	}{
		{"UDP", "UDP"},
		{"TCP", "TCP"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqStr := "INVITE sip:test SIP/2.0\r\n" +
				"Via: SIP/2.0/" + tt.proto + " 1.1.1.1:5060;branch=z9hG4bK-2xx-term\r\n" +
				"To: a\r\n" +
				"From: b\r\n" +
				"CSeq: 1 INVITE\r\n" +
				"Call-ID: 6\r\n" +
				"Content-Length: 0\r\n\r\n"
			req, err := ParseSIPRequest(reqStr)
			if err != nil {
				t.Fatalf("Failed to parse request: %v", err)
			}

			transport := newMockPacketConn()
			var remoteAddr net.Addr
			if tt.proto == "UDP" {
				remoteAddr = &net.UDPAddr{IP: net.ParseIP("1.1.1.1"), Port: 5060}
			} else {
				remoteAddr = &net.TCPAddr{IP: net.ParseIP("1.1.1.1"), Port: 5060}
			}

			// Create transaction
			tx, err := NewInviteServerTx(req, transport, remoteAddr, tt.proto)
			if err != nil {
				t.Fatalf("Failed to create transaction: %v", err)
			}
			<-tx.Requests()                               // Consume request from TU
			transport.getLastWritten(100 * time.Millisecond) // Consume the 100 Trying

			// TU sends a 200 OK response
			res := BuildResponse(200, "OK", req, nil)
			err = tx.Respond(res)
			if err != nil {
				t.Fatalf("TU failed to send response: %v", err)
			}

			// Check that the 200 OK was "sent"
			sentData, ok := transport.getLastWritten(100 * time.Millisecond)
			if !ok {
				t.Fatal("Transport did not write 200 OK response")
			}
			if !strings.Contains(sentData, "SIP/2.0 200 OK") {
				t.Errorf("Expected 200 OK, but got: %s", sentData)
			}

			// Transaction should be terminated now for any transport
			select {
			case <-tx.Done():
				// Success
			case <-time.After(50 * time.Millisecond):
				t.Fatalf("Transaction did not terminate immediately after 2xx response on %s", tt.proto)
			}
		})
	}
}
