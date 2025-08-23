package sip

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

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
