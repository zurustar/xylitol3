package sip

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// NonInviteServerTxState は、非INVITEサーバートランザクションの状態を定義します。
type NonInviteServerTxState int

const (
	NonInviteServerTxStateTrying NonInviteServerTxState = iota
	NonInviteServerTxStateProceeding
	NonInviteServerTxStateCompleted
	NonInviteServerTxStateTerminated
)

// NonInviteServerTx は、サーバー側の非INVITEトランザクションステートマシンを実装します。
type NonInviteServerTx struct {
	id           string
	originalReq  *SIPRequest
	lastResponse *SIPResponse
	state        NonInviteServerTxState
	mu           sync.RWMutex
	timerJ       *time.Timer
	done         chan bool
	responses    chan *SIPResponse
	requests     chan *SIPRequest
	transport    Transport
}

// NewNonInviteServerTx は、新しい非INVITEサーバートランザクションを作成して開始します。
func NewNonInviteServerTx(req *SIPRequest, transport Transport) (ServerTransaction, error) {
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
		state:       NonInviteServerTxStateTrying,
		done:        make(chan bool),
		responses:   make(chan *SIPResponse, 1),
		requests:    make(chan *SIPRequest, 1),
		transport:   transport,
	}

	go tx.run()
	tx.requests <- req // 最初の要求をTUに渡します

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
	if tx.state == NonInviteServerTxStateTerminated {
		tx.mu.Unlock()
		return
	}
	log.Printf("Terminating non-INVITE server transaction %s", tx.id)
	tx.state = NonInviteServerTxStateTerminated
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
	case NonInviteServerTxStateTrying:
	case NonInviteServerTxStateProceeding, NonInviteServerTxStateCompleted:
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

func (tx *NonInviteServerTx) OriginalRequest() *SIPRequest {
	return tx.originalReq
}

// LastResponseは、このトランザクションで送信された最後のレスポンスを返します。
func (tx *NonInviteServerTx) LastResponse() *SIPResponse {
	tx.mu.RLock()
	defer tx.mu.RUnlock()
	return tx.lastResponse
}

func (tx *NonInviteServerTx) Transport() Transport {
	return tx.transport
}

func (tx *NonInviteServerTx) run() {
	defer tx.Terminate()
	for {
		select {
		case res := <-tx.responses:
			tx.mu.Lock()
			if tx.state == NonInviteServerTxStateTerminated {
				tx.mu.Unlock()
				return
			}
			if tx.state == NonInviteServerTxStateCompleted && tx.lastResponse.StatusCode >= 200 {
				log.Printf("Ignoring new final response for completed transaction %s", tx.id)
				tx.mu.Unlock()
				continue
			}
			tx.lastResponse = res
			tx.send(res)
			if res.StatusCode >= 200 {
				tx.state = NonInviteServerTxStateCompleted
				// 信頼性の高いトランスポートの場合はすぐに終了します。信頼性の低い場合はタイマーJを開始します。
				if isReliable(tx.transport.GetProto()) {
					tx.mu.Unlock() // Terminateを呼び出す前にロックを解除します
					tx.Terminate()
					return // ゴルーチンを終了します
				}
				tx.timerJ = time.AfterFunc(64*T1, tx.Terminate)
			} else {
				tx.state = NonInviteServerTxStateProceeding
			}
			tx.mu.Unlock()
		case <-tx.done:
			return
		}
	}
}

func (tx *NonInviteServerTx) send(res *SIPResponse) {
	log.Printf("TX %s: Sending response:\n%s", tx.id, res.String())
	_, err := tx.transport.Write([]byte(res.String()))
	if err != nil {
		log.Printf("TX %s: transport error sending response: %v", tx.id, err)
		tx.Terminate()
	}
}
