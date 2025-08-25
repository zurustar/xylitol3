package sip

import (
	"context"
	"fmt"
	"net"
	"sip-server/internal/storage"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseSessionExpires(t *testing.T) {
	t.Run("DeltaOnly", func(t *testing.T) {
		se, err := ParseSessionExpires("1800")
		assert.NoError(t, err)
		assert.NotNil(t, se)
		assert.Equal(t, 1800, se.Delta)
		assert.Empty(t, se.Refresher)
	})

	t.Run("WithRefresherUAC", func(t *testing.T) {
		se, err := ParseSessionExpires("90;refresher=uac")
		assert.NoError(t, err)
		assert.NotNil(t, se)
		assert.Equal(t, 90, se.Delta)
		assert.Equal(t, "uac", se.Refresher)
	})

	t.Run("WithRefresherUAS", func(t *testing.T) {
		se, err := ParseSessionExpires("3600; refresher = uas")
		assert.NoError(t, err)
		assert.NotNil(t, se)
		assert.Equal(t, 3600, se.Delta)
		assert.Equal(t, "uas", se.Refresher)
	})

	t.Run("InvalidDelta", func(t *testing.T) {
		_, err := ParseSessionExpires("not-a-number;refresher=uac")
		assert.Error(t, err)
	})
}

func TestRequestSessionTimerHeaders(t *testing.T) {
	t.Run("Parses Session-Expires and Min-SE", func(t *testing.T) {
		req := &SIPRequest{
			Headers: make(map[string]string),
		}
		req.Headers["Session-Expires"] = "120;refresher=uac"
		req.Headers["Min-SE"] = "90"

		se, err := req.SessionExpires()
		assert.NoError(t, err)
		assert.Equal(t, 120, se.Delta)
		assert.Equal(t, "uac", se.Refresher)

		minSE, err := req.MinSE()
		assert.NoError(t, err)
		assert.Equal(t, 90, minSE)
	})
}

func TestSipProxy_SessionTimer_RejectsLowSE(t *testing.T) {
	// --- SETUP ---
	s, _ := storage.NewStorage(":memory:")
	defer s.Close()
	realm := "go-sip.test"
	server := NewSIPServer(s, realm, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	assert.NoError(t, err)
	defer serverConn.Close()
	server.listenAddr = serverConn.LocalAddr().String()
	server.udpConn = serverConn

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

	alice, err := net.ListenPacket("udp", "127.0.0.1:0")
	assert.NoError(t, err)
	defer alice.Close()

	// Register bob so the proxy can find him
	server.registrations["bob"] = []Registration{{
		ContactURI: "sip:bob@192.0.2.2:5060",
		ExpiresAt:  time.Now().Add(time.Hour),
	}}

	// --- TEST ---
	inviteReq := fmt.Sprintf(
		"INVITE sip:bob@%s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP %s;branch=z9hG4bK-low-se\r\n"+
			"From: Alice <sip:alice@%s>;tag=alice-tag\r\n"+
			"To: Bob <sip:bob@%s>\r\n"+
			"Call-ID: call-id-low-se\r\n"+
			"CSeq: 1 INVITE\r\n"+
			"Supported: timer\r\n"+
			"Session-Expires: 90\r\n"+
			"Content-Length: 0\r\n\r\n",
		realm, alice.LocalAddr().String(), realm, realm,
	)

	_, err = alice.WriteTo([]byte(inviteReq), serverConn.LocalAddr())
	assert.NoError(t, err)

	// Read 100 Trying first
	tryingRes, _, err := tryReadFrom(t, alice, 200*time.Millisecond)
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(tryingRes, "SIP/2.0 100 Trying"))

	// Read final response
	resStr, _, err := tryReadFrom(t, alice, 200*time.Millisecond)
	assert.NoError(t, err)

	// Assertions
	assert.True(t, strings.HasPrefix(resStr, "SIP/2.0 422 Session Interval Too Small"))
	res, err := ParseSIPResponse(resStr)
	assert.NoError(t, err)
	minSE, err := res.MinSE()
	assert.NoError(t, err)
	assert.Equal(t, 1800, minSE)
}

func TestSipProxy_SessionTimer_HandlesDownstream422(t *testing.T) {
	// --- SETUP ---
	s, _ := storage.NewStorage(":memory:")
	defer s.Close()
	realm := "go-sip.test"
	server := NewSIPServer(s, realm, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	assert.NoError(t, err)
	defer serverConn.Close()
	server.listenAddr = serverConn.LocalAddr().String()
	server.udpConn = serverConn
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
			transport := NewUDPTransport(serverConn, clientAddr)
			go server.dispatchMessage(transport, string(buf[:n]))
		}
	}()

	alice, err := net.ListenPacket("udp", "127.0.0.1:0")
	assert.NoError(t, err)
	defer alice.Close()
	bob, err := net.ListenPacket("udp", "127.0.0.1:0")
	assert.NoError(t, err)
	defer bob.Close()

	server.registrations["bob"] = []Registration{{
		ContactURI: fmt.Sprintf("sip:bob@%s", bob.LocalAddr().String()),
		ExpiresAt:  time.Now().Add(time.Hour),
	}}

	// --- TEST ---
	// Bob's UAS will reject the first INVITE with 422
	go func() {
		// 1. Read INVITE from proxy
		fwdInviteStr, proxyAddr, err := tryReadFrom(t, bob, 200*time.Millisecond)
		assert.NoError(t, err)
		fwdInvite, _ := ParseSIPRequest(fwdInviteStr)

		// 2. Bob rejects with 422
		res422 := BuildResponse(422, "Session Interval Too Small", fwdInvite, map[string]string{"Min-SE": "2000"})
		_, err = bob.WriteTo([]byte(res422.String()), proxyAddr)
		assert.NoError(t, err)

		// 3. The proxy's client transaction will send an ACK for the 422. The UAS must read and ignore it.
		ackStr, _, err := tryReadFrom(t, bob, 200*time.Millisecond)
		assert.NoError(t, err)
		ackReq, _ := ParseSIPRequest(ackStr)
		assert.Equal(t, "ACK", ackReq.Method)

		// 4. Read retry INVITE from proxy
		retryInviteStr, _, err := tryReadFrom(t, bob, 200*time.Millisecond)
		assert.NoError(t, err)
		retryInvite, _ := ParseSIPRequest(retryInviteStr)
		assert.Equal(t, "2 INVITE", retryInvite.GetHeader("CSeq"))
		se, err := retryInvite.SessionExpires()
		assert.NoError(t, err)
		assert.NotNil(t, se)
		assert.Equal(t, 2000, se.Delta)

		// 5. Bob accepts the retry
		res200 := BuildResponse(200, "OK", retryInvite, map[string]string{
			"Session-Expires": "2000;refresher=uas",
			"Contact":         fmt.Sprintf("<sip:bob@%s>", bob.LocalAddr().String()),
		})
		_, err = bob.WriteTo([]byte(res200.String()), proxyAddr)
		assert.NoError(t, err)
	}()

	// Alice sends the initial INVITE
	inviteReq := fmt.Sprintf(
		"INVITE sip:bob@%s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP %s;branch=z9hG4bK-retry\r\n"+
			"From: Alice <sip:alice@%s>;tag=alice-tag\r\n"+
			"To: Bob <sip:bob@%s>\r\n"+
			"Call-ID: call-id-retry\r\n"+
			"CSeq: 1 INVITE\r\n"+
			"Supported: timer\r\n"+
			"Session-Expires: 1800\r\n"+
			"Content-Length: 0\r\n\r\n",
		realm, alice.LocalAddr().String(), realm, realm,
	)
	_, err = alice.WriteTo([]byte(inviteReq), serverConn.LocalAddr())
	assert.NoError(t, err)

	// Alice should receive a 100 Trying, then a 200 OK. She should NOT see the 422.
	tryingRes, _, err := tryReadFrom(t, alice, 200*time.Millisecond)
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(tryingRes, "SIP/2.0 100 Trying"))

	okResStr, _, err := tryReadFrom(t, alice, 500*time.Millisecond)
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(okResStr, "SIP/2.0 200 OK"))
	okRes, _ := ParseSIPResponse(okResStr)
	se, _ := okRes.SessionExpires()
	assert.Equal(t, 2000, se.Delta)
	assert.Equal(t, "uas", se.Refresher)
}

