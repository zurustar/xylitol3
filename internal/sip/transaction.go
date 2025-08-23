package sip

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

var (
	// RFC3261BranchMagicCookie is the magic cookie prefix for branch IDs.
	RFC3261BranchMagicCookie = "z9hG4bK"
	// T1 is the RTT estimate, 500ms.
	T1 = 500 * time.Millisecond
	// T2 is the max retransmit interval for non-INVITE, 4s.
	T2 = 4 * time.Second
	// T4 is the max message lifetime, 5s.
	T4 = 5 * time.Second
)

// FSM States for Transactions
type TxState int

const (
	TxStateTrying TxState = iota
	TxStateProceeding
	TxStateCompleted
	TxStateConfirmed
	TxStateTerminated
	TxStateCalling // Client specific
)

// GenerateBranchID generates a new RFC3261 compliant branch ID.
func GenerateBranchID() string {
	b := make([]byte, 8) // 16 hex characters
	rand.Read(b)
	return fmt.Sprintf("%s%s", RFC3261BranchMagicCookie, hex.EncodeToString(b))
}

// GenerateNonce generates a random hex string of length 2*n.
func GenerateNonce(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// --- Transaction Interfaces ---

// BaseTransaction is the base interface for all transactions.
type BaseTransaction interface {
	ID() string
	Done() <-chan bool
	Terminate()
}

// ServerTransaction represents a server-side transaction.
type ServerTransaction interface {
	BaseTransaction
	Receive(*SIPRequest)
	Respond(*SIPResponse) error
	Requests() <-chan *SIPRequest
}

// ClientTransaction represents a client-side transaction.
type ClientTransaction interface {
	BaseTransaction
	Responses() <-chan *SIPResponse
	ReceiveResponse(*SIPResponse)
}

// --- Transaction Manager ---

// TransactionManager manages all active SIP transactions.
type TransactionManager struct {
	transactions map[string]BaseTransaction
	mu           sync.RWMutex
}

// NewTransactionManager creates a new TransactionManager.
func NewTransactionManager() *TransactionManager {
	return &TransactionManager{
		transactions: make(map[string]BaseTransaction),
	}
}

// Get finds a transaction by its ID (branch ID).
func (tm *TransactionManager) Get(branchID string) (BaseTransaction, bool) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	tx, ok := tm.transactions[branchID]
	return tx, ok
}

// Add adds a new transaction to the manager.
func (tm *TransactionManager) Add(tx BaseTransaction) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.transactions[tx.ID()] = tx

	// Clean up the transaction from the map once it's terminated.
	go func() {
		<-tx.Done()
		tm.Remove(tx.ID())
	}()
}

// Remove removes a transaction from the manager.
func (tm *TransactionManager) Remove(branchID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	delete(tm.transactions, branchID)
}

// --- Server Transactions ---

// NonInviteServerTx implements the server-side non-INVITE transaction state machine.
type NonInviteServerTx struct {
	id           string
	originalReq  *SIPRequest
	lastResponse *SIPResponse
	state        TxState
	mu           sync.RWMutex
	timerJ       *time.Timer
	done         chan bool
	responses    chan *SIPResponse
	requests     chan *SIPRequest
	transport    net.PacketConn
	destAddr     net.Addr
	proto        string
}

// NewNonInviteServerTx creates and starts a new non-INVITE server transaction.
func NewNonInviteServerTx(req *SIPRequest, transport net.PacketConn, remoteAddr net.Addr, proto string) (ServerTransaction, error) {
	topVia, err := req.TopVia()
	if err != nil {
		return nil, err
	}
	branch := topVia.Branch()
	if branch == "" {
		return nil, fmt.Errorf("request is missing branch parameter")
	}

	tx := &NonInviteServerTx{
		id:          branch,
		originalReq: req,
		state:       TxStateTrying,
		done:        make(chan bool),
		responses:   make(chan *SIPResponse, 1),
		requests:    make(chan *SIPRequest, 1),
		transport:   transport,
		destAddr:    remoteAddr,
		proto:       proto,
	}

	go tx.run()
	tx.requests <- req // Pass the initial request to the TU

	return tx, nil
}

func (tx *NonInviteServerTx) ID() string {
	return tx.id
}

func (tx *NonInviteServerTx) Done() <-chan bool {
	return tx.done
}

