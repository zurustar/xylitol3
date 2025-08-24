package sip

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// B2BUA manages a single call, acting as a back-to-back user agent.
// It controls two call legs: the A-leg (UAC to B2BUA) and the B-leg (B2BUA to UAS).
type B2BUA struct {
	server       *SIPServer
	aLegTx       ServerTransaction // A-leg: from caller
	bLegTx       ClientTransaction // B-leg: to callee
	aLegDialogID string
	bLegDialogID string
	aLegAck      *SIPRequest
	bLegAck      *SIPRequest
	done         chan bool
	mu           sync.RWMutex
	cancel       func()
	StartTime    time.Time
}

// NewB2BUA creates and initializes a new B2BUA instance.
func NewB2BUA(s *SIPServer, aLegTx ServerTransaction) *B2BUA {
	return &B2BUA{
		server:    s,
		aLegTx:    aLegTx,
		done:      make(chan bool),
		StartTime: time.Now(),
	}
}

// Run starts the B2BUA logic. It's responsible for creating the B-leg
// and then managing the call state between the two legs.
func (b *B2BUA) Run(targetURI string) {
	log.Printf("B2BUA started for A-leg tx %s, targeting %s", b.aLegTx.ID(), targetURI)
	defer close(b.done)

	aLegReq := b.aLegTx.OriginalRequest()

	// Session Timer Logic - Section 8.1 of RFC 4028
	if aLegReq.Method == "INVITE" || aLegReq.Method == "UPDATE" {
		const b2buaMinSE = 1800 // 30 minutes, as recommended.

		se, err := aLegReq.SessionExpires()
		if err != nil {
			b.aLegTx.Respond(BuildResponse(400, "Bad Request", aLegReq, map[string]string{"Warning": "Malformed Session-Expires header"}))
			return
		}

		if se != nil { // A session timer has been requested.
			if se.Delta < b2buaMinSE {
				// UAC is timer-aware, so we can reject.
				log.Printf("Session-Expires %d is too small. Rejecting with 422.", se.Delta)
				extraHeaders := map[string]string{"Min-SE": strconv.Itoa(b2buaMinSE)}
				b.aLegTx.Respond(BuildResponse(422, "Session Interval Too Small", aLegReq, extraHeaders))
				return
			}
		}
	}


	// 1. Create a new INVITE request for the B-leg.
	bLegReq := b.createBLegInvite(aLegReq, targetURI)
	if bLegReq == nil {
		b.aLegTx.Respond(BuildResponse(500, "Server Internal Error", aLegReq, nil))
		return
	}

	// 2. Determine transport for the B-leg.
	contactURI, err := ParseSIPURI(targetURI)
	if err != nil {
		log.Printf("B2BUA: Invalid target URI %s: %v", targetURI, err)
		b.aLegTx.Respond(BuildResponse(400, "Bad Request", aLegReq, map[string]string{"Warning": "Invalid target URI"}))
		return
	}
	destPort := "5060"
	if contactURI.Port != "" {
		destPort = contactURI.Port
	}
	destHost := contactURI.Host
	proto := strings.ToUpper(b.aLegTx.Transport().GetProto())

	var outboundTransport Transport
	if proto == "UDP" {
		destAddr, err := net.ResolveUDPAddr("udp", destHost+":"+destPort)
		if err != nil {
			log.Printf("B2BUA: Could not resolve destination UDP address '%s': %v", targetURI, err)
			b.aLegTx.Respond(BuildResponse(503, "Service Unavailable", aLegReq, nil))
			return
		}
		outboundTransport = NewUDPTransport(b.server.udpConn, destAddr)
	} else {
		destAddr, err := net.ResolveTCPAddr("tcp", destHost+":"+destPort)
		if err != nil {
			log.Printf("B2BUA: Could not resolve destination TCP address '%s': %v", targetURI, err)
			b.aLegTx.Respond(BuildResponse(503, "Service Unavailable", aLegReq, nil))
			return
		}
		conn, err := net.DialTCP("tcp", nil, destAddr)
		if err != nil {
			log.Printf("B2BUA: Could not connect to destination TCP address '%s': %v", targetURI, err)
			b.aLegTx.Respond(BuildResponse(503, "Service Unavailable", aLegReq, nil))
			return
		}
		outboundTransport = NewTCPTransport(conn)
	}

	// 3. Create and run the B-leg client transaction.
	bLegTx, err := NewInviteClientTx(bLegReq, outboundTransport)
	if err != nil {
		log.Printf("B2BUA: Failed to create B-leg client transaction: %v", err)
		b.aLegTx.Respond(BuildResponse(500, "Server Internal Error", aLegReq, nil))
		return
	}
	b.mu.Lock()
	b.bLegTx = bLegTx
	b.mu.Unlock()
	b.server.txManager.Add(bLegTx)

	log.Printf("B2BUA: A-leg tx %s created B-leg tx %s", b.aLegTx.ID(), b.bLegTx.ID())

	// 4. Mediate between the two legs.
	b.mediate(bLegTx, b.aLegTx)
}

