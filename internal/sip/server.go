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
	storage        *storage.Storage
	txManager      *TransactionManager
	registrations  map[string]Registration
	regMutex       sync.RWMutex
	nonces         map[string]time.Time
	nonceMutex     sync.Mutex
	realm          string
	listenAddr     string // The address this server is listening on, e.g., "1.2.3.4:5060"
	proxiedTxs     map[string]ClientTransaction
	proxiedTxMutex sync.RWMutex
	sessions       map[string]*SessionState
	sessionMutex   sync.RWMutex
	udpConn        net.PacketConn
	tcpListener    *net.TCPListener
}

// NewSIPServer creates a new SIP server instance.
func NewSIPServer(s *storage.Storage, realm string) *SIPServer {
	return &SIPServer{
		storage:       s,
		txManager:     NewTransactionManager(),
		registrations: make(map[string]Registration),
		nonces:        make(map[string]time.Time),
		realm:         realm,
		proxiedTxs:    make(map[string]ClientTransaction),
		sessions:      make(map[string]*SessionState),
	}
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

	// Per RFC 3261, CANCEL and ACK are special hop-by-hop requests.
	// They are handled outside of the standard transaction state machines.
	if req.Method == "CANCEL" {
		s.handleCancel(transport, req)
		return
	}
	if req.Method == "ACK" {
		// ACKs for 2xx responses are hop-by-hop and stateless from a transaction perspective.
		// They must be routed based on the Route header.
		if req.GetHeader("Route") != "" {
			s.proxyAckRequest(req, transport)
		} else {
			// This could be an ACK for a non-2xx response that was retransmitted to us.
			// Find the transaction and pass it the ACK.
			topVia, err := req.TopVia()
			if err == nil {
				if tx, ok := s.txManager.Get(topVia.Branch()); ok {
					if srvTx, ok := tx.(*InviteServerTx); ok {
						log.Printf("Passing ACK to Invite Server Tx %s", srvTx.ID())
						srvTx.Receive(req)
						return
					}
				}
			}
			log.Printf("Received ACK without Route header and no matching transaction. Assuming stray and dropping.")
		}
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
		case "INVITE", "BYE", "UPDATE":
			s.doProxy(req, srvTx)
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
func (s *SIPServer) doProxy(originalReq *SIPRequest, upstreamTx ServerTransaction) {
	// RFC 3261 Section 16.3, item 5: Proxy-Require
	if proxyRequire := originalReq.GetHeader("Proxy-Require"); proxyRequire != "" {
		// This proxy doesn't support any extensions.
		log.Printf("Rejecting request with unsupported Proxy-Require: %s", proxyRequire)
		extraHeaders := map[string]string{"Unsupported": proxyRequire}
		upstreamTx.Respond(BuildResponse(420, "Bad Extension", originalReq, extraHeaders))
		return
	}

	// RFC 3261 Section 16.4: Route Information Preprocessing (maddr)
	parsedURI, err := ParseSIPURI(originalReq.URI)
	if err == nil { // Only proceed if URI parsing was successful
		if maddr, ok := parsedURI.Params["maddr"]; ok {
			listenHost, _, err := net.SplitHostPort(s.listenAddr)
			if err != nil {
				// Fallback for addresses without a port, though listenAddr should always have one.
				listenHost = s.listenAddr
			}

			// Check if maddr matches our host or realm.
			if maddr == listenHost || maddr == s.realm {
				log.Printf("Request-URI has maddr '%s' matching this proxy. Stripping it.", maddr)
				delete(parsedURI.Params, "maddr")
				originalReq.URI = parsedURI.String()
			}
		}
	}

	// Session Timer Logic - Section 8.1 of RFC 4028
	if originalReq.Method == "INVITE" || originalReq.Method == "UPDATE" {
		const proxyMinSE = 1800 // 30 minutes, as recommended.

		se, err := originalReq.SessionExpires()
		if err != nil {
			upstreamTx.Respond(BuildResponse(400, "Bad Request", originalReq, map[string]string{"Warning": "Malformed Session-Expires header"}))
			return
		}

		if se != nil { // A session timer has been requested.
			if se.Delta < proxyMinSE {
				supportedHeader := originalReq.GetHeader("Supported")
				if strings.Contains(strings.ToLower(supportedHeader), "timer") {
					log.Printf("Session-Expires %d is too small. Rejecting with 422.", se.Delta)
					extraHeaders := map[string]string{"Min-SE": strconv.Itoa(proxyMinSE)}
					upstreamTx.Respond(BuildResponse(422, "Session Interval Too Small", originalReq, extraHeaders))
					return
				} else {
					// UAC is not timer-aware, so we must not reject.
					// Instead, we modify the request before forwarding.
					minSEVal, _ := originalReq.MinSE() // -1 if not present
					if minSEVal < proxyMinSE {
						originalReq.Headers["Min-SE"] = strconv.Itoa(proxyMinSE)
						minSEVal = proxyMinSE
					}

					if se.Delta < minSEVal {
						originalReq.Headers["Session-Expires"] = strconv.Itoa(minSEVal)
					}
				}
			}
		}
	}

	if routeHeader := originalReq.GetHeader("Route"); routeHeader != "" {
		s.proxyRoutedRequest(originalReq, upstreamTx)
	} else {
		s.proxyInitialRequest(originalReq, upstreamTx)
	}
}

// proxyRoutedRequest handles stateful forwarding of in-dialog requests that contain a Route header.
func (s *SIPServer) proxyRoutedRequest(originalReq *SIPRequest, upstreamTx ServerTransaction) {
	log.Printf("Routing in-dialog %s request", originalReq.Method)
	var err error

	// 1. Validate Route header points to this proxy
	routeHeader := originalReq.GetHeader("Route")
	routes := strings.Split(routeHeader, ",") // Naive split, but ok for now
	topRoute := strings.TrimSpace(routes[0])

	if !strings.Contains(topRoute, s.listenAddr) {
		log.Printf("Received routed request not destined for this proxy. Top Route: %s, My Addr: %s", topRoute, s.listenAddr)
		upstreamTx.Respond(BuildResponse(403, "Forbidden", originalReq, nil))
		return
	}

	inboundProto := upstreamTx.Transport().GetProto()

	// 2. Prepare the forwarded request (strips our route from the Route header)
	fwdReq := s.prepareForwardedRoutedRequest(originalReq, inboundProto)
	if fwdReq == nil {
		upstreamTx.Respond(BuildResponse(483, "Too Many Hops", originalReq, nil))
		return
	}

	// 3. Determine destination from next hop in Route header and handle strict routing
	var targetURIStr string
	nextHopRouteHeader := fwdReq.GetHeader("Route")
	if nextHopRouteHeader != "" {
		// RFC 16.6 Step 7: The next hop is the first URI in the Route header.
		nextRoutes := strings.Split(nextHopRouteHeader, ",")
		firstHop := strings.TrimSpace(nextRoutes[0])
		targetURIStr = firstHop

		// RFC 16.6 Step 6: Check for strict routing (lr parameter)
		firstHopURI, err := ParseSIPURI(firstHop)
		if err != nil {
			log.Printf("Could not parse next hop Route URI: %v", err)
			upstreamTx.Respond(BuildResponse(400, "Bad Request", originalReq, map[string]string{"Warning": "Malformed Route header"}))
			return
		}

		if _, lrExists := firstHopURI.Params["lr"]; !lrExists {
			// Strict router detected. Reformat the request as per RFC 16.6, step 6.
			log.Printf("Next hop %s is a strict router. Reformatting request.", firstHop)

			// The new Request-URI is the strict router's URI.
			fwdReq.URI = firstHop

			// Update Route header: remaining routes + original Request-URI at the end.
			remainingRoutes := nextRoutes[1:]
			finalRoute := append(remainingRoutes, "<"+originalReq.URI+">")
			fwdReq.Headers["Route"] = strings.Join(finalRoute, ", ")
		}
	} else {
		// Route set is now empty, next hop is the Request-URI.
		targetURIStr = fwdReq.URI
	}

	// 4. Resolve destination address
	destURI, err := ParseSIPURI(targetURIStr)
	if err != nil {
		log.Printf("Could not parse destination URI '%s': %v", targetURIStr, err)
		upstreamTx.Respond(BuildResponse(400, "Bad Request", originalReq, map[string]string{"Warning": "Malformed target URI"}))
		return
	}
	destPort := "5060"
	if destURI.Port != "" {
		destPort = destURI.Port
	}
	destHost := destURI.Host

	// 5. Create appropriate client transaction based on transport
	var clientTx ClientTransaction
	var outboundTransport Transport
	if strings.ToUpper(inboundProto) == "UDP" {
		destAddr, err := net.ResolveUDPAddr("udp", destHost+":"+destPort)
		if err != nil {
			log.Printf("Could not resolve destination UDP address '%s': %v", targetURIStr, err)
			upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
			return
		}
		outboundTransport = NewUDPTransport(s.udpConn, destAddr)
	} else {
		destAddr, err := net.ResolveTCPAddr("tcp", destHost+":"+destPort)
		if err != nil {
			log.Printf("Could not resolve destination TCP address '%s': %v", targetURIStr, err)
			upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
			return
		}
		conn, err := net.DialTCP("tcp", nil, destAddr)
		if err != nil {
			log.Printf("Could not connect to destination TCP address '%s': %v", targetURIStr, err)
			upstreamTx.Respond(BuildResponse(503, "Service Unavailable", originalReq, nil))
			return
		}
		outboundTransport = NewTCPTransport(conn)
	}

	switch fwdReq.Method {
	case "INVITE", "UPDATE":
		clientTx, err = NewInviteClientTx(fwdReq, outboundTransport)
	default:
		clientTx, err = NewNonInviteClientTx(fwdReq, outboundTransport)
	}
	if err != nil {
		log.Printf("Failed to create client transaction for routed request: %v", err)
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
		return
	}

	// 6. Forward and stitch responses
	s.txManager.Add(clientTx)
	log.Printf("Proxying routed %s from %s to %s via client tx %s", fwdReq.Method, fwdReq.GetHeader("From"), fwdReq.GetHeader("To"), clientTx.ID())
	s.forwardAndStitch(upstreamTx, clientTx, fwdReq.URI)
}

// prepareForwardedRoutedRequest prepares an in-dialog request for forwarding.
// It removes the top Route header, decrements Max-Forwards, and adds a new Via header.
func (s *SIPServer) prepareForwardedRoutedRequest(req *SIPRequest, proto string) *SIPRequest {
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
	via := fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(proto), s.listenAddr, branch)
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

	// Add Supported: timer to indicate this proxy supports session timers.
	if existing, ok := fwdReq.Headers["Supported"]; ok {
		if !strings.Contains(strings.ToLower(existing), "timer") {
			fwdReq.Headers["Supported"] = "timer, " + existing
		}
	} else {
		fwdReq.Headers["Supported"] = "timer"
	}

	return fwdReq
}

// proxyAckRequest handles the stateless forwarding of an in-dialog ACK request.
func (s *SIPServer) proxyAckRequest(req *SIPRequest, transport Transport) {
	log.Printf("Routing in-dialog ACK request")
	var err error

	// 1. Validate Route header points to this proxy
	routeHeader := req.GetHeader("Route")
	routes := strings.Split(routeHeader, ",")
	topRoute := strings.TrimSpace(routes[0])
	if !strings.Contains(topRoute, s.listenAddr) {
		log.Printf("Received ACK with Route header not for this proxy. Dropping.")
		return
	}

	inboundProto := transport.GetProto()
	// 2. Prepare the forwarded request (strips our route from the Route header)
	fwdReq := s.prepareForwardedRoutedRequest(req, inboundProto)
	if fwdReq == nil {
		log.Printf("Dropping ACK due to Max-Forwards limit.")
		return // Can't send a response to an ACK
	}

	// 3. Determine destination and handle strict routing
	var targetURIStr string
	nextHopRouteHeader := fwdReq.GetHeader("Route")
	if nextHopRouteHeader != "" {
		nextRoutes := strings.Split(nextHopRouteHeader, ",")
		firstHop := strings.TrimSpace(nextRoutes[0])
		targetURIStr = firstHop

		firstHopURI, err := ParseSIPURI(firstHop)
		if err != nil {
			log.Printf("Could not parse next hop Route URI for ACK: %v. Dropping.", err)
			return
		}

		if _, lrExists := firstHopURI.Params["lr"]; !lrExists {
			// Strict router detected. Reformat the request.
			log.Printf("Next hop %s for ACK is a strict router. Reformatting request.", firstHop)
			fwdReq.URI = firstHop
			remainingRoutes := nextRoutes[1:]
			finalRoute := append(remainingRoutes, "<"+req.URI+">")
			fwdReq.Headers["Route"] = strings.Join(finalRoute, ", ")
		}
	} else {
		targetURIStr = fwdReq.URI
	}

	// 4. Resolve destination address
	destURI, err := ParseSIPURI(targetURIStr)
	if err != nil {
		log.Printf("Could not parse destination URI for ACK: %v. Dropping.", err)
		return
	}
	destPort := "5060"
	if destURI.Port != "" {
		destPort = destURI.Port
	}
	destHost := destURI.Host

	// 5. Send it.
	log.Printf("Proxying ACK to %s:%s via %s", destHost, destPort, inboundProto)
	if strings.ToUpper(inboundProto) == "UDP" {
		destAddr, err := net.ResolveUDPAddr("udp", destHost+":"+destPort)
		if err != nil {
			log.Printf("Could not resolve destination for ACK: %v. Dropping.", err)
			return
		}
		if _, err = s.udpConn.WriteTo([]byte(fwdReq.String()), destAddr); err != nil {
			log.Printf("Error sending proxied ACK over UDP: %v", err)
		}
	} else {
		destAddr, err := net.ResolveTCPAddr("tcp", destHost+":"+destPort)
		if err != nil {
			log.Printf("Could not resolve destination for ACK: %v. Dropping.", err)
			return
		}
		conn, err := net.DialTCP("tcp", nil, destAddr)
		if err != nil {
			log.Printf("Could not connect to destination for ACK: %v", err)
			return
		}
		defer conn.Close()
		if _, err = conn.Write([]byte(fwdReq.String())); err != nil {
			log.Printf("Error sending proxied ACK over TCP: %v", err)
		}
	}
}

// proxyInitialRequest is the stateful proxy logic for initial requests (no Route header).
func (s *SIPServer) proxyInitialRequest(originalReq *SIPRequest, upstreamTx ServerTransaction) {
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
	destHost := contactURI.Host
	inboundProto := upstreamTx.Transport().GetProto()

	fwdReq := s.prepareForwardedRequest(originalReq, registration.ContactURI, inboundProto)
	if fwdReq == nil {
		log.Printf("Dropping request for user %s due to Max-Forwards limit", toURI.User)
		upstreamTx.Respond(BuildResponse(483, "Too Many Hops", originalReq, nil))
		return
	}

	var clientTx ClientTransaction
	var outboundTransport Transport
	if strings.ToUpper(inboundProto) == "UDP" {
		destAddr, err := net.ResolveUDPAddr("udp", destHost+":"+destPort)
		if err != nil {
			log.Printf("Could not resolve destination UDP address '%s': %v", registration.ContactURI, err)
			upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
			return
		}
		outboundTransport = NewUDPTransport(s.udpConn, destAddr)
	} else {
		destAddr, err := net.ResolveTCPAddr("tcp", destHost+":"+destPort)
		if err != nil {
			log.Printf("Could not resolve destination TCP address '%s': %v", registration.ContactURI, err)
			upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
			return
		}
		conn, err := net.DialTCP("tcp", nil, destAddr)
		if err != nil {
			log.Printf("Could not connect to destination TCP address '%s': %v", registration.ContactURI, err)
			upstreamTx.Respond(BuildResponse(503, "Service Unavailable", originalReq, nil))
			return
		}
		outboundTransport = NewTCPTransport(conn)
	}

	switch fwdReq.Method {
	case "INVITE", "UPDATE":
		clientTx, err = NewInviteClientTx(fwdReq, outboundTransport)
	default:
		clientTx, err = NewNonInviteClientTx(fwdReq, outboundTransport)
	}

	if err != nil {
		log.Printf("Failed to create client transaction: %v", err)
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", originalReq, nil))
		return
	}
	s.txManager.Add(clientTx)
	log.Printf("Proxying initial %s from %s to %s via client tx %s", originalReq.Method, originalReq.GetHeader("From"), originalReq.GetHeader("To"), clientTx.ID())

	s.forwardAndStitch(upstreamTx, clientTx, registration.ContactURI)
}

// forwardAndStitch handles the forwarding of a request via a client transaction
// and stitches the response back to the upstream server transaction. It also handles
// retries for session timer negotiation (422 responses).
func (s *SIPServer) forwardAndStitch(upstreamTx ServerTransaction, clientTx ClientTransaction, nextHopURI string) {
	s.proxiedTxMutex.Lock()
	s.proxiedTxs[upstreamTx.ID()] = clientTx
	s.proxiedTxMutex.Unlock()

	go func() {
		defer func() {
			s.proxiedTxMutex.Lock()
			delete(s.proxiedTxs, upstreamTx.ID())
			s.proxiedTxMutex.Unlock()
		}()

		isInvite := upstreamTx.OriginalRequest().Method == "INVITE"
		var timerC *time.Timer
		if isInvite {
			// RFC 3261 Section 16.6 Item 11: Set Timer C for INVITE transactions.
			// It must be larger than 3 minutes.
			timerC = time.NewTimer(3*time.Minute + 5*time.Second)
			defer timerC.Stop()
		}
		receivedProvisional := false

		currentClientTx := clientTx
		maxRetries := 5
		maxMinSE := 0

		for i := 0; i < maxRetries; i++ {
			var timerCChan <-chan time.Time
			if timerC != nil {
				timerCChan = timerC.C
			}

			select {
			case <-timerCChan:
				// RFC 3261 Section 16.8: Processing Timer C
				log.Printf("Proxy Timer C fired for INVITE tx %s", upstreamTx.ID())
				if receivedProvisional {
					log.Printf("Timer C: Provisional response was received. Cancelling downstream transaction.")
					if inviteClientTx, ok := currentClientTx.(*InviteClientTx); ok {
						cancelReq := createCancelRequest(inviteClientTx.OriginalRequest())
					log.Printf("Proxying CANCEL for Timer C to %s", inviteClientTx.Transport().GetRemoteAddr())
					if _, err := inviteClientTx.Transport().Write([]byte(cancelReq.String())); err != nil {
							log.Printf("Error proxying CANCEL for Timer C: %v", err)
						}
					}
				} else {
					log.Printf("Timer C: No provisional response received. Responding 408 to upstream.")
					upstreamTx.Respond(BuildResponse(408, "Request Timeout", upstreamTx.OriginalRequest(), nil))
				}
				return // End this goroutine.

			case res := <-currentClientTx.Responses():
				if res == nil {
					continue
				}

				// RFC 3261 Section 16.7 Item 2: Reset timer C on provisional response.
				if isInvite && res.StatusCode > 100 && res.StatusCode < 200 {
					log.Printf("Received provisional response for INVITE tx %s, resetting Timer C", upstreamTx.ID())
					receivedProvisional = true
					timerC.Reset(3*time.Minute + 5*time.Second)
				}

				if res.StatusCode == 422 && currentClientTx.OriginalRequest().Method == "INVITE" {
					log.Printf("Proxy received 422 for tx %s. Retrying...", currentClientTx.ID())
					currentClientTx.Terminate()

					minSE, err := res.MinSE()
					if err != nil || minSE <= 0 {
						log.Printf("422 response has invalid or missing Min-SE header. Aborting.")
						upstreamTx.Respond(BuildResponse(500, "Server Internal Error", upstreamTx.OriginalRequest(), nil))
						return
					}
					if minSE > maxMinSE {
						maxMinSE = minSE
					}

					cseqStr := currentClientTx.OriginalRequest().GetHeader("CSeq")
					cseq, method, ok := parseCSeq(cseqStr)
					if !ok {
						log.Printf("Could not parse CSeq from forwarded request. Aborting.")
						upstreamTx.Respond(BuildResponse(500, "Server Internal Error", upstreamTx.OriginalRequest(), nil))
						return
					}

					inboundProto := upstreamTx.Transport().GetProto()
					newFwdReq := s.prepareForwardedRequest(upstreamTx.OriginalRequest(), nextHopURI, inboundProto)
					newFwdReq.Headers["CSeq"] = fmt.Sprintf("%d %s", cseq+1, method)
					newFwdReq.Headers["Min-SE"] = strconv.Itoa(maxMinSE)
					newFwdReq.Headers["Session-Expires"] = strconv.Itoa(maxMinSE)

					// Reuse the transport from the previous attempt.
					outboundTransport := currentClientTx.Transport()
					newClientTx, err := NewInviteClientTx(newFwdReq, outboundTransport)
					if err != nil {
						log.Printf("Failed to create client transaction for retry: %v", err)
						upstreamTx.Respond(BuildResponse(500, "Server Internal Error", upstreamTx.OriginalRequest(), nil))
						return
					}
					s.txManager.Add(newClientTx)
					s.proxiedTxMutex.Lock()
					s.proxiedTxs[upstreamTx.ID()] = newClientTx
					s.proxiedTxMutex.Unlock()

					currentClientTx = newClientTx
					continue
				}

				log.Printf("Proxy received response for tx %s: %d %s", currentClientTx.ID(), res.StatusCode, res.Reason)

				topVia, err := res.TopVia()
				if err != nil {
					log.Printf("Could not parse Via header from response: %v. Dropping response.", err)
					continue
				}

				if res.StatusCode >= 200 && res.StatusCode < 300 && (upstreamTx.OriginalRequest().Method == "INVITE" || upstreamTx.OriginalRequest().Method == "UPDATE") {
					uacSupportsTimer := strings.Contains(strings.ToLower(upstreamTx.OriginalRequest().GetHeader("Supported")), "timer")
					respSE, _ := res.SessionExpires()

					if respSE != nil {
						s.createOrUpdateSession(upstreamTx, res, respSE)
					} else {
						if uacSupportsTimer {
							reqSE, _ := upstreamTx.OriginalRequest().SessionExpires()
							if reqSE != nil {
								log.Printf("UAS doesn't support session timer, but UAC does. Inserting Session-Expires header into 2xx response.")
								res.Headers["Session-Expires"] = fmt.Sprintf("%d;refresher=uac", reqSE.Delta)
								if existing, ok := res.Headers["Require"]; ok {
									if !strings.Contains(strings.ToLower(existing), "timer") {
										res.Headers["Require"] = "timer, " + existing
									}
								} else {
									res.Headers["Require"] = "timer"
								}
								finalSE, _ := res.SessionExpires()
								s.createOrUpdateSession(upstreamTx, res, finalSE)
							}
						}
					}
				}

				if topVia.Branch() == currentClientTx.ID() {
					res.PopVia()
				} else {
					log.Printf("WARN: Top Via branch '%s' in response did not match client tx branch '%s'. Not stripping Via.", topVia.Branch(), currentClientTx.ID())
				}
				upstreamTx.Respond(res)
				if res.StatusCode >= 200 {
					return // This will trigger the deferred timerC.Stop()
				}
			case <-currentClientTx.Done():
				log.Printf("Client transaction %s terminated.", currentClientTx.ID())
				return
			case <-upstreamTx.Done():
				log.Printf("Upstream transaction %s terminated. Terminating downstream.", upstreamTx.ID())
				currentClientTx.Terminate()
				return
			}
		}
		log.Printf("Exceeded max retries for session timer negotiation for tx %s", upstreamTx.ID())
		upstreamTx.Respond(BuildResponse(500, "Server Internal Error", upstreamTx.OriginalRequest(), map[string]string{"Warning": "Session timer negotiation failed"}))
	}()
}

// parseCSeq is a helper to extract number and method from CSeq header.
func parseCSeq(cseqHeader string) (int, string, bool) {
	parts := strings.SplitN(strings.TrimSpace(cseqHeader), " ", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	cseq, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", false
	}
	return cseq, parts[1], true
}

// prepareForwardedRequest creates a copy of a request suitable for forwarding.
func (s *SIPServer) prepareForwardedRequest(req *SIPRequest, targetURI string, proto string) *SIPRequest {
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
	via := fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(proto), s.listenAddr, branch)
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

	// Add Supported: timer to indicate this proxy supports session timers.
	if existing, ok := fwdReq.Headers["Supported"]; ok {
		if !strings.Contains(strings.ToLower(existing), "timer") {
			fwdReq.Headers["Supported"] = "timer, " + existing
		}
	} else {
		fwdReq.Headers["Supported"] = "timer"
	}

	return fwdReq
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

func (s *SIPServer) handleCancel(transport Transport, req *SIPRequest) {
	log.Printf("Handling CANCEL request for Call-ID %s", req.GetHeader("Call-ID"))

	topVia, err := req.TopVia()
	if err != nil {
		log.Printf("Could not get Via from CANCEL request: %v", err)
		return
	}
	branchID := topVia.Branch()

	tx, ok := s.txManager.Get(branchID)
	if !ok {
		// If the original server transaction is not found, it likely has already been
		// completed and terminated. Per RFC 3261 Section 16.8, the proxy must
		// still respond 200 OK to the CANCEL.
		log.Printf("Could not find transaction %s to cancel. Responding 200 OK statelessly.", branchID)
		res200 := BuildResponse(200, "OK", req, nil)
		if _, err := transport.Write([]byte(res200.String())); err != nil {
			log.Printf("Error sending 200 OK for CANCEL: %v", err)
		}
		return
	}

	srvTx, isServerTx := tx.(ServerTransaction)
	if !isServerTx {
		log.Printf("Transaction %s found, but it is not a server transaction. Cannot process CANCEL.", branchID)
		// Still respond 200 OK to the CANCEL as we've consumed it.
		res200 := BuildResponse(200, "OK", req, nil)
		if _, err := transport.Write([]byte(res200.String())); err != nil {
			log.Printf("Error sending 200 OK for CANCEL: %v", err)
		}
		return
	}

	// Immediately respond 200 OK to the CANCEL request.
	res200 := BuildResponse(200, "OK", req, nil)
	log.Printf("Sending 200 OK for CANCEL to %s", transport.GetRemoteAddr())
	if _, err := transport.Write([]byte(res200.String())); err != nil {
		log.Printf("Error sending 200 OK for CANCEL: %v", err)
	}

	// Check if this INVITE was proxied and cancel the downstream leg.
	s.proxiedTxMutex.RLock()
	clientTx, foundProxy := s.proxiedTxs[branchID]
	s.proxiedTxMutex.RUnlock()

	if foundProxy {
		log.Printf("Found downstream client transaction %s to cancel", clientTx.ID())
		cancelReq := createCancelRequest(clientTx.OriginalRequest())
		// The destination for the CANCEL is the same as the INVITE's client transaction
		if inviteClientTx, ok := clientTx.(*InviteClientTx); ok {
			log.Printf("Proxying CANCEL to %s", inviteClientTx.Transport().GetRemoteAddr())
			if _, err := inviteClientTx.Transport().Write([]byte(cancelReq.String())); err != nil {
				log.Printf("Error proxying CANCEL request: %v", err)
			}
		}
	}

	// Respond 487 to the original INVITE.
	res487 := BuildResponse(487, "Request Terminated", srvTx.OriginalRequest(), nil)
	if err := srvTx.Respond(res487); err != nil {
		log.Printf("Error sending 487 response to original INVITE tx %s: %v", branchID, err)
	}

	// The transaction will terminate itself upon sending the 487 response.
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
