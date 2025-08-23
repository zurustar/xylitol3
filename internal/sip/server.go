package sip

import (
	"context"
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

// SIPServer holds the dependencies for the SIP server.
type SIPServer struct {
	storage       *storage.Storage
	txManager     *TransactionManager
	registrations map[string]Registration
	regMutex      sync.RWMutex
	nonces        map[string]time.Time
	nonceMutex    sync.Mutex
	realm         string
	listenAddr    string // The address this server is listening on, e.g., "1.2.3.4:5060"
}

// NewSIPServer creates a new SIP server instance.
func NewSIPServer(s *storage.Storage, realm string) *SIPServer {
	return &SIPServer{
		storage:       s,
		txManager:     NewTransactionManager(),
		registrations: make(map[string]Registration),
		nonces:        make(map[string]time.Time),
		realm:         realm,
	}
}

// Run starts the SIP server listening on a UDP port.
func (s *SIPServer) Run(ctx context.Context, addr string) error {
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
		go s.dispatchMessage(pc, message, clientAddr)
	}
}

// dispatchMessage determines if an incoming message is a request or a response
// and routes it to the appropriate handler.
func (s *SIPServer) dispatchMessage(pc net.PacketConn, rawMsg string, remoteAddr net.Addr) {
	// Peek at the first line to see if it's a request or response.
	// Requests start with a method (e.g., INVITE, ACK), responses start with "SIP/2.0".
	if strings.HasPrefix(rawMsg, "SIP/2.0") {
		s.handleResponse(pc, rawMsg, remoteAddr)
	} else {
		s.handleRequest(pc, rawMsg, remoteAddr)
	}
}

// handleResponse parses an incoming SIP response and passes it to the
// appropriate client transaction.
func (s *SIPServer) handleResponse(pc net.PacketConn, rawMsg string, remoteAddr net.Addr) {
	res, err := ParseSIPResponse(rawMsg)
	if err != nil {
		log.Printf("Error parsing SIP response: %v", err)
		return
	}

	topVia, err := res.TopVia()
	if err != nil {
		log.Printf("Could not get Via from response: %v", err)
		return
	}
	branchID := topVia.Branch()

	tx, ok := s.txManager.Get(branchID)
	if !ok {
		log.Printf("No matching client transaction for response with branch %s", branchID)
		return
	}

	if clientTx, ok := tx.(ClientTransaction); ok {
		log.Printf("Passing response to client transaction %s", branchID)
		clientTx.ReceiveResponse(res)
	} else {
		log.Printf("Transaction %s is not a client transaction", branchID)
	}
}

// handleRequest is the entry point for all incoming SIP requests.
// It manages transactions and dispatches requests to the appropriate handlers.
func (s *SIPServer) handleRequest(pc net.PacketConn, rawMsg string, remoteAddr net.Addr) {
	req, err := ParseSIPRequest(rawMsg)
	if err != nil {
		log.Printf("Error parsing SIP request: %v", err)
		return
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
		srvTx, err = NewNonInviteServerTx(req, pc, remoteAddr, "UDP")
	case "INVITE":
		srvTx, err = NewInviteServerTx(req, pc, remoteAddr, "UDP")
	case "ACK":
		// ACKs for 2xx responses are hop-by-hop and stateless from a transaction perspective.
		// They must be routed based on the Route header.
		if req.GetHeader("Route") != "" {
			s.proxyAckRequest(req, pc, remoteAddr)
		} else {
			// This could be an ACK for a non-2xx response that was retransmitted.
			// The transaction layer should have already consumed it. If it reaches here,
			// it's a stray ACK.
			log.Printf("Received ACK without Route header. Assuming stray and dropping.")
		}
		return
	case "BYE":
		srvTx, err = NewNonInviteServerTx(req, pc, remoteAddr, "UDP")
	default:
		log.Printf("Handling method %s statelessly.", req.Method)
		s.handleStateless(pc, req, remoteAddr)
		return
	}

	if err != nil {
		log.Printf("Error creating server transaction: %v", err)
		return
	}
	s.txManager.Add(srvTx)
	go s.handleTransaction(srvTx, pc)
}