// createBLegInvite creates a new INVITE request for the B-leg based on the A-leg request.
func (b *B2BUA) createBLegInvite(aLegReq *SIPRequest, targetURI string) *SIPRequest {
	bLegReq := &SIPRequest{
		Method:  "INVITE",
		URI:     targetURI,
		Proto:   aLegReq.Proto,
		Headers: make(map[string]string),
		Body:    aLegReq.Body, // Pass SDP body through
	}

	// Copy most headers, but create new dialog-forming headers.
	for k, v := range aLegReq.Headers {
		lowerK := strings.ToLower(k)
		// Exclude dialog-specific headers that the B2BUA will create itself.
		if lowerK != "from" && lowerK != "to" && lowerK != "call-id" && lowerK != "cseq" &&
			lowerK != "via" && lowerK != "contact" && lowerK != "record-route" && lowerK != "route" {
			bLegReq.Headers[k] = v
		}
	}

	// Create new dialog identifiers.
	bLegReq.Headers["Call-ID"] = GenerateCallID()

	// Reconstruct From header with a new tag, preserving the display name.
	fromHeader := aLegReq.GetHeader("From")
	// A simple but more robust way to strip the tag is to split by ";tag=".
	fromBase := strings.Split(fromHeader, ";tag=")[0]
	fromTag := GenerateTag()
	bLegReq.Headers["From"] = fmt.Sprintf("%s;tag=%s", fromBase, fromTag)

	// To header should be based on the target
	toURI, err := ParseSIPURI(targetURI)
	if err != nil {
		return nil
	}
	bLegReq.Headers["To"] = fmt.Sprintf("<sip:%s@%s>", toURI.User, toURI.Host)

	bLegReq.Headers["CSeq"] = "1 INVITE" // CSeq is independent for the new dialog

	// B2BUA's Contact header.
	bLegReq.Headers["Contact"] = fmt.Sprintf("<sip:%s>", b.server.listenAddr)

	// The transaction layer requires a Via header to be present to determine the branch ID.
	branch := GenerateBranchID()
	via := fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(b.aLegTx.Transport().GetProto()), b.server.listenAddr, branch)
	bLegReq.Headers["Via"] = via

	bLegReq.Headers["Max-Forwards"] = "69" // Decrement Max-Forwards

	return bLegReq
}

