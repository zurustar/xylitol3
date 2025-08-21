package sip

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"sip-server/internal/storage"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Simplified SIP message representation
type sipMessage struct {
	Method        string
	RequestURI    string
	From          string
	To            string
	CallID        string
	CSeq          string
	Contact       string
	Authorization map[string]string
	Expires       int
	Via           string
	ContentLength int
}

// Registration holds information about a user's registered location.
type Registration struct {
	ContactURI string
	ExpiresAt  time.Time
}

// Server holds the dependencies for the SIP server.
type Server struct {
	storage       *storage.Storage
	registrations map[string]Registration
	regMutex      sync.RWMutex
	nonces        map[string]time.Time
	nonceMutex    sync.Mutex
	realm         string
}

// NewServer creates a new SIP server instance.
func NewServer(s *storage.Storage, realm string) *Server {
	return &Server{
		storage:       s,
		registrations: make(map[string]Registration),
		nonces:        make(map[string]time.Time),
		realm:         realm,
	}
}

// Run starts the SIP server, listening on both TCP and UDP.
func (s *Server) Run(ctx context.Context, addr string) error {
	g, gCtx := errgroup.WithContext(ctx)

	// Start UDP listener
	g.Go(func() error {
		return s.runUDPListener(gCtx, addr)
	})

	// Start TCP listener
	g.Go(func() error {
		return s.runTCPListener(gCtx, addr)
	})

	log.Printf("SIP server started on %s (TCP/UDP)", addr)
	return g.Wait()
}

func (s *Server) runUDPListener(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("could not listen on UDP: %w", err)
	}
	defer pc.Close()
	log.Printf("Listening on UDP %s", addr)

	go func() {
		<-ctx.Done()
		pc.Close()
	}()

	buf := make([]byte, 4096)
	for {
		n, clientAddr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil // Graceful shutdown
			}
			log.Printf("Error reading from UDP: %v", err)
			continue
		}
		message := string(buf[:n])
		go func() {
			response := s.handleMessage(message)
			if response != "" {
				pc.WriteTo([]byte(response), clientAddr)
			}
		}()
	}
}

func (s *Server) runTCPListener(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("could not listen on TCP: %w", err)
	}
	defer l.Close()
	log.Printf("Listening on TCP %s", addr)

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // Graceful shutdown
			}
			log.Printf("Error accepting TCP connection: %v", err)
			continue
		}
		go s.handleTCPConnection(ctx, conn)
	}
}

func (s *Server) handleTCPConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		// Set a read deadline
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))

		// Naive parsing: read until we see a double CRLF
		var sb strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					log.Printf("Error reading from TCP stream: %v", err)
				}
				return // Close connection on error or EOF
			}
			sb.WriteString(line)
			if line == "\r\n" {
				break // End of headers
			}
		}

		message := sb.String()
		if message != "" {
			response := s.handleMessage(message)
			if response != "" {
				conn.Write([]byte(response))
			}
		}
	}
}

func (s *Server) handleMessage(rawMsg string) string {
	req, err := parseSipRequest(rawMsg)
	if err != nil {
		log.Printf("Error parsing SIP request: %v\n---\n%s\n---", err, rawMsg)
		return "" // Do not respond to malformed messages
	}

	if req.Method == "REGISTER" {
		return s.handleRegister(req)
	}

	log.Printf("Received unhandled method '%s'", req.Method)
	// In a real proxy, you would send a 405 Method Not Allowed
	return ""
}

func (s *Server) handleRegister(req *sipMessage) string {
	log.Printf("Handling REGISTER for %s", req.To)

	if len(req.Authorization) == 0 {
		return s.createUnauthorizedResponse(req)
	}

	auth := req.Authorization
	username, _ := auth["username"]
	nonce, _ := auth["nonce"]
	uri, _ := auth["uri"]
	response, _ := auth["response"]

	if !s.validateNonce(nonce) {
		log.Printf("Invalid nonce for user %s", username)
		return s.createUnauthorizedResponse(req)
	}

	user, err := s.storage.GetUserByUsername(username)
	if err != nil || user == nil {
		log.Printf("Auth failed for user %s: not found or db error", username)
		return s.createUnauthorizedResponse(req)
	}

	ha1 := user.Password
	expectedResponse := calculateResponseWithHA1(ha1, req.Method, uri, nonce)

	if response != expectedResponse {
		log.Printf("Auth failed for user %s: incorrect password", username)
		return s.createUnauthorizedResponse(req)
	}

	log.Printf("User '%s' authenticated successfully", username)

	s.regMutex.Lock()
	defer s.regMutex.Unlock()
	if req.Expires > 0 {
		s.registrations[username] = Registration{
			ContactURI: req.Contact,
			ExpiresAt:  time.Now().Add(time.Duration(req.Expires) * time.Second),
		}
		log.Printf("Registered user '%s' at %s, expires in %d", username, req.Contact, req.Expires)
	} else {
		delete(s.registrations, username)
		log.Printf("Unregistered user '%s'", username)
	}

	return s.createOKResponse(req)
}

