package sip

import (
	"bufio"
	"context"
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

// Registration holds information about a user's registered location.
type Registration struct {
	ContactURI string
	ExpiresAt  time.Time
}

// SessionState holds the state for a session with an active session timer.
type SessionState struct {
	DialogID  string
	Interval  int
	ExpiresAt time.Time
	Refresher string // "uac" or "uas"
	timer     *time.Timer
}

// SIPServer holds the dependencies for the SIP server.
type SIPServer struct {
	storage       *storage.Storage
	txManager     *TransactionManager
	registrations map[string][]Registration
	regMutex      sync.RWMutex
	nonces        map[string]time.Time
	nonceMutex    sync.Mutex
	realm         string
	listenAddr    string // The address this server is listening on, e.g., "1.2.3.4:5060"
	sessions      map[string]*SessionState
	sessionMutex  sync.RWMutex
	dialogs       sync.Map // Stores active B2BUA dialogs, keyed by dialog ID
	b2buaByTx     map[string]*B2BUA // Keyed by A-leg transaction ID
	b2buaMutex    sync.RWMutex
	udpConn       net.PacketConn
	tcpListener   *net.TCPListener
}

// NewSIPServer creates a new SIP server instance.
func NewSIPServer(s *storage.Storage, realm string) *SIPServer {
	return &SIPServer{
		storage:       s,
		txManager:     NewTransactionManager(),
		registrations: make(map[string][]Registration),
		nonces:        make(map[string]time.Time),
		realm:         realm,
		sessions:      make(map[string]*SessionState),
		b2buaByTx:     make(map[string]*B2BUA),
	}
}

// NewClientTx creates a new client transaction based on the request method.
func (s *SIPServer) NewClientTx(req *SIPRequest, transport Transport) (ClientTransaction, error) {
	var clientTx ClientTransaction
	var err error
	switch req.Method {
	case "INVITE", "UPDATE":
		clientTx, err = NewInviteClientTx(req, transport)
	default:
		clientTx, err = NewNonInviteClientTx(req, transport)
	}
	if err != nil {
		return nil, err
	}
	s.txManager.Add(clientTx)
	return clientTx, nil
}

// getDialogID generates a unique identifier for a dialog.
// The order of tags is important for lookup.
func getDialogID(callID, fromTag, toTag string) string {
	return fmt.Sprintf("%s:%s:%s", callID, fromTag, toTag)
}

// Run starts the SIP server, listening on both UDP and TCP ports.
func (s *SIPServer) Run(ctx context.Context, addr string) error {
	g, gCtx := errgroup.WithContext(ctx)

	// Resolve the address to get a common host/port for logging.
	// We use UDP address resolution as the baseline.
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("could not resolve UDP address: %w", err)
	}
	s.listenAddr = udpAddr.String()
	if udpAddr.IP.IsUnspecified() {
		// If listening on 0.0.0.0, use a loopback address for headers,
		// assuming this is for local testing. A public IP should be configured
		// in a real deployment.
		s.listenAddr = fmt.Sprintf("127.0.0.1:%d", udpAddr.Port)
	}

	// --- UDP Listener ---
	g.Go(func() error {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return fmt.Errorf("could not listen on UDP: %w", err)
		}
		defer pc.Close()
		s.udpConn = pc
		log.Printf("SIP server listening on UDP %s", pc.LocalAddr())

		go func() {
			<-gCtx.Done()
			pc.Close()
		}()

		buf := make([]byte, 4096)
		for {
			n, clientAddr, err := pc.ReadFrom(buf)
			if err != nil {
				if gCtx.Err() != nil {
					return nil // Graceful shutdown
				}
				log.Printf("Error reading from UDP: %v", err)
				continue
			}

			message := string(buf[:n])
			transport := NewUDPTransport(pc, clientAddr)
			go s.dispatchMessage(transport, message)
		}
	})

	// --- TCP Listener ---
	g.Go(func() error {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("could not listen on TCP: %w", err)
		}
		defer listener.Close()
		s.tcpListener = listener.(*net.TCPListener)
		log.Printf("SIP server listening on TCP %s", listener.Addr())

		go func() {
			<-gCtx.Done()
			listener.Close()
		}()

		for {
			conn, err := listener.Accept()
			if err != nil {
				if gCtx.Err() != nil {
					return nil // Graceful shutdown
				}
				log.Printf("Error accepting TCP connection: %v", err)
				continue
			}
			go s.handleConnection(gCtx, conn)
		}
	})

	log.Printf("SIP server started with UDP and TCP listeners on %s", s.listenAddr)
	return g.Wait()
}