func (tx *NonInviteServerTx) Terminate() {
	tx.mu.Lock()
	if tx.state == TxStateTerminated {
		tx.mu.Unlock()
		return
	}
	log.Printf("Terminating non-INVITE server transaction %s", tx.id)
	tx.state = TxStateTerminated
	if tx.timerJ != nil {
		tx.timerJ.Stop()
	}
	tx.mu.Unlock()
	close(tx.done)
}

func (tx *NonInviteServerTx) Receive(req *SIPRequest) {
	tx.mu.RLock()
	defer tx.mu.RUnlock()
	switch tx.state {
	case TxStateTrying:
	case TxStateProceeding, TxStateCompleted:
		if tx.lastResponse != nil {
			log.Printf("Retransmitting last response for transaction %s", tx.id)
			tx.send(tx.lastResponse)
		}
	}
}

func (tx *NonInviteServerTx) Respond(res *SIPResponse) error {
	select {
	case tx.responses <- res:
		return nil
	case <-tx.done:
		return fmt.Errorf("transaction terminated")
	}
}

func (tx *NonInviteServerTx) Requests() <-chan *SIPRequest {
	return tx.requests
}

func (tx *NonInviteServerTx) run() {
	defer tx.Terminate()
	for {
		select {
		case res := <-tx.responses:
			tx.mu.Lock()
			if tx.state == TxStateTerminated {
				tx.mu.Unlock()
				return
			}
			if tx.state == TxStateCompleted && tx.lastResponse.StatusCode >= 200 {
				log.Printf("Ignoring new final response for completed transaction %s", tx.id)
				tx.mu.Unlock()
				continue
			}
			tx.lastResponse = res
			tx.send(res)
			if res.StatusCode >= 200 {
				tx.state = TxStateCompleted
				// For reliable transports, terminate immediately. For unreliable, start Timer J.
				if strings.ToUpper(tx.proto) == "TCP" {
					tx.mu.Unlock() // Unlock before calling Terminate
					tx.Terminate()
					return // End the goroutine
				}
				tx.timerJ = time.AfterFunc(64*T1, tx.Terminate)
			} else {
				tx.state = TxStateProceeding
			}
			tx.mu.Unlock()
		case <-tx.done:
			return
		}
	}
}

func (tx *NonInviteServerTx) send(res *SIPResponse) {
	log.Printf("TX %s: Sending response:\n%s", tx.id, res.String())
	_, err := tx.transport.WriteTo([]byte(res.String()), tx.destAddr)
	if err != nil {
		log.Printf("TX %s: transport error sending response: %v", tx.id, err)
		tx.Terminate()
	}
}

// InviteServerTx implements the server-side INVITE transaction state machine.
type InviteServerTx struct {
	id           string
	originalReq  *SIPRequest
	lastResponse *SIPResponse
	state        TxState
	mu           sync.RWMutex
	timerG       *time.Timer
	timerH       *time.Timer
	timerI       *time.Timer
	done         chan bool
	responses    chan *SIPResponse
	requests     chan *SIPRequest
	transport    net.PacketConn
	destAddr     net.Addr
	proto        string
}

// NewInviteServerTx creates and starts a new INVITE server transaction.
func NewInviteServerTx(req *SIPRequest, transport net.PacketConn, remoteAddr net.Addr, proto string) (ServerTransaction, error) {
	topVia, err := req.TopVia()
	if err != nil {
		return nil, err
	}
	branch := topVia.Branch()
	if branch == "" {
		return nil, fmt.Errorf("request is missing branch parameter")
	}
	tx := &InviteServerTx{
		id:          branch,
		originalReq: req,
		state:       TxStateProceeding,
		done:        make(chan bool),
		responses:   make(chan *SIPResponse, 1),
		requests:    make(chan *SIPRequest, 1),
		transport:   transport,
		destAddr:    remoteAddr,
		proto:       proto,
	}
	tryingRes := BuildResponse(100, "Trying", req, nil)
	tx.send(tryingRes)
	go tx.run()
	tx.requests <- req
	return tx, nil
}

func (tx *InviteServerTx) ID() string { return tx.id }
func (tx *InviteServerTx) Done() <-chan bool { return tx.done }
func (tx *InviteServerTx) Requests() <-chan *SIPRequest { return tx.requests }

