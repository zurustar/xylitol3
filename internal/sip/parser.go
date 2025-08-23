package sip

import (
	"fmt"
	"strconv"
	"strings"
)

// SIPRequest represents a parsed SIP request.
type SIPRequest struct {
	Method        string
	URI           string
	Proto         string
	Headers       map[string]string
	Authorization map[string]string
	Body          []byte
}

// GetHeader returns the value of a header, case-insensitively.
func (r *SIPRequest) GetHeader(name string) string {
	return r.Headers[strings.Title(name)]
}

// ParseSIPRequest parses a raw SIP request from a string.
func ParseSIPRequest(raw string) (*SIPRequest, error) {
	lines := strings.Split(raw, "\r\n")
	if len(lines) < 1 {
		return nil, fmt.Errorf("empty request")
	}

	reqLine := strings.Split(lines[0], " ")
	if len(reqLine) != 3 {
		return nil, fmt.Errorf("invalid request line: %s", lines[0])
	}

	req := &SIPRequest{
		Method:  reqLine[0],
		URI:     reqLine[1],
		Proto:   reqLine[2],
		Headers: make(map[string]string),
	}

	for _, line := range lines[1:] {
		if line == "" {
			continue // End of headers or empty line
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue // Malformed header
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		// Canonicalize header names for consistent access
		headerKey := strings.Title(key)
		req.Headers[headerKey] = value

		if headerKey == "Authorization" {
			req.Authorization = parseAuthHeader(value)
		}
	}

	if req.Method == "" {
		return nil, fmt.Errorf("missing method")
	}

	return req, nil
}

// parseAuthHeader parses the Digest Authorization header value.
func parseAuthHeader(headerValue string) map[string]string {
	result := make(map[string]string)
	if !strings.HasPrefix(headerValue, "Digest ") {
		return result
	}
	headerValue = strings.TrimPrefix(headerValue, "Digest ")

	parts := strings.Split(headerValue, ",")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			key := strings.TrimSpace(kv[0])
			value := strings.Trim(strings.TrimSpace(kv[1]), `"`)
			result[key] = value
		}
	}
	return result
}

// Expires returns the expiration time from the Contact or Expires header.
func (r *SIPRequest) Expires() int {
	expires := 3600 // Default expires
	contactHeader := r.GetHeader("Contact")
	if strings.Contains(contactHeader, "expires=") {
		parts := strings.Split(contactHeader, ";")
		for _, p := range parts {
			if strings.HasPrefix(p, "expires=") {
				expStr := strings.TrimPrefix(p, "expires=")
				if exp, err := strconv.Atoi(expStr); err == nil {
					return exp
				}
			}
		}
	}
	if expStr := r.GetHeader("Expires"); expStr != "" {
		if exp, err := strconv.Atoi(expStr); err == nil {
			expires = exp
		}
	}
	return expires
}
