package sip

import (
	"fmt"
	"strings"
)

// BuildResponse constructs a SIP response string.
// It copies necessary headers from the original request and allows adding new ones.
func BuildResponse(statusCode int, statusText string, req *SIPRequest, extraHeaders map[string]string) string {
	var builder strings.Builder

	// Status-Line
	builder.WriteString(fmt.Sprintf("%s %d %s\r\n", req.Proto, statusCode, statusText))

	// Copy essential headers from the request.
	// These headers are canonicalized to Title-Case by the parser.
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
			builder.WriteString(fmt.Sprintf("%s: %s\r\n", h, val))
		}
	}

	// Add any extra headers provided by the caller (e.g., WWW-Authenticate, Contact, Allow).
	for key, val := range extraHeaders {
		builder.WriteString(fmt.Sprintf("%s: %s\r\n", key, val))
	}

	// Add Content-Length (assuming no body for now).
	builder.WriteString("Content-Length: 0\r\n")

	// Final CRLF to terminate the header section.
	builder.WriteString("\r\n")

	return builder.String()
}