// mediate handles the response and request flow between the two legs.
func (b *B2BUA) mediate(bLegTx ClientTransaction, aLegTx ServerTransaction) {
	aLegReq := aLegTx.OriginalRequest()

	for {
		select {
		// Handle responses from the B-leg (callee)
		case res, ok := <-bLegTx.Responses():
			if !ok {
				return
			}
			log.Printf("B2BUA: Received response %d from B-leg tx %s", res.StatusCode, bLegTx.ID())

			// Handle 422 Session Interval Too Small by retrying
			if res.StatusCode == 422 {
				minSE, err := res.MinSE()
				if err == nil && minSE > 0 {
					log.Printf("B2BUA: Handling 422, retrying with Min-SE: %d", minSE)
					// Create a new request with the updated Session-Expires
					retryReq := bLegTx.OriginalRequest().Clone()
					retryReq.Headers["Session-Expires"] = strconv.Itoa(minSE)

					// Increment CSeq
					cseqStr := retryReq.GetHeader("CSeq")
					if parts := strings.Fields(cseqStr); len(parts) == 2 {
						if cseq, err := strconv.Atoi(parts[0]); err == nil {
							retryReq.Headers["CSeq"] = fmt.Sprintf("%d %s", cseq+1, parts[1])
						}
					}

					// Add a new Via header for the new transaction
					branch := GenerateBranchID()
					via := fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(b.aLegTx.Transport().GetProto()), b.server.listenAddr, branch)
					retryReq.Headers["Via"] = via

					// Create and run a new B-leg client transaction for the retry
					newBLegTx, err := NewInviteClientTx(retryReq, bLegTx.Transport())
					if err != nil {
						log.Printf("B2BUA: Failed to create retry B-leg client transaction: %v", err)
						aLegTx.Respond(BuildResponse(500, "Server Internal Error", aLegReq, nil))
						return
					}
					b.mu.Lock()
					b.bLegTx = newBLegTx
					b.mu.Unlock()
					b.server.txManager.Add(newBLegTx)

					// Continue mediating with the new transaction
					b.mediate(newBLegTx, aLegTx)
					return // End this mediation loop
				}
			}


			// If we got a 200 OK, we might need to add Session-Expires header ourselves
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				// Session Timer: UAS Does Not Support Timers (RFC 4028 Section 8.1.2)
				// If the original request wanted a timer, but the 2xx response doesn't have one,
				// the B2BUA MUST insert it.
				if se, _ := aLegReq.SessionExpires(); se != nil { // Timer was requested
					if resSE, _ := res.SessionExpires(); resSE == nil { // Timer not in 2xx response
						log.Printf("B2BUA: UAS did not include Session-Expires in 2xx. Adding it now for A-leg.")
						res.Headers["Session-Expires"] = fmt.Sprintf("%d;refresher=uac", se.Delta)
						if existing, ok := res.Headers["Require"]; ok {
							res.Headers["Require"] = "timer, " + existing
						} else {
							res.Headers["Require"] = "timer"
						}
					}
				}
			}

			// Create a corresponding response for the A-leg.
			aLegRes := b.createALegResponse(aLegReq, res)
			aLegTx.Respond(aLegRes)

			// If it's a final response, the INVITE transaction is over.
			if res.StatusCode >= 200 {
				if res.StatusCode < 300 {
					// Call was successful, set up dialog state.
					b.establishDialogs(aLegRes, res)

					// Activate the session timer if it was negotiated.
					if se, err := aLegRes.SessionExpires(); err == nil && se != nil {
						b.server.createOrUpdateSession(aLegTx, aLegRes, se)
					}

					// Wait for in-dialog messages
					b.waitForDialogMessages()
				}
				return // End mediation for INVITE transaction
			}

		// Handle cancellation from the A-leg (caller)
		case <-aLegTx.Done():
			log.Printf("B2BUA: A-leg tx %s terminated.", aLegTx.ID())
			// If B-leg is still active, cancel it.
			select {
			case <-bLegTx.Done():
				// B-leg already done, nothing to do.
			default:
				log.Printf("B2BUA: A-leg terminated, cancelling B-leg tx %s", bLegTx.ID())
				cancelReq := createCancelRequest(bLegTx.OriginalRequest())
				bLegTx.Transport().Write([]byte(cancelReq.String()))
				bLegTx.Terminate()
			}
			return

		case <-time.After(32 * time.Second): // Fail-safe timer
			log.Printf("B2BUA: Timeout waiting for response on B-leg tx %s", bLegTx.ID())
			aLegTx.Respond(BuildResponse(408, "Request Timeout", aLegReq, nil))
			bLegTx.Terminate()
			return
		}
	}
}

// createALegResponse creates a response for the A-leg based on the B-leg's response.
func (b *B2BUA) createALegResponse(aLegReq *SIPRequest, bLegRes *SIPResponse) *SIPResponse {
	extraHeaders := make(map[string]string)
	// Copy all headers from B-leg response that are not dialog-specific.
	for k, v := range bLegRes.Headers {
		lowerK := strings.ToLower(k)
		switch lowerK {
		case "via", "from", "to", "call-id", "cseq", "contact", "record-route":
			// These are managed by the B2BUA or BuildResponse.
		default:
			extraHeaders[k] = v
		}
	}

	// Re-use the status code and reason from the B-leg response.
	aLegRes := BuildResponse(bLegRes.StatusCode, bLegRes.Reason, aLegReq, extraHeaders)

	// If the B-leg response was 2xx, we need to add our own Contact header and a To tag.
	if aLegRes.StatusCode >= 200 && aLegRes.StatusCode < 300 {
		aLegRes.Headers["Contact"] = fmt.Sprintf("<sip:%s>", b.server.listenAddr)
		if getTag(aLegRes.Headers["To"]) == "" {
			aLegRes.Headers["To"] = fmt.Sprintf("%s;tag=%s", aLegRes.Headers["To"], GenerateTag())
		}
	}

	// The Via, From, To, Call-ID, and CSeq headers are already correctly set by BuildResponse.
	return aLegRes
}