func (tx *InviteServerTx) Terminate() {
	tx.mu.Lock()
	if tx.state == TxStateTerminated {
		tx.mu.Unlock()
		return
	}
	log.Printf("Terminating INVITE server transaction %s", tx.id)
	tx.state = TxStateTerminated
	if tx.timerG != nil { tx.timerG.Stop() }
	if tx.timerH != nil { tx.timerH.Stop() }
	if tx.timerI != nil { tx.timerI.Stop() }
	tx.mu.Unlock()
	close(tx.done)
}

func (tx *InviteServerTx) Receive(req *SIPRequest) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if req.Method == "ACK" {
		if tx.state == TxStateCompleted {
			log.Printf("ACK received for transaction %s, moving to Confirmed state.", tx.id)
			tx.state = TxStateConfirmed
			if tx.timerG != nil { tx.timerG.Stop() }
			if tx.timerH != nil { tx.timerH.Stop() }
			tx.timerI = time.AfterFunc(T4, tx.Terminate)
		}
		return
	}
	switch tx.state {
	case TxStateProceeding, TxStateCompleted:
		if tx.lastResponse != nil {
			log.Printf("Retransmitting last response for INVITE transaction %s", tx.id)
			tx.send(tx.lastResponse)
		}
	}
}

func (tx *InviteServerTx) Respond(res *SIPResponse) error {
	select {
	case tx.responses <- res:
		return nil
	case <-tx.done:
		return fmt.Errorf("transaction terminated")
	}
}

func (tx *InviteServerTx) run() {
	defer tx.Terminate()
	for {
		select {
		case res := <-tx.responses:
			tx.mu.Lock()
			if tx.state == TxStateTerminated {
				tx.mu.Unlock()
				return
			}
			tx.lastResponse = res
			tx.send(res)
			switch {
			case res.StatusCode >= 101 && res.StatusCode < 200:
			case res.StatusCode >= 200 && res.StatusCode < 300:
				// Per RFC 3261 Section 17.2.1, the transaction terminates immediately after
				// passing a 2xx response to the transport. The TU is responsible for retransmissions.
				tx.mu.Unlock()
				tx.Terminate()
				return
			case res.StatusCode >= 300 && res.StatusCode < 700:
				tx.state = TxStateCompleted
				tx.startTimerG()
				tx.timerH = time.AfterFunc(64*T1, func() {
					log.Printf("INVITE server transaction %s timed out waiting for ACK (Timer H).", tx.id)
					tx.Terminate()
				})
			}
			tx.mu.Unlock()
		case <-tx.done:
			return
		}
	}
}

func (tx *InviteServerTx) startTimerG() {
	if strings.ToUpper(tx.proto) == "TCP" {
		return // Do not retransmit responses over reliable transport
	}
	interval := T1
	tx.timerG = time.AfterFunc(interval, func() {
		tx.mu.Lock()
		defer tx.mu.Unlock()
		if tx.state != TxStateCompleted { return }
		log.Printf("Retransmitting final response for INVITE tx %s (Timer G)", tx.id)
		tx.send(tx.lastResponse)
		interval *= 2
		if interval > T2 { interval = T2 }
		tx.timerG.Reset(interval)
	})
}

func (tx *InviteServerTx) send(res *SIPResponse) {
	log.Printf("TX %s: Sending response:\n%s", tx.id, res.String())
	_, err := tx.transport.WriteTo([]byte(res.String()), tx.destAddr)
	if err != nil {
		log.Printf("TX %s: transport error sending response: %v", tx.id, err)
		tx.Terminate()
	}
}

// --- Client Transactions ---

// NonInviteClientTx implements the client-side non-INVITE transaction state machine.
type NonInviteClientTx struct {
	id        string
	request   *SIPRequest
	state     TxState
	mu        sync.RWMutex
	timerE    *time.Timer
	timerF    *time.Timer
	timerK    *time.Timer
	done      chan bool
	responses chan *SIPResponse
	transport net.PacketConn
	destAddr  net.Addr
	proto     string
}

