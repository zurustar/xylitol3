package sip

import (
	"log"
	"sync"
	"time"
)

// InviteClientTxState は、INVITEクライアントトランザクションの状態を定義します。
type InviteClientTxState int

const (
	InviteClientTxStateCalling InviteClientTxState = iota
	InviteClientTxStateProceeding
	InviteClientTxStateCompleted
	InviteClientTxStateTerminated
)

// InviteClientTx は、クライアント側のINVITEトランザクションのステートマシンを実装します。
type InviteClientTx struct {
	id           string
	request      *SIPRequest
	state        InviteClientTxState
	mu           sync.RWMutex
	timerA       *time.Timer
	timerB       *time.Timer
	timerD       *time.Timer
	done         chan bool
	responses    chan *SIPResponse
	transport    Transport
	lastResponse *SIPResponse
}

func (tx *InviteClientTx) LastResponse() *SIPResponse {
	tx.mu.RLock()
	defer tx.mu.RUnlock()
	return tx.lastResponse
}

func NewInviteClientTx(req *SIPRequest, transport Transport) (ClientTransaction, error) {
	topVia, err := req.TopVia()
	if err != nil {
		return nil, err
	}
	tx := &InviteClientTx{
		id:        topVia.Branch(),
		request:   req,
		state:     InviteClientTxStateCalling,
		done:      make(chan bool),
		responses: make(chan *SIPResponse, 1),
		transport: transport,
	}
	go tx.run()
	return tx, nil
}

func (tx *InviteClientTx) ID() string { return tx.id }
func (tx *InviteClientTx) Done() <-chan bool { return tx.done }
func (tx *InviteClientTx) Responses() <-chan *SIPResponse { return tx.responses }
func (tx *InviteClientTx) OriginalRequest() *SIPRequest { return tx.request }

func (tx *InviteClientTx) Transport() Transport {
	return tx.transport
}

func (tx *InviteClientTx) Terminate() {
	tx.mu.Lock()
	if tx.state == InviteClientTxStateTerminated {
		tx.mu.Unlock()
		return
	}
	log.Printf("Terminating INVITE client transaction %s", tx.id)
	tx.state = InviteClientTxStateTerminated
	if tx.timerA != nil { tx.timerA.Stop() }
	if tx.timerB != nil { tx.timerB.Stop() }
	if tx.timerD != nil { tx.timerD.Stop() }
	tx.mu.Unlock()
	close(tx.done)
}

func (tx *InviteClientTx) ReceiveResponse(res *SIPResponse) {
	tx.mu.Lock()
	tx.lastResponse = res

	if tx.state == InviteClientTxStateTerminated {
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
		if tx.state == InviteClientTxStateCalling {
			tx.state = InviteClientTxStateProceeding
			// RFC 3261 セクション 17.1.1.2 に従い、暫定応答で再送を停止します。
			if tx.timerA != nil {
				tx.timerA.Stop()
			}
		}
		sendResponseToTU(res)
		tx.mu.Unlock()
		return
	}

	if res.StatusCode >= 300 {
		if tx.state == InviteClientTxStateCalling || tx.state == InviteClientTxStateProceeding {
			tx.state = InviteClientTxStateCompleted
			sendResponseToTU(res)
			tx.sendAck(res) // 2xx以外の最終応答に対してACKを送信します
			if isReliable(tx.transport.GetProto()) {
				tx.mu.Unlock()
				tx.Terminate()
				return
			}
			tx.timerD = time.AfterFunc(32*time.Second, tx.Terminate)
		} else if tx.state == InviteClientTxStateCompleted {
			// 最終応答の再送。トランザクションはACKを再送します。
			tx.sendAck(res)
		}
		tx.mu.Unlock()
		return
	}

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		if tx.state == InviteClientTxStateCalling || tx.state == InviteClientTxStateProceeding {
			tx.state = InviteClientTxStateTerminated
			sendResponseToTU(res)
			// ロックを解除する必要はありません。run()からTerminateが呼び出され、そこでロックが解除されます。
			// ただし、Terminateを直接呼び出す方が良いでしょう。ロックを解除して終了しましょう。
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
	_, err := tx.transport.Write([]byte(ack.String()))
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
	if isReliable(tx.transport.GetProto()) {
		return // 信頼性の高いトランスポートでINVITEを再送しないでください
	}
	tx.timerA = time.AfterFunc(interval, func() {
		tx.mu.Lock()
		defer tx.mu.Unlock()
		// RFCによると、再送は「呼び出し中」状態でのみ送信されます。
		if tx.state != InviteClientTxStateCalling {
			return
		}

		tx.sendRequest()
		tx.startTimerA(interval * 2)
	})
}

func (tx *InviteClientTx) sendRequest() {
	log.Printf("TX %s: Sending INVITE request:\n%s", tx.id, tx.request.String())
	_, err := tx.transport.Write([]byte(tx.request.String()))
	if err != nil {
		log.Printf("TX %s: transport error sending request: %v", tx.id, err)
		tx.responses <- &SIPResponse{StatusCode: 503, Reason: "Service Unavailable"}
		tx.Terminate()
	}
}
