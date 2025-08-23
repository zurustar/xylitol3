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

// SIPURI represents a parsed SIP URI.
type SIPURI struct {
	Scheme string // e.g., "sip"
	User   string
	Host   string
	Port   string
}

// ParseSIPURI parses a SIP URI string into a SIPURI struct.
// It handles URIs in the format: <sip:user@host:port> or sip:user@host
func ParseSIPURI(uri string) (*SIPURI, error) {
	uri = strings.Trim(uri, "<>") // Remove angle brackets

	parts := strings.SplitN(uri, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid URI format: %s", uri)
	}

	s := &SIPURI{Scheme: parts[0]}
	remaining := parts[1]

	userHostPort := strings.SplitN(remaining, "@", 2)
	if len(userHostPort) == 2 {
		s.User = userHostPort[0]
		remaining = userHostPort[1]
	}

	hostPort := strings.SplitN(remaining, ":", 2)
	s.Host = hostPort[0]
	if len(hostPort) == 2 {
		s.Port = hostPort[1]
	}

	if s.Host == "" {
		return nil, fmt.Errorf("host is missing in URI: %s", uri)
	}

	return s, nil
}

// GetHeader returns the value of a header, case-insensitively.
func (r *SIPRequest) GetHeader(name string) string {
	return r.Headers[strings.Title(name)]
}

// GetSIPURIHeader parses a header that is expected to contain a SIP URI.
func (r *SIPRequest) GetSIPURIHeader(name string) (*SIPURI, error) {
	headerValue := r.GetHeader(name)
	if headerValue == "" {
		return nil, fmt.Errorf("header '%s' not found", name)
	}
	// The value might be just the URI, or it might have a display name like "Bob" <sip:bob@host>
	// We'll look for the angle brackets first.
	start := strings.Index(headerValue, "<")
	end := strings.Index(headerValue, ">")
	if start != -1 && end != -1 && end > start {
		headerValue = headerValue[start : end+1]
	}

	return ParseSIPURI(headerValue)
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