// handleConnection reads SIP messages from a TCP connection in a loop and dispatches them.
func (s *SIPServer) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	transport := NewTCPTransport(conn)
	reader := bufio.NewReader(conn)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var headers strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					log.Printf("Error reading from TCP connection %s: %v", conn.RemoteAddr(), err)
				}
				return
			}
			headers.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		headerStr := headers.String()

		contentLength := parseContentLength(headerStr)
		body := make([]byte, contentLength)
		if contentLength > 0 {
			if _, err := io.ReadFull(reader, body); err != nil {
				log.Printf("Error reading body from TCP connection %s: %v", conn.RemoteAddr(), err)
				return
			}
		}

		message := headerStr + string(body)
		go s.dispatchMessage(transport, message)
	}
}

// parseContentLength is a helper to extract the Content-Length value from SIP headers.
func parseContentLength(headerStr string) int {
	lines := strings.Split(headerStr, "\r\n")
	for _, line := range lines {
		lowerLine := strings.ToLower(line)
		if strings.HasPrefix(lowerLine, "content-length:") || strings.HasPrefix(lowerLine, "l:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				lengthStr := strings.TrimSpace(parts[1])
				length, err := strconv.Atoi(lengthStr)
				if err == nil {
					return length
				}
			}
		}
	}
	return 0
}

// dispatchMessage determines if an incoming message is a request or a response
// and routes it to the appropriate handler.
func (s *SIPServer) dispatchMessage(transport Transport, rawMsg string) {
	// Peek at the first line to see if it's a request or response.
	// Requests start with a method (e.g., INVITE, ACK), responses start with "SIP/2.0".
	if strings.HasPrefix(rawMsg, "SIP/2.0") {
		s.handleResponse(transport, rawMsg)
	} else {
		s.handleRequest(transport, rawMsg)
	}
}