// establishDialogs stores the dialog identifiers after a 2xx response.
func (b *B2BUA) establishDialogs(aLegRes, bLegRes *SIPResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.aLegDialogID = getDialogID(
		aLegRes.GetHeader("Call-ID"),
		getTag(aLegRes.GetHeader("From")),
		getTag(aLegRes.GetHeader("To")),
	)
	b.bLegDialogID = getDialogID(
		bLegRes.GetHeader("Call-ID"),
		getTag(bLegRes.GetHeader("From")),
		getTag(bLegRes.GetHeader("To")),
	)

	b.server.dialogs.Store(b.aLegDialogID, b)
	b.server.dialogs.Store(b.bLegDialogID, b)

	log.Printf("B2BUA: Established dialogs. A-leg: %s, B-leg: %s", b.aLegDialogID, b.bLegDialogID)
}

// waitForDialogMessages is the main loop for handling in-dialog requests like BYE or re-INVITE.
func (b *B2BUA) waitForDialogMessages() {
	// This part of the logic will be implemented to handle BYE, re-INVITEs, etc.
	// For now, it just waits until the B2BUA is cancelled.
	<-b.done
}

// Cancel is called when the upstream transaction is cancelled.
func (b *B2BUA) Cancel() {
	log.Printf("B2BUA: Received external cancel for A-leg tx %s", b.aLegTx.ID())

	b.mu.RLock()
	bLegTx := b.bLegTx
	b.mu.RUnlock()

	// If the B-leg INVITE transaction is still in flight, cancel it.
	if bLegTx != nil {
		select {
		case <-bLegTx.Done():
			// Transaction already finished, nothing to cancel.
		default:
			log.Printf("B2BUA: Sending CANCEL to B-leg tx %s", bLegTx.ID())
			cancelReq := createCancelRequest(bLegTx.OriginalRequest())
			if _, err := bLegTx.Transport().Write([]byte(cancelReq.String())); err != nil {
				log.Printf("B2BUA: Error sending CANCEL to B-leg: %v", err)
			}
			bLegTx.Terminate()
		}
	}

	// Respond 487 to the original INVITE.
	if err := b.aLegTx.Respond(BuildResponse(487, "Request Terminated", b.aLegTx.OriginalRequest(), nil)); err != nil {
		log.Printf("B2BUA: Error sending 487 response to A-leg: %v", err)
	}

	b.cleanup()
}

// HandleInDialogRequest processes a request that belongs to an existing dialog managed by this B2BUA.
func (b *B2BUA) HandleInDialogRequest(req *SIPRequest, tx ServerTransaction) {
	log.Printf("B2BUA: Handling in-dialog %s for dialog %s", req.Method, b.aLegDialogID)

	b.mu.Lock()
	aLegDialogID := b.aLegDialogID
	b.mu.Unlock()

	// Determine if the request came from the A-leg or B-leg.
	// This is a simplified check. A robust implementation would check both dialog IDs.
	reqDialogID := getDialogID(req.GetHeader("Call-ID"), getTag(req.GetHeader("From")), getTag(req.GetHeader("To")))

	if reqDialogID == aLegDialogID {
		b.handleALegRequest(req, tx)
	} else {
		b.handleBLegRequest(req, tx)
	}
}

// handleALegRequest handles in-dialog requests from the caller (A-leg).
func (b *B2BUA) handleALegRequest(req *SIPRequest, tx ServerTransaction) {
	switch req.Method {
	case "ACK":
		// ACK for the 200 OK is received on the A-leg. We need to generate a new ACK for the B-leg.
		log.Printf("B2BUA: Received ACK for A-leg dialog %s", b.aLegDialogID)
		b.mu.Lock()
		b.aLegAck = req
		b.mu.Unlock()

		// Create and send ACK for the B-leg
		if b.bLegTx != nil && b.bLegTx.LastResponse() != nil {
			bLegAck := b.createBLegAck(b.bLegTx.LastResponse())
			if bLegAck != nil {
				b.bLegTx.Transport().Write([]byte(bLegAck.String()))
				log.Printf("B2BUA: Sent ACK for B-leg dialog %s", b.bLegDialogID)
			}
		}

	case "BYE":
		// BYE from A-leg. Respond 200 OK to A-leg, then send BYE to B-leg.
		log.Printf("B2BUA: Received BYE for A-leg dialog %s", b.aLegDialogID)
		tx.Respond(BuildResponse(200, "OK", req, nil))

		// Create and send BYE for the B-leg
		bLegBye := b.createForwardedRequest(req, b.bLegDialogID)
		if bLegBye != nil {
			b.sendRequestOnBLeg(bLegBye)
		}
		b.cleanup()
	}
}

