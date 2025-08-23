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
	txManager     *TransactionManager
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
		txManager:     NewTransactionManager(),
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
		go s.handleRequest(pc, message, clientAddr)
	}
}

// handleRequest is the new entry point for all incoming SIP requests.
// It manages transactions and dispatches requests to the appropriate handlers.
func (s *Server) handleRequest(pc net.PacketConn, rawMsg string, remoteAddr net.Addr) {
	req, err := ParseSIPRequest(rawMsg)
	if err != nil {
		log.Printf("Error parsing SIP request: %v", err)
		// Cannot send a response if parsing fails completely.
		return
	}

	topVia, err := req.TopVia()
	if err != nil {
		log.Printf("Could not get Via header: %v", err)
		// Maybe send a 400 Bad Request? For now, just log.
		return
	}
	branchID := topVia.Branch()

	// RFC 3261 compliant transaction ID must start with magic cookie.
	if !strings.HasPrefix(branchID, RFC3261BranchMagicCookie) {
		log.Printf("Received request with non-RFC3261 branch ID: %s. Handling statelessly.", branchID)
		s.handleStateless(pc, req, remoteAddr)
		return
	}

	// Look for an existing transaction.
	if tx, ok := s.txManager.Get(branchID); ok {
		log.Printf("Retransmission received for transaction %s", branchID)
		if srvTx, ok := tx.(ServerTransaction); ok {
			srvTx.Receive(req)
		} else if clientTx, ok := tx.(ClientTransaction); ok {
			// This case is for when a proxy receives a response.
			// The response needs to be parsed and passed to the client transaction.
			// This logic will be built out later.
			log.Printf("Received message for client transaction %s, but response handling not implemented yet.", clientTx.ID())
		}
		return
	}

	// No existing transaction, create a new one.
	log.Printf("New request received, creating transaction %s for method %s", branchID, req.Method)
	var tx ServerTransaction
	switch req.Method {
	case "REGISTER":
		tx, err = NewNonInviteServerTx(req, pc, remoteAddr)
	case "INVITE":
		tx, err = NewInviteServerTx(req, pc, remoteAddr)
	default:
		// For other methods like ACK, CANCEL, BYE, or proxying, handle statelessly for now.
		log.Printf("Handling method %s statelessly.", req.Method)
		s.handleStateless(pc, req, remoteAddr)
		return
	}

	if err != nil {
		log.Printf("Error creating transaction: %v", err)
    return
	}
	s.txManager.Add(tx)

	// Handle responses to our client transactions
	if req.Method == "" && strings.HasPrefix(rawMsg, "SIP/2.0") {
		// This is a response. We need to find the matching client transaction.
		// A real implementation would parse the response and find the branch ID.
		// For now, this part is missing as we are not yet creating client transactions
		// in a way that can receive responses.
		log.Printf("Received a response, but response handling for client transactions is not fully implemented.")
		return
	// This goroutine is the Transaction User (TU)
	go s.handleTransaction(tx)
}

func (s *Server) handleTransaction(tx ServerTransaction) {
	select {
	case tuReq := <-tx.Requests():
		var response *SIPResponse
		// Dispatch to the correct handler based on method
		switch tuReq.Method {
		case "REGISTER":
			response = s.handleRegister(tuReq)
		case "INVITE":
			// The current proxy logic is stateless. We'll need a stateful proxy handler.
			// For now, let's just respond with a 501 Not Implemented for INVITEs
			// that reach the server transaction layer, as the proxy logic isn't stateful yet.
			response = BuildResponse(501, "Not Implemented", tuReq, nil)
		default:
			response = BuildResponse(405, "Method Not Allowed", tuReq, nil)
		}

		if err := tx.Respond(response); err != nil {
			log.Printf("Error sending response to transaction %s: %v", tx.ID(), err)
		}
	case <-tx.Done():
		// Transaction terminated before we could process it.
	}
}

	topVia, err := req.TopVia()
	if err != nil {
		log.Printf("Could not get Via header: %v", err)
		return
	}
	branchID := topVia.Branch()

	if !strings.HasPrefix(branchID, RFC3261BranchMagicCookie) {
		log.Printf("Received request with non-RFC3261 branch ID: %s. Handling statelessly.", branchID)
		s.handleStateless(pc, req, remoteAddr)
		return
	}

	if tx, ok := s.txManager.Get(branchID); ok {
		log.Printf("Retransmission received for transaction %s", branchID)
		if srvTx, ok := tx.(ServerTransaction); ok {
			srvTx.Receive(req)
		}
		return
	}

	log.Printf("New request received, creating transaction %s for method %s", branchID, req.Method)
	var srvTx ServerTransaction
	switch req.Method {
	case "REGISTER":
		srvTx, err = NewNonInviteServerTx(req, pc, remoteAddr)
	case "INVITE":
		srvTx, err = NewInviteServerTx(req, pc, remoteAddr)
	case "ACK":
		// ACKs for non-2xx are handled by the INVITE server transaction's Receive method.
		// ACKs for 2xx are end-to-end and do not create a server transaction.
		// We can't easily find the transaction here without more context.
		// For now, we'll just log and drop it.
		log.Printf("Received ACK, but no matching transaction found. Dropping.")
		return
	default:
		log.Printf("Handling method %s statelessly.", req.Method)
		s.handleStateless(pc, req, remoteAddr)
		return
	}

	if err != nil {
		log.Printf("Error creating server transaction: %v", err)
		return

func (s *Server) handleStateless(pc net.PacketConn, req *SIPRequest, remoteAddr net.Addr) {
	// Statelessly forward proxyable requests, respond to others.
	if req.Method == "INVITE" || req.Method == "BYE" { // Add other proxy methods here
		fwdMsg, destAddr, err := s.handleProxy(req, remoteAddr)
		if err != nil {
			log.Printf("Error handling stateless proxy message: %v", err)
			// Send a 500 error back if proxying fails
			errResp := BuildResponse(500, "Proxying Error", req, nil)
			if _, writeErr := pc.WriteTo([]byte(errResp.String()), remoteAddr); writeErr != nil {
				log.Printf("Error sending error response: %v", writeErr)
			}
			return
		}
		if fwdMsg != "" && destAddr != nil {
			if _, err := pc.WriteTo([]byte(fwdMsg), destAddr); err != nil {
				log.Printf("Error sending forwarded message to %s: %v", destAddr, err)
			}
		}
	} else {
		// For non-proxyable methods, we'd need a handler. For now, 405.
		response := BuildResponse(405, "Method Not Allowed", req, nil)
		if _, err := pc.WriteTo([]byte(response.String()), remoteAddr); err != nil {
			log.Printf("Error sending 405 response: %v", err)
		}
	}
}

// handleProxy forwards a SIP request to a registered user.
// NOTE: This is currently stateless and will be refactored to use a client transaction.
func (s *Server) handleProxy(req *SIPRequest, clientAddr net.Addr) (string, net.Addr, error) {
	toURI, err := req.GetSIPURIHeader("To")
	if err != nil {
		return "", nil, fmt.Errorf("malformed To header")
	}
	s.txManager.Add(srvTx)
	go s.handleTransaction(srvTx, pc)
}

// handleTransaction is the Transaction User (TU) for server transactions.
func (s *Server) handleTransaction(srvTx ServerTransaction, pc net.PacketConn) {
	select {
	case req := <-srvTx.Requests():
		switch req.Method {
		case "REGISTER":
			res := s.handleRegister(req)
			if err := srvTx.Respond(res); err != nil {
				log.Printf("Error sending response to transaction %s: %v", srvTx.ID(), err)
			}
		case "INVITE":
			s.doProxy(req, srvTx, pc)
		default:
			res := BuildResponse(405, "Method Not Allowed", req, nil)
			if err := srvTx.Respond(res); err != nil {
				log.Printf("Error sending response to transaction %s: %v", srvTx.ID(), err)
			}
		}
	case <-srvTx.Done():
	}
}

// doProxy is the stateful proxy logic.
func (s *Server) doProxy(originalReq *SIPRequest, upstreamTx ServerTransaction, pc net.PacketConn) {
	// 1. Determine target(s) - for now, just one from the registrations map.
	toURI, err := originalReq.GetSIPURIHeader("To")
	if err != nil {
		upstreamTx.Respond(BuildResponse(400, "Bad Request", originalReq, map[string]string{"Warning": "Malformed To header"}))
		return
	if toURI.Host != s.realm {
		log.Printf("Proxy received request for unknown realm: %s", toURI.Host)
		return "", nil, fmt.Errorf("unknown realm")
	}

	s.regMutex.RLock()
	registration, ok := s.registrations[toURI.User]
	s.regMutex.RUnlock()

	if !ok {
		log.Printf("Proxy lookup failed for user '%s': not registered", toURI.User)
		upstreamTx.Respond(BuildResponse(480, "Temporarily Unavailable", originalReq, nil))
	}

	contactURI, err := ParseSIPURI(registration.ContactURI)
	if err != nil {
		log.Printf("Error parsing stored Contact URI for user '%s': %v", toURI.User, err)
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
		return
	}

	destPort := "5060"
	if contactURI.Port != "" {
		destPort = contactURI.Port
	}
	destAddr, err := net.ResolveUDPAddr("udp", contactURI.Host+":"+destPort)
	if err != nil {
		log.Printf("Could not resolve Contact URI '%s' for user '%s': %v", registration.ContactURI, toURI.User, err)
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
		return
	}

	// 2. Create a new request to forward.
	fwdReq := s.prepareForwardedRequest(originalReq)

	// 3. Create a client transaction to send the forwarded request.
	clientTx, err := NewInviteClientTx(fwdReq, pc, destAddr)
	if err != nil {
		log.Printf("Failed to create client transaction: %v", err)
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
		return
	}
	s.txManager.Add(clientTx)
	log.Printf("Proxying INVITE from %s to %s via client tx %s", originalReq.GetHeader("From"), originalReq.GetHeader("To"), clientTx.ID())

	// 4. Wait for responses on the client transaction and forward them
	//    back up through the server transaction.
	go func() {
		for {
			select {
			case res := <-clientTx.Responses():
				if res == nil { continue }
				log.Printf("Proxy received response for tx %s: %d %s", clientTx.ID(), res.StatusCode, res.Reason)

				// TODO: This is a simplified forwarding logic. A real proxy would need to manage
				// multiple branches (forking) and carefully craft the response.
				// For now, we just forward the first response we get.

				// We need to remove the Via header we added before forwarding.
				vias := strings.Split(res.Headers["Via"], ",")
				if len(vias) > 1 {
					res.Headers["Via"] = strings.Join(vias[1:], ",")
				} else {
					delete(res.Headers, "Via")
				}

				upstreamTx.Respond(res)

				// If it's a final response, our job for this branch is done.
				if res.StatusCode >= 200 {
					return
				}

			case <-clientTx.Done():
				log.Printf("Client transaction %s terminated.", clientTx.ID())
				// If the upstream is still running, we might need to send a timeout.
				// For now, we just exit.
				return
			case <-upstreamTx.Done():
				log.Printf("Upstream transaction %s terminated. Terminating downstream.", upstreamTx.ID())
				clientTx.Terminate()
				return
			}
		}
	}()
}

// prepareForwardedRequest creates a copy of a request suitable for forwarding.
func (s *Server) prepareForwardedRequest(req *SIPRequest) *SIPRequest {
	fwdReq := &SIPRequest{
		Method:  req.Method,
		URI:     req.URI,
		Proto:   req.Proto,
		Headers: make(map[string]string),
		Body:    req.Body,
	}
	for k, v := range req.Headers {
		fwdReq.Headers[k] = v
		return "", nil, fmt.Errorf("error parsing stored Contact URI for user '%s': %v", toURI.User, err)
	}

	maxForwards := 70
	if mfStr := req.GetHeader("Max-Forwards"); mfStr != "" {
		if mf, err := strconv.Atoi(mfStr); err == nil {
			maxForwards = mf
		}
	}
	fwdReq.Headers["Max-Forwards"] = strconv.Itoa(maxForwards - 1)

	// Add our own Via header to the top.
	if maxForwards <= 1 {
		response := BuildResponse(483, "Too Many Hops", req, nil)
		return response.String(), clientAddr, nil
	}
	req.Headers["Max-Forwards"] = strconv.Itoa(maxForwards - 1)

	branch := GenerateBranchID()
	via := fmt.Sprintf("SIP/2.0/UDP %s;branch=%s", s.listenAddr, branch)
	if existingVia := req.GetHeader("Via"); existingVia != "" {
		fwdReq.Headers["Via"] = via + "," + existingVia
	} else {
		fwdReq.Headers["Via"] = via
	}

	// Add Record-Route
	recordRoute := fmt.Sprintf("<sip:%s;lr>", s.listenAddr)
	if existing, ok := req.Headers["Record-Route"]; ok {
		fwdReq.Headers["Record-Route"] = recordRoute + ", " + existing
	} else {
		fwdReq.Headers["Record-Route"] = recordRoute
	}

	destAddr, err := net.ResolveUDPAddr("udp", contactURI.Host+":"+destPort)
	if err != nil {
		return "", nil, fmt.Errorf("could not resolve Contact URI '%s' for user '%s': %v", registration.ContactURI, toURI.User, err)
	}

	log.Printf("Proxying %s for %s to %s", req.Method, toURI.User, destAddr.String())
	return forwardedMsg, destAddr, nil
}


func (s *Server) handleStateless(pc net.PacketConn, req *SIPRequest, remoteAddr net.Addr) {
	// This is now only for methods we don't handle transactionally and don't proxy.
	response := BuildResponse(405, "Method Not Allowed", req, nil)
	if _, err := pc.WriteTo([]byte(response.String()), remoteAddr); err != nil {
		log.Printf("Error sending 405 response: %v", err)
	}
}

func (s *Server) handleRegister(req *SIPRequest) *SIPResponse {
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

func (s *Server) createOKResponse(req *SIPRequest) *SIPResponse {
	headers := map[string]string{
		"Contact": fmt.Sprintf("%s;expires=%d", req.GetHeader("Contact"), req.Expires()),
		"Date":    time.Now().UTC().Format(time.RFC1123),
	}
	return BuildResponse(200, "OK", req, headers)
}

func (s *Server) createUnauthorizedResponse(req *SIPRequest) *SIPResponse {
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
