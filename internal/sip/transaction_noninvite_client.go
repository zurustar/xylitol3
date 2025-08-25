package sip

import (
	"log"
	"sync"
	"time"
)

// NonInviteClientTxState は、非INVITEクライアントトランザクションの状態を定義します。
type NonInviteClientTxState int

const (
	NonInviteClientTxStateTrying NonInviteClientTxState = iota
	NonInviteClientTxStateProceeding
	NonInviteClientTxStateCompleted
	NonInviteClientTxStateTerminated
)

// NonInviteClientTx は、クライアント側の非INVITEトランザクションステートマシンを実装します。
type NonInviteClientTx struct {
	id           string
	request      *SIPRequest
	state        NonInviteClientTxState
	mu           sync.RWMutex
	timerE       *time.Timer
	timerF       *time.Timer
	timerK       *time.Timer
	done         chan bool
	responses    chan *SIPResponse
	transport    Transport
	lastResponse *SIPResponse
}

func (tx *NonInviteClientTx) LastResponse() *SIPResponse {
	tx.mu.RLock()
	defer tx.mu.RUnlock()
	return tx.lastResponse
}

func NewNonInviteClientTx(req *SIPRequest, transport Transport) (ClientTransaction, error) {
	topVia, err := req.TopVia()
	if err != nil {
		return nil, err
	}
	tx := &NonInviteClientTx{
		id:        topVia.Branch(),
		request:   req,
		state:     NonInviteClientTxStateTrying,
		done:      make(chan bool),
		responses: make(chan *SIPResponse, 1),
		transport: transport,
	}
	go tx.run()
	return tx, nil
}

func (tx *NonInviteClientTx) ID() string { return tx.id }
func (tx *NonInviteClientTx) Done() <-chan bool { return tx.done }
func (tx *NonInviteClientTx) Responses() <-chan *SIPResponse { return tx.responses }
func (tx *NonInviteClientTx) OriginalRequest() *SIPRequest { return tx.request }

func (tx *NonInviteClientTx) Transport() Transport {
	return tx.transport
}

func (tx *NonInviteClientTx) Terminate() {
	tx.mu.Lock()
	if tx.state == NonInviteClientTxStateTerminated {
		tx.mu.Unlock()
		return
	}
	log.Printf("Terminating non-INVITE client transaction %s", tx.id)
	tx.state = NonInviteClientTxStateTerminated
	if tx.timerE != nil { tx.timerE.Stop() }
	if tx.timerF != nil { tx.timerF.Stop() }
	if tx.timerK != nil { tx.timerK.Stop() }
	tx.mu.Unlock()
	close(tx.done)
}

func (tx *NonInviteClientTx) ReceiveResponse(res *SIPResponse) {
	tx.mu.Lock()
	tx.lastResponse = res
	if tx.state == NonInviteClientTxStateTerminated || tx.state == NonInviteClientTxStateCompleted {
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

	sendResponseToTU(res)
	if res.StatusCode >= 200 {
		tx.state = NonInviteClientTxStateCompleted
		if tx.timerE != nil {
			tx.timerE.Stop()
		}
		if tx.timerF != nil {
			tx.timerF.Stop()
		}
		if isReliable(tx.transport.GetProto()) {
			tx.mu.Unlock()
			tx.Terminate()
			return
		}
		tx.timerK = time.AfterFunc(T4, tx.Terminate)
	} else {
		tx.state = NonInviteClientTxStateProceeding
	}
	tx.mu.Unlock()
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
	if isReliable(tx.transport.GetProto()) {
		return // 信頼性の高いトランスポートでリクエストを再送しないでください
	}
	tx.timerE = time.AfterFunc(interval, func() {
		tx.mu.Lock()
		defer tx.mu.Unlock()
		if tx.state != NonInviteClientTxStateTrying && tx.state != NonInviteClientTxStateProceeding { return }

		tx.sendRequest()

		newInterval := interval * 2
		if tx.state == NonInviteClientTxStateProceeding || newInterval > T2 {
			newInterval = T2
		}
		tx.startTimerE(newInterval)
	})
}

func (tx *NonInviteClientTx) sendRequest() {
	log.Printf("TX %s: Sending request:\n%s", tx.id, tx.request.String())
	_, err := tx.transport.Write([]byte(tx.request.String()))
	if err != nil {
		log.Printf("TX %s: transport error sending request: %v", tx.id, err)
		tx.responses <- &SIPResponse{StatusCode: 503, Reason: "Service Unavailable"}
		tx.Terminate()
	}
}
