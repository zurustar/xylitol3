package sip

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

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