func TestSipProxy_SessionTimer_UASDoesNotSupport(t *testing.T) {
	// --- SETUP ---
	s, _ := storage.NewStorage(":memory:")
	defer s.Close()
	realm := "go-sip.test"
	server := NewSIPServer(s, realm, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	assert.NoError(t, err)
	defer serverConn.Close()
	server.listenAddr = serverConn.LocalAddr().String()
	server.udpConn = serverConn
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
			transport := NewUDPTransport(serverConn, clientAddr)
			go server.dispatchMessage(transport, string(buf[:n]))
		}
	}()

	alice, err := net.ListenPacket("udp", "127.0.0.1:0")
	assert.NoError(t, err)
	defer alice.Close()
	bob, err := net.ListenPacket("udp", "127.0.0.1:0")
	assert.NoError(t, err)
	defer bob.Close()

	server.registrations["bob"] = []Registration{{
		ContactURI: fmt.Sprintf("sip:bob@%s", bob.LocalAddr().String()),
		ExpiresAt:  time.Now().Add(time.Hour),
	}}

	// --- TEST ---
	// Bob's UAS will reply 200 OK but without any session timer headers
	go func() {
		fwdInviteStr, proxyAddr, err := tryReadFrom(t, bob, 200*time.Millisecond)
		assert.NoError(t, err)
		fwdInvite, _ := ParseSIPRequest(fwdInviteStr)
		res200 := BuildResponse(200, "OK", fwdInvite, map[string]string{
			"Contact": fmt.Sprintf("<sip:bob@%s>", bob.LocalAddr().String()),
		})
		_, err = bob.WriteTo([]byte(res200.String()), proxyAddr)
		assert.NoError(t, err)
	}()

	// Alice sends INVITE asking for a session timer
	inviteReq := fmt.Sprintf(
		"INVITE sip:bob@%s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP %s;branch=z9hG4bK-uas-no-support\r\n"+
			"From: Alice <sip:alice@%s>;tag=alice-tag\r\n"+
			"To: Bob <sip:bob@%s>\r\n"+
			"Call-ID: call-id-uas-no-support\r\n"+
			"CSeq: 1 INVITE\r\n"+
			"Supported: timer\r\n"+
			"Session-Expires: 2000\r\n"+
			"Content-Length: 0\r\n\r\n",
		realm, alice.LocalAddr().String(), realm, realm,
	)
	_, err = alice.WriteTo([]byte(inviteReq), serverConn.LocalAddr())
	assert.NoError(t, err)

	// Alice should receive a 100 Trying, then a 200 OK.
	tryingRes, _, err := tryReadFrom(t, alice, 200*time.Millisecond)
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(tryingRes, "SIP/2.0 100 Trying"))

	// The 200 OK she receives should have been modified by the proxy
	okResStr, _, err := tryReadFrom(t, alice, 200*time.Millisecond)
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(okResStr, "SIP/2.0 200 OK"))
	okRes, _ := ParseSIPResponse(okResStr)

	// Check that the proxy added the required headers
	se, _ := okRes.SessionExpires()
	assert.NotNil(t, se)
	assert.Equal(t, 2000, se.Delta)
	assert.Equal(t, "uac", se.Refresher)

	requireHeader := okRes.GetHeader("Require")
	assert.Contains(t, strings.ToLower(requireHeader), "timer")
}
