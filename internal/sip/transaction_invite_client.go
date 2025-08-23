package sip

import (
	"log"
	"net"
	"sync"
	"time"
)

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

	if tx.state == TxStateTerminated {
		tx.mu.Unlock()
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
		tx.mu.Unlock()
		return
	}

	if res.StatusCode >= 300 {
		if tx.state == TxStateCalling || tx.state == TxStateProceeding {
			tx.state = TxStateCompleted
			sendResponseToTU(res)
			tx.sendAck(res) // Send ACK for non-2xx final response
			if isReliable(tx.proto) {
				tx.mu.Unlock()
				tx.Terminate()
				return
			}
			tx.timerD = time.AfterFunc(32*time.Second, tx.Terminate)
		} else if tx.state == TxStateCompleted {
			// Retransmission of the final response. The transaction re-sends the ACK.
			tx.sendAck(res)
		}
		tx.mu.Unlock()
		return
	}

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		if tx.state == TxStateCalling || tx.state == TxStateProceeding {
			tx.state = TxStateTerminated
			sendResponseToTU(res)
			// No need to unlock, Terminate will be called by run() which will unlock.
			// However, direct call to Terminate is better. Let's unlock and terminate.
			tx.mu.Unlock()
			tx.Terminate()
			return
		}
	}
	tx.mu.Unlock()
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
	if isReliable(tx.proto) {
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
