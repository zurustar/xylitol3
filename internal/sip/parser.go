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

// String returns the string representation of the SIP request.
func (r *SIPRequest) String() string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("%s %s %s\r\n", r.Method, r.URI, r.Proto))
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

// Clone creates a deep copy of the SIPRequest.
func (r *SIPRequest) Clone() *SIPRequest {
	if r == nil {
		return nil
	}
	clone := &SIPRequest{
		Method:        r.Method,
		URI:           r.URI,
		Proto:         r.Proto,
		Headers:       make(map[string]string, len(r.Headers)),
		Authorization: make(map[string]string, len(r.Authorization)),
		Body:          make([]byte, len(r.Body)),
	}
	for k, v := range r.Headers {
		clone.Headers[k] = v
	}
	for k, v := range r.Authorization {
		clone.Authorization[k] = v
	}
	copy(clone.Body, r.Body)
	return clone
}

// SIPURI represents a parsed SIP URI.
type SIPURI struct {
	Scheme string // e.g., "sip"
	User   string
	Host   string
	Port   string
	Params map[string]string
}

// String reassembles the URI into a string.
func (u *SIPURI) String() string {
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString(":")
	if u.User != "" {
		b.WriteString(u.User)
		b.WriteString("@")
	}
	b.WriteString(u.Host)
	if u.Port != "" {
		b.WriteString(":")
		b.WriteString(u.Port)
	}
	// Note: This does not preserve the original order of parameters.
	for k, v := range u.Params {
		b.WriteString(";")
		b.WriteString(k)
		if v != "" {
			b.WriteString("=")
			b.WriteString(v)
		}
	}
	return b.String()
}

// ParseSIPURI parses a SIP URI string into a SIPURI struct.
// It handles URIs in the format: <sip:user@host:port;params> or sip:user@host
func ParseSIPURI(uri string) (*SIPURI, error) {
	uri = strings.Trim(uri, "<>") // Remove angle brackets

	schemeParts := strings.SplitN(uri, ":", 2)
	if len(schemeParts) != 2 {
		return nil, fmt.Errorf("invalid URI format (missing scheme): %s", uri)
	}

	s := &SIPURI{
		Scheme: schemeParts[0],
		Params: make(map[string]string),
	}
	remaining := schemeParts[1]

	// Separate params from the main part
	mainAndParams := strings.Split(remaining, ";")
	mainPart := mainAndParams[0]

	// Parse params
	if len(mainAndParams) > 1 {
		for _, param := range mainAndParams[1:] {
			kv := strings.SplitN(param, "=", 2)
			key := strings.TrimSpace(kv[0])
			var val string
			if len(kv) == 2 {
				val = strings.TrimSpace(kv[1])
			}
			s.Params[strings.ToLower(key)] = val
		}
	}

	// Parse user@host:port from the main part
	userHostPort := strings.SplitN(mainPart, "@", 2)
	var hostPortPart string
	if len(userHostPort) == 2 {
		s.User = userHostPort[0]
		hostPortPart = userHostPort[1]
	} else {
		hostPortPart = userHostPort[0]
	}

	hostPort := strings.SplitN(hostPortPart, ":", 2)
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

// Via represents a single Via header value.
type Via struct {
	Proto  string // e.g., "SIP/2.0/UDP"
	Host   string
	Port   string
	Params map[string]string
}

// GetParam returns a parameter from the Via header, case-insensitively.
func (v *Via) GetParam(name string) (string, bool) {
	val, ok := v.Params[strings.ToLower(name)]
	return val, ok
}

// Branch returns the branch parameter from the Via header.
func (v *Via) Branch() string {
	val, _ := v.GetParam("branch")
	return val
}

// ParseVia parses a single Via header field value string.
func ParseVia(viaValue string) (*Via, error) {
	parts := strings.Split(viaValue, ";")
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty via value")
	}

	// First part is proto and sent-by
	sentByParts := strings.Fields(parts[0])
	if len(sentByParts) != 2 {
		return nil, fmt.Errorf("malformed sent-by in Via: %s", parts[0])
	}

	via := &Via{
		Proto:  sentByParts[0],
		Params: make(map[string]string),
	}

	hostPort := strings.SplitN(sentByParts[1], ":", 2)
	via.Host = hostPort[0]
	if len(hostPort) == 2 {
		via.Port = hostPort[1]
	}

	// Subsequent parts are params
	for _, param := range parts[1:] {
		kv := strings.SplitN(param, "=", 2)
		key := strings.TrimSpace(kv[0])
		var val string
		if len(kv) == 2 {
			val = strings.TrimSpace(kv[1])
		}
		via.Params[strings.ToLower(key)] = val
	}

	return via, nil
}

// TopVia parses and returns the top-most Via header.
func (r *SIPRequest) TopVia() (*Via, error) {
	viaHeader := r.GetHeader("Via")
	if viaHeader == "" {
		return nil, fmt.Errorf("no Via header found")
	}

	// A request might have multiple Via headers, represented as a comma-separated list
	// in a single header line. The top-most one is the first one.
	topViaValue := viaHeader
	if idx := strings.Index(viaHeader, ","); idx != -1 {
		// This is a naive split. A proper parser would handle quoted commas.
		topViaValue = strings.TrimSpace(viaHeader[:idx])
	}

	return ParseVia(topViaValue)
}

// AllVias parses and returns all Via headers from the request.
// A request can have multiple Via headers, either on separate lines (which
// get concatenated into a comma-separated list by the message parser) or
// as a comma-separated list in a single header line.
func (r *SIPRequest) AllVias() ([]*Via, error) {
	viaHeader := r.GetHeader("Via")
	if viaHeader == "" {
		return nil, nil // No via headers is not an error
	}

	// This is a naive split. A proper parser would handle quoted commas.
	// For this proxy's purpose, it's sufficient.
	viaValues := strings.Split(viaHeader, ",")
	vias := make([]*Via, 0, len(viaValues))

	for _, viaValue := range viaValues {
		viaValue = strings.TrimSpace(viaValue)
		if viaValue == "" {
			continue
		}
		via, err := ParseVia(viaValue)
		if err != nil {
			// Return a partial list and the error
			return vias, fmt.Errorf("failed to parse via entry '%s': %w", viaValue, err)
		}
		vias = append(vias, via)
	}

	return vias, nil
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

// SessionExpires represents the parsed value of a Session-Expires header.
type SessionExpires struct {
	Delta     int
	Refresher string // "uac" or "uas"
}

// ParseSessionExpires parses the value of a Session-Expires header.
func ParseSessionExpires(headerValue string) (*SessionExpires, error) {
	se := &SessionExpires{}
	parts := strings.Split(headerValue, ";")

	delta, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return nil, fmt.Errorf("could not parse delta-seconds: %w", err)
	}
	se.Delta = delta

	for _, part := range parts[1:] {
		paramParts := strings.SplitN(part, "=", 2)
		if len(paramParts) == 2 {
			key := strings.ToLower(strings.TrimSpace(paramParts[0]))
			val := strings.ToLower(strings.TrimSpace(paramParts[1]))
			if key == "refresher" {
				se.Refresher = val
			}
		}
	}
	return se, nil
}

// SessionExpires parses the Session-Expires header from the request.
func (r *SIPRequest) SessionExpires() (*SessionExpires, error) {
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

// MinSE parses the Min-SE header from the request. Returns -1 if not found.
func (r *SIPRequest) MinSE() (int, error) {
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
