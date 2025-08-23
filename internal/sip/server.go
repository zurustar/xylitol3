package sip

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"sip-server/internal/storage"
	"sync"
	"time"
)

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

// Run starts the SIP server listening on a UDP port.
func (s *Server) Run(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("could not listen on UDP: %w", err)
	}
	defer pc.Close()
	log.Printf("SIP server listening on UDP %s", addr)

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

func (s *Server) handleMessage(rawMsg string) string {
	req, err := ParseSIPRequest(rawMsg)
	if err != nil {
		log.Printf("Error parsing SIP request: %v\n---\n%s\n---", err, rawMsg)
		// Do not respond to malformed messages to avoid amplification attacks.
		return ""
	}

	if req.Method == "REGISTER" {
		return s.handleRegister(req)
	}

	// For any other method, respond with 405 Method Not Allowed.
	headers := map[string]string{"Allow": "REGISTER"}
	return BuildResponse(405, "Method Not Allowed", req, headers)
}

func (s *Server) handleRegister(req *SIPRequest) string {
	log.Printf("Handling REGISTER for %s", req.GetHeader("To"))

	if len(req.Authorization) == 0 {
		return s.createUnauthorizedResponse(req)
	}

	auth := req.Authorization
	username, okU := auth["username"]
	nonce, okN := auth["nonce"]
	uri, okR := auth["uri"]
	response, okResp := auth["response"]

	if !okU || !okN || !okR || !okResp {
		return BuildResponse(400, "Bad Request", req, map[string]string{"Warning": "Missing digest parameters"})
	}

	if !s.validateNonce(nonce) {
		log.Printf("Invalid or expired nonce for user %s", username)
		return s.createUnauthorizedResponse(req) // Respond with a fresh nonce
	}

	user, err := s.storage.GetUserByUsername(username)
	if err != nil || user == nil {
		log.Printf("Auth failed for user '%s': not found or db error", username)
		// Do not reveal if user exists. Treat as unauthorized.
		return s.createUnauthorizedResponse(req)
	}

	// user.Password is the pre-calculated HA1 hash
	expectedResponse := CalculateResponse(user.Password, req.Method, uri, nonce)

	if response != expectedResponse {
		log.Printf("Auth failed for user '%s': incorrect password", username)
		return s.createUnauthorizedResponse(req)
	}

	log.Printf("User '%s' authenticated successfully", username)
	s.updateRegistration(req)

	return s.createOKResponse(req)
}

func (s *Server) updateRegistration(req *SIPRequest) {
	s.regMutex.Lock()
	defer s.regMutex.Unlock()

	username := req.Authorization["username"]
	contact := req.GetHeader("Contact")
	expires := req.Expires()

	if expires > 0 {
		s.registrations[username] = Registration{
			ContactURI: contact,
			ExpiresAt:  time.Now().Add(time.Duration(expires) * time.Second),
		}
		log.Printf("Registered user '%s' at %s, expires in %d seconds", username, contact, expires)
	} else {
		delete(s.registrations, username)
		log.Printf("Unregistered user '%s'", username)
	}
}

// --- Response Creation ---

func (s *Server) createOKResponse(req *SIPRequest) string {
	headers := map[string]string{
		"Contact": fmt.Sprintf("%s;expires=%d", req.GetHeader("Contact"), req.Expires()),
	}
	return BuildResponse(200, "OK", req, headers)
}

func (s *Server) createUnauthorizedResponse(req *SIPRequest) string {
	nonce, _ := s.generateNonce()
	authValue := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5, qop="auth"`, s.realm, nonce)
	headers := map[string]string{
		"WWW-Authenticate": authValue,
	}
	return BuildResponse(401, "Unauthorized", req, headers)
}

// --- Nonce Management ---

func (s *Server) generateNonce() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(bytes)
	s.nonceMutex.Lock()
	defer s.nonceMutex.Unlock()
	s.nonces[nonce] = time.Now().Add(5 * time.Minute) // Nonces are valid for 5 minutes
	return nonce, nil
}

func (s *Server) validateNonce(nonce string) bool {
	s.nonceMutex.Lock()
	defer s.nonceMutex.Unlock()

	expiration, ok := s.nonces[nonce]
	if !ok {
		return false // Nonce not found
	}

	if time.Now().After(expiration) {
		delete(s.nonces, nonce) // Clean up expired nonce
		return false
	}

	// Nonce is valid, remove it to prevent reuse (replay attacks)
	delete(s.nonces, nonce)
	return true
}