func NewNonInviteClientTx(req *SIPRequest, transport net.PacketConn, dest net.Addr, proto string) (ClientTransaction, error) {
	topVia, err := req.TopVia()
	if err != nil {
		return nil, err
	}
	tx := &NonInviteClientTx{
		id:        topVia.Branch(),
		request:   req,
		state:     TxStateTrying,
		done:      make(chan bool),
		responses: make(chan *SIPResponse, 1),
		transport: transport,
		destAddr:  dest,
		proto:     proto,
	}
	go tx.run()
	return tx, nil
}

func (tx *NonInviteClientTx) ID() string { return tx.id }
func (tx *NonInviteClientTx) Done() <-chan bool { return tx.done }
func (tx *NonInviteClientTx) Responses() <-chan *SIPResponse { return tx.responses }

func (tx *NonInviteClientTx) Terminate() {
	tx.mu.Lock()
	if tx.state == TxStateTerminated {
		tx.mu.Unlock()
		return
	}
	log.Printf("Terminating non-INVITE client transaction %s", tx.id)
	tx.state = TxStateTerminated
	if tx.timerE != nil { tx.timerE.Stop() }
	if tx.timerF != nil { tx.timerF.Stop() }
	if tx.timerK != nil { tx.timerK.Stop() }
	tx.mu.Unlock()
	close(tx.done)
}

func (tx *NonInviteClientTx) ReceiveResponse(res *SIPResponse) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.state == TxStateTerminated || tx.state == TxStateCompleted { return }

	sendResponseToTU := func(r *SIPResponse) {
		select {
		case tx.responses <- r:
		default:
			log.Printf("TX %s: responses channel full or closed, dropping response", tx.id)
		}
	}

	sendResponseToTU(res)
	if res.StatusCode >= 200 {
		tx.state = TxStateCompleted
		if tx.timerE != nil { tx.timerE.Stop() }
		if tx.timerF != nil { tx.timerF.Stop() }
		tx.timerK = time.AfterFunc(T4, tx.Terminate)
	} else {
		tx.state = TxStateProceeding
	}
}

func (tx *NonInviteClientTx) run() {
	defer tx.Terminate()
	tx.sendRequest()
	tx.timerF = time.AfterFunc(64*T1, func() {
		log.Printf("Non-INVITE client tx %s timed out (Timer F)", tx.id)
		tx.responses <- &SIPResponse{StatusCode: 408, Reason: "Request Timeout"}
		tx.Terminate()
	})
	tx.startTimerE(T1)
	<-tx.done
}

func (tx *NonInviteClientTx) startTimerE(interval time.Duration) {
	if strings.ToUpper(tx.proto) == "TCP" {
		return // Do not retransmit requests over reliable transport
	}
	tx.timerE = time.AfterFunc(interval, func() {
		tx.mu.Lock()
		defer tx.mu.Unlock()
		if tx.state != TxStateTrying && tx.state != TxStateProceeding { return }

		tx.sendRequest()

		newInterval := interval * 2
		if tx.state == TxStateProceeding || newInterval > T2 {
			newInterval = T2
		}
		tx.startTimerE(newInterval)
	})
}

func (tx *NonInviteClientTx) sendRequest() {
	log.Printf("TX %s: Sending request:\n%s", tx.id, tx.request.String())
	_, err := tx.transport.WriteTo([]byte(tx.request.String()), tx.destAddr)
	if err != nil {
		log.Printf("TX %s: transport error sending request: %v", tx.id, err)
		tx.responses <- &SIPResponse{StatusCode: 503, Reason: "Service Unavailable"}
		tx.Terminate()
	}
}

// InviteClientTx implements the client-side INVITE transaction state machine.
type InviteClientTx struct {
	id        string
	request   *SIPRequest
	state     TxState
	mu        sync.RWMutex
	timerA    *time.Timer
	timerB    *time.Timer
	timerD    *time.Timer
	done      chan bool
	responses chan *SIPResponse
	transport net.PacketConn
	destAddr  net.Addr
	proto     string
}

func NewInviteClientTx(req *SIPRequest, transport net.PacketConn, dest net.Addr, proto string) (ClientTransaction, error) {
	topVia, err := req.TopVia()
	if err != nil {
		return nil, err
	}
	tx := &InviteClientTx{
		id:        topVia.Branch(),
		request:   req,
		state:     TxStateCalling,
		done:      make(chan bool),
		responses: make(chan *SIPResponse, 1),
		transport: transport,
		destAddr:  dest,
		proto:     proto,
	}
	go tx.run()
	return tx, nil
}

