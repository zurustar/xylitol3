package sip

import (
	"fmt"
	"strconv"
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
		// Ignore Content-Length from the map, we'll add it based on the Body.
		if strings.Title(key) != "Content-Length" {
			builder.WriteString(fmt.Sprintf("%s: %s\r\n", strings.Title(key), value))
		}
	}
	builder.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(r.Body)))
	builder.WriteString("\r\n")
	builder.Write(r.Body)
	return builder.String()
}

// GetHeader returns the value of a header. It expects the name to be in canonical form.
func (r *SIPResponse) GetHeader(name string) string {
	return r.Headers[strings.Title(name)]
}

// SessionExpires parses the Session-Expires header from the response.
func (r *SIPResponse) SessionExpires() (*SessionExpires, error) {
	seHeader := r.GetHeader("Session-Expires")
	if seHeader == "" {
		// Also check compact form 'x'
		seHeader = r.GetHeader("x")
	}
	if seHeader == "" {
		return nil, nil // Not an error, header is just not present
	}
	return ParseSessionExpires(seHeader)
}

// MinSE parses the Min-SE header from the response. Returns -1 if not found.
func (r *SIPResponse) MinSE() (int, error) {
	minSEHeader := r.GetHeader("Min-SE")
	if minSEHeader == "" {
		return -1, nil // Not an error, header is not present
	}
	// Min-SE only has a delta-seconds value
	minSE, err := strconv.Atoi(strings.TrimSpace(minSEHeader))
	if err != nil {
		return -1, fmt.Errorf("could not parse Min-SE value: %w", err)
	}
	return minSE, nil
}

// TopVia parses and returns the top-most Via header from the response.
func (r *SIPResponse) TopVia() (*Via, error) {
	viaHeader := r.GetHeader("Via")
	if viaHeader == "" {
		return nil, fmt.Errorf("no Via header found in response")
	}

	topViaValue := viaHeader
	if idx := strings.Index(viaHeader, ","); idx != -1 {
		topViaValue = strings.TrimSpace(viaHeader[:idx])
	}

	return ParseVia(topViaValue)
}

// PopVia removes the top-most Via header from the response.
func (r *SIPResponse) PopVia() {
	viaHeader := r.GetHeader("Via")
	if viaHeader == "" {
		return
	}

	if idx := strings.Index(viaHeader, ","); idx != -1 {
		// There are more Via headers, so we set the header to the rest of the list.
		r.Headers["Via"] = strings.TrimSpace(viaHeader[idx+1:])
	} else {
		// This was the only Via header, so we remove the header entirely.
		delete(r.Headers, "Via")
	}
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
		resp.Headers[strings.Title(key)] = val
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

// ParseSIPResponse parses a raw string into a SIPResponse struct.
func ParseSIPResponse(raw string) (*SIPResponse, error) {
	lines := strings.Split(raw, "\r\n")
	if len(lines) < 1 {
		return nil, fmt.Errorf("empty response")
	}

	respLine := strings.SplitN(lines[0], " ", 3)
	if len(respLine) < 2 { // Reason phrase can be empty
		return nil, fmt.Errorf("invalid response line: %s", lines[0])
	}

	statusCode, err := strconv.Atoi(respLine[1])
	if err != nil {
		return nil, fmt.Errorf("invalid status code '%s' in response line", respLine[1])
	}

	var reason string
	if len(respLine) > 2 {
		reason = respLine[2]
	}

	res := &SIPResponse{
		Proto:      respLine[0],
		StatusCode: statusCode,
		Reason:     reason,
		Headers:    make(map[string]string),
	}

	// Find the end of headers (blank line)
	headerEndIndex := len(lines)
	bodyIndex := -1
	for i, line := range lines[1:] {
		if line == "" {
			headerEndIndex = i + 1
			bodyIndex = strings.Index(raw, "\r\n\r\n")
			break
		}
	}

	// Parse headers
	for _, line := range lines[1:headerEndIndex] {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue // Malformed header
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		res.Headers[strings.Title(key)] = value
	}

	// Get body if Content-Length indicates it
	if contentLengthStr := res.GetHeader("Content-Length"); contentLengthStr != "" {
		if contentLength, err := strconv.Atoi(contentLengthStr); err == nil && contentLength > 0 {
			if bodyIndex != -1 && len(raw) >= bodyIndex+4 {
				res.Body = []byte(raw[bodyIndex+4:])
			}
		}
	}

	return res, nil
}