// handleResponse parses an incoming SIP response and passes it to the
// appropriate client transaction.
func (s *SIPServer) handleResponse(transport Transport, rawMsg string) {
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
func (s *SIPServer) handleRequest(transport Transport, rawMsg string) {
	req, err := ParseSIPRequest(rawMsg)
	if err != nil {
		log.Printf("Error parsing SIP request: %v", err)
		return
	}

	// RFC 3261 Section 16.3 Item 3: Loop Detection
	vias, err := req.AllVias()
	if err != nil {
		log.Printf("Error parsing Via headers: %v", err)
		// We can't be sure about loops, but the request is likely malformed.
		// For now, we'll let it fail later if it's critical.
	} else {
		listenHost, listenPort, _ := net.SplitHostPort(s.listenAddr)
		if listenHost == "" {
			listenHost = s.listenAddr // Fallback if port is missing, though it shouldn't be.
		}

		for _, via := range vias {
			// Compare the host and port of the Via header with our own listen address.
			viaPort := via.Port
			if viaPort == "" {
				// Per RFC 3261 Section 18.2.1, if the port is absent, the default
				// is 5060 for sip, and 5061 for sips. We only handle sip here.
				viaPort = "5060"
			}

			if via.Host == listenHost && viaPort == listenPort {
				log.Printf("Loop detected: request Via header contains this proxy's address (%s).", s.listenAddr)
				res := BuildResponse(482, "Loop Detected", req, nil)
				if _, err := transport.Write([]byte(res.String())); err != nil {
					log.Printf("Error sending 482 Loop Detected response: %v", err)
				}
				return // Stop processing
			}
		}
	}

	// For in-dialog requests (except for initial INVITE), we need to find the B2BUA.
	if req.Method != "INVITE" {
		// This is a simplified dialog lookup. A robust implementation would handle loose routing.
		dialogID := getDialogID(req.GetHeader("Call-ID"), getTag(req.GetHeader("From")), getTag(req.GetHeader("To")))
		if val, ok := s.dialogs.Load(dialogID); ok {
			b2bua := val.(*B2BUA)
			// These requests need a transaction to be handled statefully by the B2BUA.
			srvTx, err := NewNonInviteServerTx(req, transport)
			if err != nil {
				log.Printf("Error creating server transaction for in-dialog request: %v", err)
				return
			}
			s.txManager.Add(srvTx)
			b2bua.HandleInDialogRequest(req, srvTx)
			return
		}
	}

	// Per RFC 3261, CANCEL is a special hop-by-hop request.
	if req.Method == "CANCEL" {
		s.handleCancel(transport, req)
		return
	}

	// ACK for a non-2xx response might not have a dialog yet.
	if req.Method == "ACK" {
		topVia, err := req.TopVia()
		if err == nil {
			if tx, ok := s.txManager.Get(topVia.Branch()); ok {
				if srvTx, ok := tx.(*InviteServerTx); ok {
					log.Printf("Passing ACK for non-2xx response to Invite Server Tx %s", srvTx.ID())
					srvTx.Receive(req)
					return
				}
			}
		}
		log.Printf("Received ACK that did not match any transaction or dialog. Dropping.")
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
		s.handleStateless(transport, req)
		return
	}

	// Check for retransmissions.
	if tx, ok := s.txManager.Get(branchID); ok {
		log.Printf("Retransmission of %s received for transaction %s", req.Method, branchID)
		if srvTx, ok := tx.(ServerTransaction); ok {
			srvTx.Receive(req)
		}
		return
	}

	// If it's a new request, create a new transaction.
	log.Printf("New request received, creating transaction %s for method %s", branchID, req.Method)
	var srvTx ServerTransaction
	switch req.Method {
	case "REGISTER":
		srvTx, err = NewNonInviteServerTx(req, transport)
	case "INVITE", "UPDATE":
		srvTx, err = NewInviteServerTx(req, transport)
	case "BYE":
		srvTx, err = NewNonInviteServerTx(req, transport)
	default:
		log.Printf("Handling method %s statelessly.", req.Method)
		s.handleStateless(transport, req)
		return
	}

	if err != nil {
		log.Printf("Error creating server transaction: %v", err)
		return
	}
	s.txManager.Add(srvTx)
	go s.handleTransaction(srvTx)
}

// handleTransaction is the Transaction User (TU) for server transactions.
func (s *SIPServer) handleTransaction(srvTx ServerTransaction) {
	select {
	case req := <-srvTx.Requests():
		switch req.Method {
		case "REGISTER":
			res := s.handleRegister(req)
			if err := srvTx.Respond(res); err != nil {
				log.Printf("Error sending response to transaction %s: %v", srvTx.ID(), err)
			}
		case "INVITE", "UPDATE":
			s.handleInvite(req, srvTx)
		default:
			// BYE and other in-dialog methods are handled in handleRequest
			res := BuildResponse(405, "Method Not Allowed", req, nil)
			if err := srvTx.Respond(res); err != nil {
				log.Printf("Error sending response to transaction %s: %v", srvTx.ID(), err)
			}
		}
	case <-srvTx.Done():
	}
}

// handleInvite decides whether to start a B2BUA for a new call or
// pass a re-INVITE to an existing dialog.
func (s *SIPServer) handleInvite(req *SIPRequest, tx ServerTransaction) {
	// A re-INVITE will have a To-tag. An initial INVITE will not.
	if getTag(req.GetHeader("To")) != "" {
		dialogID := getDialogID(req.GetHeader("Call-ID"), getTag(req.GetHeader("From")), getTag(req.GetHeader("To")))
		if val, ok := s.dialogs.Load(dialogID); ok {
			b2bua := val.(*B2BUA)
			log.Printf("Passing re-INVITE to existing B2BUA for dialog %s", dialogID)
			b2bua.HandleInDialogRequest(req, tx)
			return
		}
		log.Printf("Received re-INVITE for unknown dialog %s. Rejecting.", dialogID)
		tx.Respond(BuildResponse(481, "Call/Transaction Does Not Exist", req, nil))
		return
	}

	// This is an initial INVITE, so we set up a new B2BUA.
	toURI, err := req.GetSIPURIHeader("To")
	if err != nil {
		tx.Respond(BuildResponse(400, "Bad Request", req, map[string]string{"Warning": "Malformed To header"}))
		return
	}
	if toURI.Host != s.realm {
		log.Printf("Request for unknown realm: %s", toURI.Host)
		tx.Respond(BuildResponse(404, "Not Found", req, nil))
		return
	}

	s.regMutex.RLock()
	registrations, ok := s.registrations[toURI.User]
	s.regMutex.RUnlock()

	activeRegistrations := []Registration{}
	if ok {
		s.regMutex.Lock()
		now := time.Now()
		for _, reg := range registrations {
			if now.Before(reg.ExpiresAt) {
				activeRegistrations = append(activeRegistrations, reg)
			}
		}
		if len(activeRegistrations) > 0 {
			s.registrations[toURI.User] = activeRegistrations
		} else {
			delete(s.registrations, toURI.User)
		}
		s.regMutex.Unlock()
	}

	if len(activeRegistrations) == 0 {
		log.Printf("User '%s' not registered or all contacts expired", toURI.User)
		tx.Respond(BuildResponse(480, "Temporarily Unavailable", req, nil))
		return
	}

	targetContact := activeRegistrations[0].ContactURI
	log.Printf("Found contact for user %s: %s. Setting up B2BUA.", toURI.User, targetContact)

	b2bua := NewB2BUA(s, tx)
	s.b2buaMutex.Lock()
	s.b2buaByTx[tx.ID()] = b2bua
	s.b2buaMutex.Unlock()

	// The B2BUA is responsible for this transaction from now on.
	go b2bua.Run(targetContact)
}

func (s *SIPServer) handleStateless(transport Transport, req *SIPRequest) {
	response := BuildResponse(405, "Method Not Allowed", req, nil)
	if _, err := transport.Write([]byte(response.String())); err != nil {
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
	expires := req.Expires()

	// Handle wildcard un-registration
	if contactHeader == "*" && expires == 0 {
		delete(s.registrations, username)
		log.Printf("Unregistered all contacts for user '%s'", username)
		return
	}

	// Per RFC 3261, a REGISTER can have multiple contacts in a comma-separated list.
	contactHeaders := strings.Split(contactHeader, ",")

	for _, singleContact := range contactHeaders {
		trimmedContact := strings.TrimSpace(singleContact)
		start := strings.Index(trimmedContact, "<")
		end := strings.Index(trimmedContact, ">")
		var contactURI string
		if start != -1 && end != -1 {
			contactURI = trimmedContact[start+1 : end]
		} else {
			contactURI = trimmedContact
		}

		if expires > 0 {
			s.addOrUpdateContact(username, contactURI, expires)
		} else {
			s.removeContact(username, contactURI)
		}
	}
}

func (s *SIPServer) addOrUpdateContact(username, contactURI string, expires int) {
	existingRegs, ok := s.registrations[username]
	if !ok {
		existingRegs = []Registration{}
	}

	found := false
	for i := range existingRegs {
		if existingRegs[i].ContactURI == contactURI {
			existingRegs[i].ExpiresAt = time.Now().Add(time.Duration(expires) * time.Second)
			found = true
			log.Printf("Updated registration for user '%s' at %s, expires in %d seconds", username, contactURI, expires)
			break
		}
	}

	if !found {
		newReg := Registration{
			ContactURI: contactURI,
			ExpiresAt:  time.Now().Add(time.Duration(expires) * time.Second),
		}
		existingRegs = append(existingRegs, newReg)
		log.Printf("Added new registration for user '%s' at %s, expires in %d seconds", username, contactURI, expires)
	}

	s.registrations[username] = existingRegs
}

func (s *SIPServer) removeContact(username, contactURI string) {
	existingRegs, ok := s.registrations[username]
	if !ok {
		return
	}

	updatedRegs := []Registration{}
	for _, reg := range existingRegs {
		if reg.ContactURI != contactURI {
			updatedRegs = append(updatedRegs, reg)
		} else {
			log.Printf("Removed registration for user '%s' at %s", username, contactURI)
		}
	}

	if len(updatedRegs) > 0 {
		s.registrations[username] = updatedRegs
	} else {
		delete(s.registrations, username)
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

func (s *SIPServer) handleCancel(transport Transport, req *SIPRequest) {
	log.Printf("Handling CANCEL request for Call-ID %s", req.GetHeader("Call-ID"))

	// Immediately respond 200 OK to the CANCEL request, as per RFC 3261.
	res200 := BuildResponse(200, "OK", req, nil)
	if _, err := transport.Write([]byte(res200.String())); err != nil {
		log.Printf("Error sending 200 OK for CANCEL: %v", err)
	}

	topVia, err := req.TopVia()
	if err != nil {
		log.Printf("Could not get Via from CANCEL request: %v", err)
		return
	}
	branchID := topVia.Branch()

	s.b2buaMutex.RLock()
	b2bua, ok := s.b2buaByTx[branchID]
	s.b2buaMutex.RUnlock()

	if ok {
		log.Printf("Found B2BUA for tx %s, telling it to cancel.", branchID)
		b2bua.Cancel()
	} else {
		log.Printf("Could not find a B2BUA for CANCEL with branch ID %s. The INVITE transaction might have already completed.", branchID)
	}
}

func createCancelRequest(inviteReq *SIPRequest) *SIPRequest {
	cancelReq := &SIPRequest{
		Method:  "CANCEL",
		URI:     inviteReq.URI,
		Proto:   inviteReq.Proto,
		Headers: make(map[string]string),
		Body:    []byte{},
	}
	// Copy essential headers per RFC 3261 Section 16.3
	for _, h := range []string{"From", "To", "Call-ID", "Via"} {
		cancelReq.Headers[h] = inviteReq.Headers[h]
	}

	// CSeq number must be the same, but the method is CANCEL
	cseqStr := inviteReq.GetHeader("CSeq")
	if parts := strings.SplitN(cseqStr, " ", 2); len(parts) == 2 {
		cancelReq.Headers["CSeq"] = parts[0] + " CANCEL"
	} else {
		// This should not happen with valid requests
		cancelReq.Headers["CSeq"] = "1 CANCEL"
	}
	cancelReq.Headers["Max-Forwards"] = "70"
	return cancelReq
}

func getTag(headerValue string) string {
	parts := strings.Split(headerValue, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "tag=") {
			return strings.TrimPrefix(part, "tag=")
		}
	}
	return ""
}

func (s *SIPServer) createOrUpdateSession(upstreamTx ServerTransaction, finalResponse *SIPResponse, se *SessionExpires) {
	if se == nil || se.Refresher == "" {
		return
	}

	callID := finalResponse.GetHeader("Call-ID")
	fromTag := getTag(finalResponse.GetHeader("From"))
	toTag := getTag(finalResponse.GetHeader("To"))

	if callID == "" || fromTag == "" || toTag == "" {
		log.Printf("Cannot create session state: missing Call-ID or tags in 2xx response.")
		return
	}

	dialogID := getDialogID(callID, fromTag, toTag)

	s.sessionMutex.Lock()
	defer s.sessionMutex.Unlock()

	if existingSession, ok := s.sessions[dialogID]; ok {
		existingSession.timer.Stop()
	}

	session := &SessionState{
		DialogID:  dialogID,
		Interval:  se.Delta,
		ExpiresAt: time.Now().Add(time.Duration(se.Delta) * time.Second),
		Refresher: se.Refresher,
	}

	log.Printf("Session timer established for dialog %s. Interval: %d, Refresher: %s", dialogID, session.Interval, session.Refresher)

	session.timer = time.AfterFunc(time.Duration(session.Interval)*time.Second, func() {
		s.expireSession(dialogID)
	})

	s.sessions[dialogID] = session
}

func (s *SIPServer) expireSession(dialogID string) {
	s.sessionMutex.Lock()
	defer s.sessionMutex.Unlock()

	if _, ok := s.sessions[dialogID]; ok {
		log.Printf("Session timer expired for dialog %s. Removing state.", dialogID)
		delete(s.sessions, dialogID)
	}
}

// SessionInfo provides a snapshot of an active session for display purposes.
type SessionInfo struct {
	Caller   string
	Callee   string
	Duration time.Duration
	CallID   string
}

// GetActiveSessions returns a slice of SessionInfo for all active dialogs.
func (s *SIPServer) GetActiveSessions() []SessionInfo {
	var sessions []SessionInfo
	s.dialogs.Range(func(key, value interface{}) bool {
		b2bua, ok := value.(*B2BUA)
		if !ok {
			return true // continue
		}

		b2bua.mu.RLock()
		defer b2bua.mu.RUnlock()

		if b2bua.aLegTx == nil || b2bua.aLegTx.OriginalRequest() == nil {
			return true
		}

		// To avoid showing the same session twice (since we store by both a-leg and b-leg dialog IDs),
		// we only act when we're iterating on the A-leg's ID.
		// Also check that the dialog has been fully established.
		if b2bua.aLegDialogID == "" || key.(string) != b2bua.aLegDialogID {
			return true
		}

		origReq := b2bua.aLegTx.OriginalRequest()
		info := SessionInfo{
			Caller:   origReq.GetHeader("From"),
			Callee:   origReq.GetHeader("To"),
			Duration: time.Since(b2bua.StartTime).Round(time.Second),
			CallID:   origReq.GetHeader("Call-ID"),
		}
		sessions = append(sessions, info)

		return true // continue iteration
	})
	return sessions
}
