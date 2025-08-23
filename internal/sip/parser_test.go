package sip

import (
	"reflect"
	"testing"
)

func TestParseSIPRequest(t *testing.T) {
	t.Run("Valid REGISTER request", func(t *testing.T) {
		rawReq := "REGISTER sip:registrar.example.com SIP/2.0\r\n" +
			"Via: SIP/2.0/UDP client.example.com:5060;branch=z9hG4bK-1\r\n" +
			"From: \"Alice\" <sip:alice@example.com>;tag=9fxced76sl\r\n" +
			"To: \"Alice\" <sip:alice@example.com>\r\n" +
			"Call-ID: 1-2345@client.example.com\r\n" +
			"CSeq: 1 REGISTER\r\n" +
			"Authorization: Digest username=\"alice\", realm=\"registrar.example.com\", nonce=\"dcd98b7102dd2f0e8b11d0f600bfb0c093\", uri=\"sip:registrar.example.com\", response=\"6629fae49393a05397450978507c4ef1\"\r\n" +
			"Content-Length: 0\r\n\r\n"

		req, err := ParseSIPRequest(rawReq)
		if err != nil {
			t.Fatalf("ParseSIPRequest failed: %v", err)
		}

		if req.Method != "REGISTER" {
			t.Errorf("Expected method REGISTER, got %s", req.Method)
		}
		if req.URI != "sip:registrar.example.com" {
			t.Errorf("Expected URI sip:registrar.example.com, got %s", req.URI)
		}
		if req.Proto != "SIP/2.0" {
			t.Errorf("Expected proto SIP/2.0, got %s", req.Proto)
		}

		expectedFrom := "\"Alice\" <sip:alice@example.com>;tag=9fxced76sl"
		if from := req.GetHeader("From"); from != expectedFrom {
			t.Errorf("Expected From header %q, got %q", expectedFrom, from)
		}

		// Test case-insensitivity
		if from := req.GetHeader("from"); from != expectedFrom {
			t.Errorf("Expected From header %q, got %q when using lowercase key", expectedFrom, from)
		}

		expectedAuth := map[string]string{
			"username": "alice",
			"realm":    "registrar.example.com",
			"nonce":    "dcd98b7102dd2f0e8b11d0f600bfb0c093",
			"uri":      "sip:registrar.example.com",
			"response": "6629fae49393a05397450978507c4ef1",
		}
		if !reflect.DeepEqual(req.Authorization, expectedAuth) {
			t.Errorf("Authorization header parsed incorrectly.\nExpected: %v\nGot:      %v", expectedAuth, req.Authorization)
		}
	})

	t.Run("Valid REGISTER with whitespace in Auth header", func(t *testing.T) {
		rawReq := "REGISTER sip:registrar.example.com SIP/2.0\r\n" +
			"Authorization: Digest username = \"alice\", realm = \"registrar.example.com\", nonce = \"dcd98b7102dd2f0e8b11d0f600bfb0c093\", uri = \"sip:registrar.example.com\", response = \"6629fae49393a053974507c4ef1\"\r\n" +
			"Content-Length: 0\r\n\r\n"

		req, err := ParseSIPRequest(rawReq)
		if err != nil {
			t.Fatalf("ParseSIPRequest failed: %v", err)
		}

		expectedAuth := map[string]string{
			"username": "alice",
			"realm":    "registrar.example.com",
			"nonce":    "dcd98b7102dd2f0e8b11d0f600bfb0c093",
			"uri":      "sip:registrar.example.com",
			"response": "6629fae49393a053974507c4ef1",
		}
		if !reflect.DeepEqual(req.Authorization, expectedAuth) {
			t.Errorf("Authorization header parsed incorrectly.\nExpected: %v\nGot:      %v", expectedAuth, req.Authorization)
		}
	})

	t.Run("Malformed request line", func(t *testing.T) {
		rawReq := "REGISTER sip:registrar.example.com\r\n"
		_, err := ParseSIPRequest(rawReq)
		if err == nil {
			t.Error("Expected error for malformed request line, but got nil")
		}
	})

	t.Run("Empty request", func(t *testing.T) {
		rawReq := ""
		_, err := ParseSIPRequest(rawReq)
		if err == nil {
			t.Error("Expected error for empty request, but got nil")
		}
	})
}
