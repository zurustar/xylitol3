package sip

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// InviteServerTxState は、INVITEサーバートランザクションの状態を定義します。
type InviteServerTxState int

const (
	InviteServerTxStateProceeding InviteServerTxState = iota
	InviteServerTxStateCompleted
	InviteServerTxStateConfirmed
	InviteServerTxStateTerminated
)

// InviteServerTx は、サーバー側のINVITEトランザクションステートマシンを実装します。
type InviteServerTx struct {
	id           string
	originalReq  *SIPRequest
	lastResponse *SIPResponse
	state        InviteServerTxState
	mu           sync.RWMutex
	timerG       *time.Timer
	timerH       *time.Timer
	timerI       *time.Timer
	done         chan bool
	responses    chan *SIPResponse
	requests     chan *SIPRequest
	transport    Transport
}

// NewInviteServerTx は、新しいINVITEサーバートランザクションを作成して開始します。
func NewInviteServerTx(req *SIPRequest, transport Transport) (ServerTransaction, error) {
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
		state:       InviteServerTxStateProceeding,
		done:        make(chan bool),
		responses:   make(chan *SIPResponse, 1),
		requests:    make(chan *SIPRequest, 1),
		transport:   transport,
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
func (tx *InviteServerTx) OriginalRequest() *SIPRequest { return tx.originalReq }

func (tx *InviteServerTx) Transport() Transport {
	return tx.transport
}

func (tx *InviteServerTx) Terminate() {
	tx.mu.Lock()
	if tx.state == InviteServerTxStateTerminated {
		tx.mu.Unlock()
		return
	}
	log.Printf("Terminating INVITE server transaction %s", tx.id)
	tx.state = InviteServerTxStateTerminated
	if tx.timerG != nil { tx.timerG.Stop() }
	if tx.timerH != nil { tx.timerH.Stop() }
	if tx.timerI != nil { tx.timerI.Stop() }
	tx.mu.Unlock()
	close(tx.done)
}

func (tx *InviteServerTx) Receive(req *SIPRequest) {
	tx.mu.Lock()
	if req.Method == "ACK" {
		if tx.state == InviteServerTxStateCompleted {
			log.Printf("ACK received for transaction %s, moving to Confirmed state.", tx.id)
			tx.state = InviteServerTxStateConfirmed
			if tx.timerG != nil {
				tx.timerG.Stop()
			}
			if tx.timerH != nil {
				tx.timerH.Stop()
			}
			// 信頼性の高いトランスポートの場合はすぐに終了します。信頼性の低い場合はタイマーIを開始します。
			if isReliable(tx.transport.GetProto()) {
				tx.mu.Unlock() // Terminateを呼び出す前にロックを解除します。Terminateは再度ロックします。
				tx.Terminate()
				return
			}
			tx.timerI = time.AfterFunc(T4, tx.Terminate)
		}
		tx.mu.Unlock()
		return
	}

	switch tx.state {
	case InviteServerTxStateProceeding, InviteServerTxStateCompleted:
		if tx.lastResponse != nil {
			log.Printf("Retransmitting last response for INVITE transaction %s", tx.id)
			tx.send(tx.lastResponse)
		}
	}
	tx.mu.Unlock()
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
			if tx.state == InviteServerTxStateTerminated {
				tx.mu.Unlock()
				return
			}
			tx.lastResponse = res
			tx.send(res)
			switch {
			case res.StatusCode >= 101 && res.StatusCode < 200:
			case res.StatusCode >= 200 && res.StatusCode < 300:
				// RFC 3261セクション17.2.1によると、トランザクションは2xx応答をトランスポートに渡した直後に終了します。
				// TUが再送信を担当します。
				tx.mu.Unlock()
				tx.Terminate()
				return
			case res.StatusCode >= 300 && res.StatusCode < 700:
				tx.state = InviteServerTxStateCompleted
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
	if isReliable(tx.transport.GetProto()) {
		return // 信頼性の高いトランスポートで応答を再送信しないでください
	}
	interval := T1
	tx.timerG = time.AfterFunc(interval, func() {
		tx.mu.Lock()
		defer tx.mu.Unlock()
		if tx.state != InviteServerTxStateCompleted { return }
		log.Printf("Retransmitting final response for INVITE tx %s (Timer G)", tx.id)
		tx.send(tx.lastResponse)
		interval *= 2
		if interval > T2 { interval = T2 }
		tx.timerG.Reset(interval)
	})
}

func (tx *InviteServerTx) send(res *SIPResponse) {
	log.Printf("TX %s: Sending response:\n%s", tx.id, res.String())
	_, err := tx.transport.Write([]byte(res.String()))
	if err != nil {
		log.Printf("TX %s: transport error sending response: %v", tx.id, err)
		tx.Terminate()
	}
}