func (tx *InviteClientTx) ID() string { return tx.id }
func (tx *InviteClientTx) Done() <-chan bool { return tx.done }
func (tx *InviteClientTx) Responses() <-chan *SIPResponse { return tx.responses }

func (tx *InviteClientTx) Terminate() {
	tx.mu.Lock()
	if tx.state == TxStateTerminated {
		tx.mu.Unlock()
		return
	}
	log.Printf("Terminating INVITE client transaction %s", tx.id)
	tx.state = TxStateTerminated
	if tx.timerA != nil { tx.timerA.Stop() }
	if tx.timerB != nil { tx.timerB.Stop() }
	if tx.timerD != nil { tx.timerD.Stop() }
	tx.mu.Unlock()
	close(tx.done)
}

func (tx *InviteClientTx) ReceiveResponse(res *SIPResponse) {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	if tx.state == TxStateTerminated {
		return
	}

	sendResponseToTU := func(r *SIPResponse) {
		select {
		case tx.responses <- r:
		default:
			log.Printf("TX %s: responses channel full or closed, dropping response", tx.id)
		}
	}

	if res.StatusCode >= 100 && res.StatusCode < 200 {
		if tx.state == TxStateCalling {
			tx.state = TxStateProceeding
			// Per RFC 3261 Section 17.1.1.2, stop retransmitting on provisional response.
			if tx.timerA != nil {
				tx.timerA.Stop()
			}
		}
		sendResponseToTU(res)
		return
	}

	if res.StatusCode >= 300 {
		if tx.state == TxStateCalling || tx.state == TxStateProceeding {
			tx.state = TxStateCompleted
			sendResponseToTU(res)
			tx.sendAck(res) // Send ACK for non-2xx final response
			// Timer D value depends on transport. For reliable transport, it's 0.
			timerDVal := 32 * time.Second
			if strings.ToUpper(tx.proto) == "TCP" {
				timerDVal = 0
			}
			tx.timerD = time.AfterFunc(timerDVal, tx.Terminate)
		} else if tx.state == TxStateCompleted {
			// Retransmission of the final response. The transaction re-sends the ACK.
			tx.sendAck(res)
		}
		return
	}

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		if tx.state == TxStateCalling || tx.state == TxStateProceeding {
			tx.state = TxStateTerminated
			sendResponseToTU(res)
		}
	}
}

func (tx *InviteClientTx) sendAck(res *SIPResponse) {
	ack := BuildAck(res, tx.request)
	log.Printf("TX %s: Sending ACK for non-2xx final response:\n%s", tx.id, ack.String())
	_, err := tx.transport.WriteTo([]byte(ack.String()), tx.destAddr)
	if err != nil {
		log.Printf("TX %s: transport error sending ACK: %v", tx.id, err)
		tx.Terminate()
	}
}

func (tx *InviteClientTx) run() {
	defer tx.Terminate()
	tx.sendRequest()
	tx.timerB = time.AfterFunc(64*T1, func() {
		log.Printf("INVITE client tx %s timed out (Timer B)", tx.id)
		tx.responses <- &SIPResponse{StatusCode: 408, Reason: "Request Timeout"}
		tx.Terminate()
	})
	tx.startTimerA(T1)
	<-tx.done
}

func (tx *InviteClientTx) startTimerA(interval time.Duration) {
	if strings.ToUpper(tx.proto) == "TCP" {
		return // Do not retransmit INVITE over reliable transport
	}
	tx.timerA = time.AfterFunc(interval, func() {
		tx.mu.Lock()
		defer tx.mu.Unlock()
		// Per RFC, retransmissions are only sent in the "Calling" state.
		if tx.state != TxStateCalling {
			return
		}

		tx.sendRequest()
		tx.startTimerA(interval * 2)
	})
}

func (tx *InviteClientTx) sendRequest() {
	log.Printf("TX %s: Sending INVITE request:\n%s", tx.id, tx.request.String())
	_, err := tx.transport.WriteTo([]byte(tx.request.String()), tx.destAddr)
	if err != nil {
		log.Printf("TX %s: transport error sending request: %v", tx.id, err)
		tx.responses <- &SIPResponse{StatusCode: 503, Reason: "Service Unavailable"}
		tx.Terminate()
	}
}
