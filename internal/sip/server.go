package sip

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"sip-server/internal/storage"
	"strconv"
	"strings"
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
	listenAddr    string // The address this server is listening on, e.g., "1.2.3.4:5060"
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

	s.listenAddr = pc.LocalAddr().String()
	log.Printf("SIP server listening on UDP %s", s.listenAddr)

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
		go func(msg string, from net.Addr) {
			response, destAddr, err := s.handleMessage(msg, from)
			if err != nil {
				log.Printf("Error handling message: %v", err)
				return
			}
			if response != "" && destAddr != nil {
				if _, err := pc.WriteTo([]byte(response), destAddr); err != nil {
					log.Printf("Error sending response to %s: %v", destAddr, err)
				}
			}
		}(message, clientAddr)
	}
}

// handleMessage is the main router for incoming SIP messages.
func (s *Server) handleMessage(rawMsg string, clientAddr net.Addr) (string, net.Addr, error) {
	req, err := ParseSIPRequest(rawMsg)
	if err != nil {
		return "", nil, fmt.Errorf("error parsing SIP request: %v", err)
	}

	if req.Method == "REGISTER" {
		response := s.handleRegister(req)
		return response, clientAddr, nil
	}

	return s.handleProxy(req, clientAddr)
}

// handleProxy forwards a SIP request to a registered user.
func (s *Server) handleProxy(req *SIPRequest, clientAddr net.Addr) (string, net.Addr, error) {
	toURI, err := req.GetSIPURIHeader("To")
	if err != nil {
		resp := BuildResponse(400, "Bad Request", req, map[string]string{"Warning": "Malformed To header"})
		return resp, clientAddr, err
	}

	if toURI.Host != s.realm {
		log.Printf("Proxy received request for unknown realm: %s", toURI.Host)
		resp := BuildResponse(404, "Not Found", req, nil)
		return resp, clientAddr, nil
	}

	s.regMutex.RLock()
	registration, ok := s.registrations[toURI.User]
	s.regMutex.RUnlock()

	if !ok {
		log.Printf("Proxy lookup failed for user '%s': not registered", toURI.User)
		response := BuildResponse(480, "Temporarily Unavailable", req, nil)
		return response, clientAddr, nil
	}

	contactURI, err := ParseSIPURI(registration.ContactURI)
	if err != nil {
		log.Printf("Error parsing stored Contact URI '%s' for user '%s': %v", registration.ContactURI, toURI.User, err)
		return BuildResponse(500, "Server Internal Error", req, nil), clientAddr, err
	}

	maxForwards := 70
	if mfStr := req.GetHeader("Max-Forwards"); mfStr != "" {
		if mf, err := strconv.Atoi(mfStr); err == nil {
			maxForwards = mf
		}
	}
	if maxForwards <= 1 {
		return BuildResponse(483, "Too Many Hops", req, nil), clientAddr, nil
	}
	req.Headers["Max-Forwards"] = strconv.Itoa(maxForwards - 1)

	branch := "z9hG4bK-" + GenerateNonce(8)
	via := fmt.Sprintf("SIP/2.0/UDP %s;branch=%s", s.listenAddr, branch)
	if existingVia := req.GetHeader("Via"); existingVia != "" {
		req.Headers["Via"] = via + "," + existingVia
	} else {
		req.Headers["Via"] = via
	}

	recordRoute := fmt.Sprintf("<sip:%s;lr>", s.listenAddr)
	if existing, ok := req.Headers["Record-Route"]; ok {
		req.Headers["Record-Route"] = recordRoute + ", " + existing
	} else {
		req.Headers["Record-Route"] = recordRoute
	}

	forwardedMsg := RebuildRequest(req)

	destPort := "5060"
	if contactURI.Port != "" {
		destPort = contactURI.Port
	}

	destAddr, err := net.ResolveUDPAddr("udp", contactURI.Host+":"+destPort)
	if err != nil {
		log.Printf("Could not resolve Contact URI '%s' for user '%s': %v", registration.ContactURI, toURI.User, err)
		return BuildResponse(500, "Server Internal Error", req, nil), clientAddr, err
	}

	log.Printf("Proxying %s for %s to %s", req.Method, toURI.User, destAddr.String())
	return forwardedMsg, destAddr, nil
}

func RebuildRequest(req *SIPRequest) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("%s %s %s\r\n", req.Method, req.URI, req.Proto))
	for key, value := range req.Headers {
		if key == "Via" {
			vias := strings.Split(value, ",")
			for _, v := range vias {
				builder.WriteString(fmt.Sprintf("Via: %s\r\n", strings.TrimSpace(v)))
			}
		} else {
			builder.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
		}
	}
	builder.WriteString("Content-Length: 0\r\n")
	builder.WriteString("\r\n")
	return builder.String()
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
		return s.createUnauthorizedResponse(req)
	}

	user, err := s.storage.GetUserByUsername(username)
	if err != nil || user == nil {
		log.Printf("Auth failed for user '%s': not found or db error", username)
		return s.createUnauthorizedResponse(req)
	}

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
	contactHeader := req.GetHeader("Contact")

	start := strings.Index(contactHeader, "<")
	end := strings.Index(contactHeader, ">")
	var contactURI string
	if start != -1 && end != -1 {
		contactURI = contactHeader[start+1 : end]
	} else {
		contactURI = contactHeader
	}

	expires := req.Expires()
	if expires > 0 {
		s.registrations[username] = Registration{
			ContactURI: contactURI,
			ExpiresAt:  time.Now().Add(time.Duration(expires) * time.Second),
		}
		log.Printf("Registered user '%s' at %s, expires in %d seconds", username, contactURI, expires)
	} else {
		delete(s.registrations, username)
		log.Printf("Unregistered user '%s'", username)
	}
}

func (s *Server) createOKResponse(req *SIPRequest) string {
	headers := map[string]string{
		"Contact": fmt.Sprintf("%s;expires=%d", req.GetHeader("Contact"), req.Expires()),
		"Date":    time.Now().UTC().Format(time.RFC1123),
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

func GenerateNonce(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) generateNonce() (string, error) {
	nonce := GenerateNonce(8)
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

	delete(s.nonces, nonce)
	return true
}
