package sip

import (
	"fmt"
	"strings"
)

// SIPResponse represents a SIP response message.
type SIPResponse struct {
	Proto      string
	StatusCode int
	Reason     string
	Headers    map[string]string
	Body       []byte
}

// String returns the string representation of the SIP response.
func (r *SIPResponse) String() string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("%s %d %s\r\n", r.Proto, r.StatusCode, r.Reason))
	for key, value := range r.Headers {
		// Canonicalize header names for consistency
		builder.WriteString(fmt.Sprintf("%s: %s\r\n", strings.Title(key), value))
	}
	builder.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(r.Body)))
	builder.WriteString("\r\n")
	builder.Write(r.Body)
	return builder.String()
}

// BuildResponse constructs a SIP response object.
// It copies necessary headers from the original request and allows adding new ones.
func BuildResponse(statusCode int, statusText string, req *SIPRequest, extraHeaders map[string]string) *SIPResponse {
	resp := &SIPResponse{
		Proto:      req.Proto,
		StatusCode: statusCode,
		Reason:     statusText,
		Headers:    make(map[string]string),
		Body:       []byte{},
	}

	// Copy essential headers from the request.
	headersToCopy := []string{"Via", "From", "To", "Call-Id", "Cseq"}
	for _, h := range headersToCopy {
		if val := req.GetHeader(h); val != "" {
			// Add a tag to the 'To' header in the response, as required by RFC 3261,
			// if the request's 'To' header didn't already have one.
			if h == "To" && !strings.Contains(req.GetHeader("To"), "tag=") {
				// This is a simple, static tag for demonstration. A real server
				// would generate a unique tag for the dialog.
				val = fmt.Sprintf("%s;tag=z9hG4bK-response-tag", val)
			}
			resp.Headers[h] = val
		}
	}

	// Add any extra headers provided by the caller (e.g., WWW-Authenticate, Contact, Allow).
	for key, val := range extraHeaders {
		resp.Headers[key] = val
	}

	return resp
}

// BuildAck creates an ACK request for a given final response to an INVITE.
// Per RFC 3261, the ACK for a 2xx response is a separate transaction, but for
// a non-2xx response, it's part of the same transaction.
func BuildAck(res *SIPResponse, originalReq *SIPRequest) *SIPRequest {
	ack := &SIPRequest{
		Method: "ACK",
		URI:    originalReq.URI,
		Proto:  originalReq.Proto,
		Headers: map[string]string{
			// The Via header field in the ACK MUST be the same as the top Via
			// header field of the original request.
			"Via":          originalReq.GetHeader("Via"),
			// The To header field in the ACK MUST equal the To header field in the
			// response being acknowledged.
			"To":           res.Headers["To"],
			"From":         originalReq.GetHeader("From"),
			"Call-ID":      originalReq.GetHeader("Call-ID"),
			"CSeq":         strings.Split(originalReq.GetHeader("CSeq"), " ")[0] + " ACK",
			"Max-Forwards": "70",
		},
		Body: []byte{},
	}

	// Copy Route headers if they were in the original INVITE
	if route := originalReq.GetHeader("Route"); route != "" {
		ack.Headers["Route"] = route
	}

	return ack
}