// --- Response Creation ---

func (s *Server) createOKResponse(req *sipMessage) string {
	return fmt.Sprintf("SIP/2.0 200 OK\r\n"+
		"Via: %s\r\n"+
		"From: %s\r\n"+
		"To: %s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %s\r\n"+
		"Contact: %s;expires=%d\r\n"+
		"Content-Length: 0\r\n\r\n",
		req.Via, req.From, req.To, req.CallID, req.CSeq, req.Contact, req.Expires)
}

func (s *Server) createUnauthorizedResponse(req *sipMessage) string {
	nonce, _ := s.generateNonce()
	authValue := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5`, s.realm, nonce)
	return fmt.Sprintf("SIP/2.0 401 Unauthorized\r\n"+
		"Via: %s\r\n"+
		"From: %s\r\n"+
		"To: %s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %s\r\n"+
		"WWW-Authenticate: %s\r\n"+
		"Content-Length: 0\r\n\r\n",
		req.Via, req.From, req.To, req.CallID, req.CSeq, authValue)
}

// --- Parsing Logic ---

func parseSipRequest(raw string) (*sipMessage, error) {
	lines := strings.Split(raw, "\r\n")
	if len(lines) < 1 {
		return nil, fmt.Errorf("empty request")
	}

	reqLine := strings.Split(lines[0], " ")
	if len(reqLine) != 3 {
		return nil, fmt.Errorf("invalid request line: %s", lines[0])
	}

	msg := &sipMessage{
		Method:     reqLine[0],
		RequestURI: reqLine[1],
		Expires:    3600, // Default expires
	}

	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch strings.ToLower(key) {
		case "via":
			msg.Via = value
		case "from":
			msg.From = value
		case "to":
			msg.To = value
		case "call-id":
			msg.CallID = value
		case "cseq":
			msg.CSeq = value
		case "contact":
			if strings.Contains(value, "expires=") {
				parts := strings.Split(value, ";")
				msg.Contact = parts[0]
				for _, p := range parts {
					if strings.HasPrefix(p, "expires=") {
						expStr := strings.TrimPrefix(p, "expires=")
						msg.Expires, _ = strconv.Atoi(expStr)
					}
				}
			} else {
				msg.Contact = value
			}
		case "expires":
			msg.Expires, _ = strconv.Atoi(value)
		case "authorization":
			msg.Authorization = parseAuthHeader(value)
		case "content-length":
			msg.ContentLength, _ = strconv.Atoi(value)
		}
	}

	if msg.Method == "" {
		return nil, fmt.Errorf("missing method")
	}
	// A real parser would read Content-Length bytes for the body here
	return msg, nil
}

// --- Nonce and Digest Logic (reused) ---

func (s *Server) generateNonce() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(bytes)
	s.nonceMutex.Lock()
	defer s.nonceMutex.Unlock()
	s.nonces[nonce] = time.Now().Add(5 * time.Minute)
	return nonce, nil
}

func (s *Server) validateNonce(nonce string) bool {
	s.nonceMutex.Lock()
	defer s.nonceMutex.Unlock()
	expiration, ok := s.nonces[nonce]
	if !ok {
		return false
	}
	if time.Now().After(expiration) {
		delete(s.nonces, nonce)
		return false
	}
	return true
}

func md5hash(data string) string {
	sum := md5.Sum([]byte(data))
	return hex.EncodeToString(sum[:])
}

func calculateResponseWithHA1(ha1, method, digestURI, nonce string) string {
	ha2 := md5hash(fmt.Sprintf("%s:%s", method, digestURI))
	return md5hash(fmt.Sprintf("%s:%s:%s", ha1, nonce, ha2))
}

func parseAuthHeader(headerValue string) map[string]string {
	result := make(map[string]string)
	if strings.HasPrefix(headerValue, "Digest ") {
		headerValue = strings.TrimPrefix(headerValue, "Digest ")
	}
	parts := strings.Split(headerValue, ",")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			key := kv[0]
			value := strings.Trim(kv[1], `"`)
			result[key] = value
		}
	}
	return result
}