// handleTransaction is the Transaction User (TU) for server transactions.
func (s *SIPServer) handleTransaction(srvTx ServerTransaction, pc net.PacketConn) {
	select {
	case req := <-srvTx.Requests():
		switch req.Method {
		case "REGISTER":
			res := s.handleRegister(req)
			if err := srvTx.Respond(res); err != nil {
				log.Printf("Error sending response to transaction %s: %v", srvTx.ID(), err)
			}
		case "INVITE":
			fallthrough
		case "BYE":
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

// doProxy is a dispatcher for stateful proxy logic. It decides whether to handle
// the request as an initial request (using the registrar) or as an in-dialog
// request (using the Route header).
func (s *SIPServer) doProxy(originalReq *SIPRequest, upstreamTx ServerTransaction, pc net.PacketConn) {
	if routeHeader := originalReq.GetHeader("Route"); routeHeader != "" {
		s.proxyRoutedRequest(originalReq, upstreamTx, pc)
	} else {
		s.proxyInitialRequest(originalReq, upstreamTx, pc)
	}
}

// proxyRoutedRequest handles stateful forwarding of in-dialog requests that contain a Route header.
func (s *SIPServer) proxyRoutedRequest(originalReq *SIPRequest, upstreamTx ServerTransaction, pc net.PacketConn) {
	log.Printf("Routing in-dialog %s request", originalReq.Method)
	var err error

	// 1. Validate Route header
	routeHeader := originalReq.GetHeader("Route")
	routes := strings.Split(routeHeader, ",") // Naive split
	topRoute := strings.TrimSpace(routes[0])

	if !strings.Contains(topRoute, s.listenAddr) {
		log.Printf("Received routed request not destined for this proxy. Top Route: %s, My Addr: %s", topRoute, s.listenAddr)
		upstreamTx.Respond(BuildResponse(403, "Forbidden", originalReq, nil))
		return
	}

	// 2. Prepare the forwarded request
	fwdReq := s.prepareForwardedRoutedRequest(originalReq)
	if fwdReq == nil {
		upstreamTx.Respond(BuildResponse(483, "Too Many Hops", originalReq, nil))
		return
	}

	// 3. Determine destination address
	var destURI *SIPURI
	destURI, err = ParseSIPURI(fwdReq.URI)
	if err != nil {
		log.Printf("Could not parse destination URI for routed request: %v", err)
		upstreamTx.Respond(BuildResponse(400, "Bad Request", originalReq, map[string]string{"Warning": "Malformed Request-URI"}))
		return
	}

	destPort := "5060"
	if destURI.Port != "" {
		destPort = destURI.Port
	}
	var destAddr *net.UDPAddr
	destAddr, err = net.ResolveUDPAddr("udp", destURI.Host+":"+destPort)
	if err != nil {
		log.Printf("Could not resolve destination URI '%s': %v", fwdReq.URI, err)
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
		return
	}

	// 4. Create appropriate client transaction
	var clientTx ClientTransaction
	switch fwdReq.Method {
	case "INVITE":
		clientTx, err = NewInviteClientTx(fwdReq, pc, destAddr, "UDP")
	default:
		clientTx, err = NewNonInviteClientTx(fwdReq, pc, destAddr, "UDP")
	}
	if err != nil {
		log.Printf("Failed to create client transaction for routed request: %v", err)
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
		return
	}

	// 5. Forward and stitch responses
	s.txManager.Add(clientTx)
	log.Printf("Proxying routed %s from %s to %s via client tx %s", fwdReq.Method, fwdReq.GetHeader("From"), fwdReq.GetHeader("To"), clientTx.ID())
	s.forwardAndStitch(upstreamTx, clientTx)
}

// prepareForwardedRoutedRequest prepares an in-dialog request for forwarding.
// It removes the top Route header, decrements Max-Forwards, and adds a new Via header.
func (s *SIPServer) prepareForwardedRoutedRequest(req *SIPRequest) *SIPRequest {
	fwdReq := &SIPRequest{
		Method:  req.Method,
		URI:     req.URI,
		Proto:   req.Proto,
		Headers: make(map[string]string),
		Body:    req.Body,
	}
	for k, v := range req.Headers {
		fwdReq.Headers[k] = v
	}

	// 1. Decrement Max-Forwards
	maxForwards := 70
	if mfStr := req.GetHeader("Max-Forwards"); mfStr != "" {
		if mf, err := strconv.Atoi(mfStr); err == nil {
			maxForwards = mf
		}
	}
	if maxForwards <= 1 {
		return nil // Signal to caller to send 483
	}
	fwdReq.Headers["Max-Forwards"] = strconv.Itoa(maxForwards - 1)

	// 2. Add our Via header
	branch := GenerateBranchID()
	via := fmt.Sprintf("SIP/2.0/UDP %s;branch=%s", s.listenAddr, branch)
	fwdReq.Headers["Via"] = via + "," + req.GetHeader("Via")

	// 3. Remove our URI from the Route header
	routeHeader := req.GetHeader("Route")
	routes := strings.Split(routeHeader, ",")
	trimmedRoutes := make([]string, 0, len(routes))
	for _, r := range routes {
		trimmedRoutes = append(trimmedRoutes, strings.TrimSpace(r))
	}

	if len(trimmedRoutes) > 1 {
		fwdReq.Headers["Route"] = strings.Join(trimmedRoutes[1:], ", ")
	} else {
		delete(fwdReq.Headers, "Route")
	}

	return fwdReq
}

// proxyAckRequest handles the stateless forwarding of an in-dialog ACK request.
func (s *SIPServer) proxyAckRequest(req *SIPRequest, pc net.PacketConn, remoteAddr net.Addr) {
	log.Printf("Routing in-dialog ACK request")
	var err error

	// 1. Validate Route header
	routeHeader := req.GetHeader("Route")
	routes := strings.Split(routeHeader, ",")
	topRoute := strings.TrimSpace(routes[0])
	if !strings.Contains(topRoute, s.listenAddr) {
		log.Printf("Received ACK with Route header not for this proxy. Dropping.")
		return
	}

	// 2. Prepare the forwarded request
	fwdReq := s.prepareForwardedRoutedRequest(req)
	if fwdReq == nil {
		log.Printf("Dropping ACK due to Max-Forwards limit.")
		return // Can't send a response to an ACK
	}

	// 3. Determine destination
	var destURI *SIPURI
	destURI, err = ParseSIPURI(fwdReq.URI)
	if err != nil {
		log.Printf("Could not parse destination URI for ACK: %v. Dropping.", err)
		return
	}
	destPort := "5060"
	if destURI.Port != "" {
		destPort = destURI.Port
	}
	var destAddr *net.UDPAddr
	destAddr, err = net.ResolveUDPAddr("udp", destURI.Host+":"+destPort)
	if err != nil {
		log.Printf("Could not resolve destination for ACK: %v. Dropping.", err)
		return
	}

	// 4. Send it.
	log.Printf("Proxying ACK to %s", destAddr.String())
	if _, err = pc.WriteTo([]byte(fwdReq.String()), destAddr); err != nil {
		log.Printf("Error sending proxied ACK: %v", err)
	}
}

// proxyInitialRequest is the stateful proxy logic for initial requests (no Route header).
func (s *SIPServer) proxyInitialRequest(originalReq *SIPRequest, upstreamTx ServerTransaction, pc net.PacketConn) {
	var err error

	var toURI *SIPURI
	toURI, err = originalReq.GetSIPURIHeader("To")
	if err != nil {
		upstreamTx.Respond(BuildResponse(400, "Bad Request", originalReq, map[string]string{"Warning": "Malformed To header"}))
		return
	}
	if toURI.Host != s.realm {
		log.Printf("Proxy received request for unknown realm: %s", toURI.Host)
		upstreamTx.Respond(BuildResponse(404, "Not Found", originalReq, nil))
		return
	}

	s.regMutex.RLock()
	registration, ok := s.registrations[toURI.User]
	s.regMutex.RUnlock()

	if !ok {
		log.Printf("Proxy lookup failed for user '%s': not registered", toURI.User)
		upstreamTx.Respond(BuildResponse(480, "Temporarily Unavailable", originalReq, nil))
		return
	}

	var contactURI *SIPURI
	contactURI, err = ParseSIPURI(registration.ContactURI)
	if err != nil {
		log.Printf("Error parsing stored Contact URI for user '%s': %v", toURI.User, err)
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
		return
	}

	destPort := "5060"
	if contactURI.Port != "" {
		destPort = contactURI.Port
	}
	var destAddr *net.UDPAddr
	destAddr, err = net.ResolveUDPAddr("udp", contactURI.Host+":"+destPort)
	if err != nil {
		log.Printf("Could not resolve Contact URI '%s' for user '%s': %v", registration.ContactURI, toURI.User, err)
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
		return
	}

	fwdReq := s.prepareForwardedRequest(originalReq, registration.ContactURI)

	var clientTx ClientTransaction
	switch fwdReq.Method {
	case "INVITE":
		clientTx, err = NewInviteClientTx(fwdReq, pc, destAddr, "UDP")
	default:
		clientTx, err = NewNonInviteClientTx(fwdReq, pc, destAddr, "UDP")
	}

	if err != nil {
		log.Printf("Failed to create client transaction: %v", err)
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
		return
	}
	s.txManager.Add(clientTx)
	log.Printf("Proxying initial %s from %s to %s via client tx %s", originalReq.Method, originalReq.GetHeader("From"), originalReq.GetHeader("To"), clientTx.ID())

	s.forwardAndStitch(upstreamTx, clientTx)
}

// forwardAndStitch handles the forwarding of a request via a client transaction
// and stitches the response back to the upstream server transaction.
func (s *SIPServer) forwardAndStitch(upstreamTx ServerTransaction, clientTx ClientTransaction) {
	go func() {
		for {
			select {
			case res := <-clientTx.Responses():
				if res == nil {
					continue
				}
				log.Printf("Proxy received response for tx %s: %d %s", clientTx.ID(), res.StatusCode, res.Reason)

				topVia, err := res.TopVia()
				if err != nil {
					log.Printf("Could not parse Via header from response: %v. Dropping response.", err)
					continue
				}

				if topVia.Branch() == clientTx.ID() {
					res.PopVia()
				} else {
					log.Printf("WARN: Top Via branch '%s' in response did not match client tx branch '%s'. Not stripping Via.", topVia.Branch(), clientTx.ID())
				}
				upstreamTx.Respond(res)
				if res.StatusCode >= 200 {
					return
				}
			case <-clientTx.Done():
				log.Printf("Client transaction %s terminated.", clientTx.ID())
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
func (s *SIPServer) prepareForwardedRequest(req *SIPRequest, targetURI string) *SIPRequest {
	fwdReq := &SIPRequest{
		Method:  req.Method,
		URI:     targetURI,
		Proto:   req.Proto,
		Headers: make(map[string]string),
		Body:    req.Body,
	}
	for k, v := range req.Headers {
		fwdReq.Headers[k] = v
	}

	maxForwards := 70
	if mfStr := req.GetHeader("Max-Forwards"); mfStr != "" {
		if mf, err := strconv.Atoi(mfStr); err == nil {
			maxForwards = mf
		}
	}
	if maxForwards <= 1 {
		return nil // Should be handled by caller by sending 483
	}
	fwdReq.Headers["Max-Forwards"] = strconv.Itoa(maxForwards - 1)

	branch := GenerateBranchID()
	via := fmt.Sprintf("SIP/2.0/UDP %s;branch=%s", s.listenAddr, branch)
	if existingVia := req.GetHeader("Via"); existingVia != "" {
		fwdReq.Headers["Via"] = via + "," + existingVia
	} else {
		fwdReq.Headers["Via"] = via
	}

	recordRoute := fmt.Sprintf("<sip:%s;lr>", s.listenAddr)
	if existing, ok := req.Headers["Record-Route"]; ok {
		fwdReq.Headers["Record-Route"] = recordRoute + ", " + existing
	} else {
		fwdReq.Headers["Record-Route"] = recordRoute
	}
	return fwdReq
}

func (s *SIPServer) handleStateless(pc net.PacketConn, req *SIPRequest, remoteAddr net.Addr) {
	response := BuildResponse(405, "Method Not Allowed", req, nil)
	if _, err := pc.WriteTo([]byte(response.String()), remoteAddr); err != nil {
		log.Printf("Error sending 405 response: %v", err)
	}
}

func (s *SIPServer) handleRegister(req *SIPRequest) *SIPResponse {
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

func (s *SIPServer) updateRegistration(req *SIPRequest) {
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

func (s *SIPServer) createOKResponse(req *SIPRequest) *SIPResponse {
	headers := map[string]string{
		"Contact": fmt.Sprintf("%s;expires=%d", req.GetHeader("Contact"), req.Expires()),
		"Date":    time.Now().UTC().Format(time.RFC1123),
	}
	return BuildResponse(200, "OK", req, headers)
}

func (s *SIPServer) createUnauthorizedResponse(req *SIPRequest) *SIPResponse {
	nonce, _ := s.generateNonce()
	authValue := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5, qop="auth"`, s.realm, nonce)
	headers := map[string]string{
		"WWW-Authenticate": authValue,
	}
	return BuildResponse(401, "Unauthorized", req, headers)
}

func (s *SIPServer) generateNonce() (string, error) {
	nonce := GenerateNonce(8)
	s.nonceMutex.Lock()
	defer s.nonceMutex.Unlock()
	s.nonces[nonce] = time.Now().Add(5 * time.Minute)
	return nonce, nil
}

func (s *SIPServer) validateNonce(nonce string) bool {
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
