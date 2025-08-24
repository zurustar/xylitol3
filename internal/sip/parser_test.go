package sip

import (
	"reflect"
	"strconv"
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

	t.Run("Request with body", func(t *testing.T) {
		sdpBody := "v=0\r\n" +
			"o=user1 53655765 2353687637 IN IP4 192.0.2.1\r\n" +
			"s=-\r\n" +
			"c=IN IP4 192.0.2.1\r\n" +
			"t=0 0\r\n" +
			"m=audio 8000 RTP/AVP 0\r\n" +
			"a=rtpmap:0 PCMU/8000\r\n"
		rawReq := "INVITE sip:bob@biloxi.com SIP/2.0\r\n" +
			"Via: SIP/2.0/UDP client.atlanta.com;branch=z9hG4bK74bf9\r\n" +
			"From: Alice <sip:alice@atlanta.com>;tag=1928301774\r\n" +
			"To: Bob <sip:bob@biloxi.com>\r\n" +
			"Call-ID: a84b4c76e66710\r\n" +
			"CSeq: 314159 INVITE\r\n" +
			"Content-Type: application/sdp\r\n" +
			"Content-Length: " + strconv.Itoa(len(sdpBody)) + "\r\n" +
			"\r\n" +
			sdpBody

		req, err := ParseSIPRequest(rawReq)
		if err != nil {
			t.Fatalf("ParseSIPRequest failed: %v", err)
		}

		if len(req.Body) == 0 {
			t.Fatal("Request body is empty, but it should be populated.")
		}

		if string(req.Body) != sdpBody {
			t.Errorf("Request body does not match expected SDP.\nExpected:\n%s\nGot:\n%s", sdpBody, string(req.Body))
		}
	})
}

func TestParseViaHeader(t *testing.T) {
	t.Run("Single Via header", func(t *testing.T) {
		req := &SIPRequest{Headers: map[string]string{"Via": "SIP/2.0/UDP pc33.atlanta.com;branch=z9hG4bK776asdhds"}}
		vias, err := req.AllVias()
		if err != nil {
			t.Fatalf("AllVias() failed: %v", err)
		}
		if len(vias) != 1 {
			t.Fatalf("Expected 1 Via header, got %d", len(vias))
		}
		if vias[0].Host != "pc33.atlanta.com" {
			t.Errorf("Expected host pc33.atlanta.com, got %s", vias[0].Host)
		}
		if vias[0].Branch() != "z9hG4bK776asdhds" {
			t.Errorf("Expected branch z9hG4bK776asdhds, got %s", vias[0].Branch())
		}
	})

	t.Run("Multiple Via headers with quoted comma", func(t *testing.T) {
		// This tests the naive comma splitting. A proper parser should handle quoted commas.
		viaHeader := `SIP/2.0/UDP first.example.com;branch=z9hG4bK-abc, SIP/2.0/UDP second.example.com;branch=z9hG4bK-def;someparam="a,b,c"`
		req := &SIPRequest{Headers: map[string]string{"Via": viaHeader}}
		vias, err := req.AllVias()
		if err != nil {
			t.Fatalf("AllVias() failed: %v", err)
		}
		if len(vias) != 2 {
			t.Fatalf("Expected 2 Via headers, got %d", len(vias))
		}

		// Check first Via
		if vias[0].Host != "first.example.com" {
			t.Errorf("Expected host first.example.com, got %s", vias[0].Host)
		}
		if vias[0].Branch() != "z9hG4bK-abc" {
			t.Errorf("Expected branch z9hG4bK-abc, got %s", vias[0].Branch())
		}

		// Check second Via
		if vias[1].Host != "second.example.com" {
			t.Errorf("Expected host second.example.com, got %s", vias[1].Host)
		}
		if vias[1].Branch() != "z9hG4bK-def" {
			t.Errorf("Expected branch z9hG4bK-def, got %s", vias[1].Branch())
		}
		expectedParam := `"a,b,c"`
		if param, ok := vias[1].GetParam("someparam"); !ok || param != expectedParam {
			t.Errorf("Expected param 'someparam' to be %q, got %q", expectedParam, param)
		}
	})
}