// handleBLegRequest handles in-dialog requests from the callee (B-leg).
// Note: These will arrive as new ServerTransactions on the server.
func (b *B2BUA) handleBLegRequest(req *SIPRequest, tx ServerTransaction) {
	switch req.Method {
	case "BYE":
		// BYE from B-leg. Respond 200 OK to B-leg, then send BYE to A-leg.
		log.Printf("B2BUA: Received BYE for B-leg dialog %s", b.bLegDialogID)
		tx.Respond(BuildResponse(200, "OK", req, nil))

		// Create and send BYE for the A-leg
		aLegBye := b.createForwardedRequest(req, b.aLegDialogID)
		if aLegBye != nil {
			// This requires sending a request back to the original caller.
			// This logic needs a way to create a client transaction towards the A-leg UA.
			// This part is complex and will be simplified for now.
			log.Printf("B2BUA: Need to send BYE to A-leg, but client tx to UAC is not implemented yet.")
		}
		b.cleanup()
	}
}

// createBLegAck creates the ACK for the B-leg based on the B-leg's 200 OK response.
func (b *B2BUA) createBLegAck(bLegRes *SIPResponse) *SIPRequest {
	if bLegRes.StatusCode < 200 || bLegRes.StatusCode >= 300 {
		return nil
	}

	bLegToTag := getTag(bLegRes.GetHeader("To"))
	bLegFromTag := getTag(bLegRes.GetHeader("From"))
	bLegCallID := bLegRes.GetHeader("Call-ID")
	bLegCSeq := bLegRes.GetHeader("CSeq")
	bLegContact := bLegRes.GetHeader("Contact")

	contactURI, err := ParseSIPURI(bLegContact)
	if err != nil {
		log.Printf("B2BUA: Could not parse Contact URI from B-leg 200 OK: %v", err)
		return nil
	}

	ack := &SIPRequest{
		Method: "ACK",
		URI:    contactURI.String(),
		Proto:  "SIP/2.0",
		Headers: map[string]string{
			"Via":        fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(b.bLegTx.Transport().GetProto()), b.server.listenAddr, GenerateBranchID()),
			"From":       fmt.Sprintf("%s;tag=%s", b.bLegTx.OriginalRequest().GetHeader("From"), bLegFromTag),
			"To":         fmt.Sprintf("%s;tag=%s", b.bLegTx.OriginalRequest().GetHeader("To"), bLegToTag),
			"Call-ID":    bLegCallID,
			"CSeq":       strings.Replace(bLegCSeq, "INVITE", "ACK", 1),
			"Max-Forwards": "70",
		},
		Body: []byte{},
	}
	return ack
}

// createForwardedRequest creates a new request for the opposite leg,
// copying essential information but generating a new dialog context.
func (b *B2BUA) createForwardedRequest(origReq *SIPRequest, targetDialogID string) *SIPRequest {
	// This is a simplified placeholder. A real implementation needs to carefully
	// manage headers, CSeq, and routing information.
	newReq := origReq.Clone()
	newReq.Headers["Call-ID"] = strings.Split(targetDialogID, ":")[0]
	// ... and so on for From/To tags, CSeq etc.
	return newReq
}

// sendRequestOnBLeg sends a new request on the B-leg.
func (b *B2BUA) sendRequestOnBLeg(req *SIPRequest) {
	// This would require creating a new client transaction for the B-leg.
	log.Printf("B2BUA: Sending %s to B-leg", req.Method)
	b.bLegTx.Transport().Write([]byte(req.String()))
}

// cleanup removes the B2BUA from the server's dialog and transaction maps.
func (b *B2BUA) cleanup() {
	b.mu.Lock()
	defer b.mu.Unlock()

	log.Printf("B2BUA: Cleaning up dialogs %s and %s", b.aLegDialogID, b.bLegDialogID)

	// Remove from dialog maps
	if b.aLegDialogID != "" {
		b.server.dialogs.Delete(b.aLegDialogID)
	}
	if b.bLegDialogID != "" {
		b.server.dialogs.Delete(b.bLegDialogID)
	}

	// Remove from transaction map
	if b.aLegTx != nil {
		b.server.b2buaMutex.Lock()
		delete(b.server.b2buaByTx, b.aLegTx.ID())
		b.server.b2buaMutex.Unlock()
	}


	select {
	case <-b.done:
		// Already closed
	default:
		close(b.done)
	}
}
