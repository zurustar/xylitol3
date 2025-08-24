package sip

import (
	"log"
	"sort"
	"sync"
	"time"
)

// ForkingProxy manages the state for a forked SIP request. It creates and
// manages multiple client transactions and aggregates their responses.
type ForkingProxy struct {
	upstreamTx          ServerTransaction
	clientTxs           []ClientTransaction
	responses           chan *SIPResponse
	done                chan bool
	mu                  sync.Mutex
	best                *SIPResponse
	finishOnce          sync.Once
	externallyCancelled bool
}

// NewForkingProxy creates a new ForkingProxy.
func NewForkingProxy(upstreamTx ServerTransaction) *ForkingProxy {
	return &ForkingProxy{
		upstreamTx: upstreamTx,
		clientTxs:  make([]ClientTransaction, 0),
		responses:  make(chan *SIPResponse, 10), // Buffered channel
		done:       make(chan bool),
	}
}

// AddBranch adds a new client transaction (a "branch") to the proxy.
func (p *ForkingProxy) AddBranch(clientTx ClientTransaction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clientTxs = append(p.clientTxs, clientTx)
}

// SetExternallyCancelled marks the proxy as cancelled by an external event (e.g. a CANCEL request).
func (p *ForkingProxy) SetExternallyCancelled() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.externallyCancelled = true
}

// Run starts the response aggregation logic. It should be run in a goroutine.
func (p *ForkingProxy) Run(s *SIPServer) {
	// When this proxy is done, remove it from the server's map.
	defer func() {
		s.forkingMutex.Lock()
		delete(s.forkingProxies, p.upstreamTx.ID())
		s.forkingMutex.Unlock()
		log.Printf("ForkingProxy for tx %s cleaned up.", p.upstreamTx.ID())
	}()

	p.mu.Lock()
	totalBranches := len(p.clientTxs)
	p.mu.Unlock()

	if totalBranches == 0 {
		log.Printf("ForkingProxy started with no branches for tx %s", p.upstreamTx.ID())
		p.upstreamTx.Respond(BuildResponse(404, "Not Found", p.upstreamTx.OriginalRequest(), nil))
		close(p.done)
		return
	}

	log.Printf("ForkingProxy started with %d branches for tx %s", totalBranches, p.upstreamTx.ID())

	var wg sync.WaitGroup
	wg.Add(totalBranches)

	p.mu.Lock()
	for _, tx := range p.clientTxs {
		go p.listenToBranch(tx, &wg)
	}
	p.mu.Unlock()

	// Wait for all branches to terminate, then decide what to do.
	go func() {
		wg.Wait()
		p.finishOnce.Do(p.finish)
	}()

	// Main loop to process responses as they arrive.
	for res := range p.responses {
		if p.processResponse(res) {
			// A final response (2xx) was chosen, so we can exit early.
			break
		}
	}
	// No matter how the loop exits (break or channel closed), we try to finish.
	p.finishOnce.Do(p.finish)
}

// listenToBranch listens for responses from a single client transaction.
func (p *ForkingProxy) listenToBranch(tx ClientTransaction, wg *sync.WaitGroup) {
	defer wg.Done()
	isInvite := tx.OriginalRequest().Method == "INVITE"
	timerC := time.NewTimer(3 * time.Minute) // Timer C for INVITEs
	if !isInvite {
		timerC.Stop() // Not needed for non-INVITE
	}
	defer timerC.Stop()

	for {
		select {
		case res, ok := <-tx.Responses():
			if !ok {
				return
			}
			if isInvite && res.StatusCode >= 100 && res.StatusCode < 200 {
				// Any provisional response resets Timer C.
				timerC.Reset(3 * time.Minute)
				// Forward provisional responses upstream immediately.
				p.upstreamTx.Respond(res)
			} else {
				p.responses <- res
			}
		case <-tx.Done():
			return
		case <-timerC.C:
			if isInvite {
				log.Printf("ForkingProxy: Timer C fired for branch %s", tx.ID())
				tx.Terminate()
			}
			return
		}
	}
}

// processResponse handles a single response from a branch.
// It returns true if a final response has been selected and forwarded.
func (p *ForkingProxy) processResponse(res *SIPResponse) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.best != nil && p.best.StatusCode >= 200 {
		return true
	}

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		log.Printf("ForkingProxy: Received 2xx response for tx %s. This is the one!", p.upstreamTx.ID())
		p.best = res
		p.upstreamTx.Respond(p.best)
		close(p.responses) // Signal the main loop to stop and trigger cancellation.
		return true
	}

	if res.StatusCode >= 300 {
		log.Printf("ForkingProxy: Received %d response for tx %s", res.StatusCode, p.upstreamTx.ID())
		if p.best == nil || res.StatusCode > p.best.StatusCode {
			p.best = res
		}
	}
	return false
}

// finish is called when all branches have terminated or a 2xx was received.
func (p *ForkingProxy) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Cancel all branches that are not the source of the best response.
	for _, tx := range p.clientTxs {
		isBestBranch := false
		if p.best != nil {
			topVia, err := p.best.TopVia()
			if err == nil && topVia.Branch() == tx.ID() {
				isBestBranch = true
			}
		}

		if !isBestBranch {
			select {
			case <-tx.Done():
			default:
				if tx.OriginalRequest().Method == "INVITE" {
					log.Printf("ForkingProxy: Cancelling branch %s", tx.ID())
					cancelReq := createCancelRequest(tx.OriginalRequest())
					if _, err := tx.Transport().Write([]byte(cancelReq.String())); err != nil {
						log.Printf("Error sending CANCEL to branch %s: %v", tx.ID(), err)
					}
				}
				tx.Terminate()
			}
		}
	}

	// If we were not cancelled externally and we still only have a non-2xx response, send it now.
	if !p.externallyCancelled && p.best != nil && p.best.StatusCode >= 300 {
		log.Printf("ForkingProxy: All branches terminated. Sending best non-2xx response (%d) for tx %s", p.best.StatusCode, p.upstreamTx.ID())
		p.upstreamTx.Respond(p.best)
	} else if !p.externallyCancelled && p.best == nil {
		log.Printf("ForkingProxy: All branches terminated without any usable response for tx %s.", p.upstreamTx.ID())
		p.upstreamTx.Respond(BuildResponse(408, "Request Timeout", p.upstreamTx.OriginalRequest(), nil))
	}

	close(p.done)
}

// Wait blocks until the forking proxy has completed its work.
func (p *ForkingProxy) Wait() {
	<-p.done
}

// ByStatusCode implements sort.Interface for []*SIPResponse based on StatusCode.
type ByStatusCode []*SIPResponse

func (a ByStatusCode) Len() int           { return len(a) }
func (a ByStatusCode) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByStatusCode) Less(i, j int) bool { return a[i].StatusCode < a[j].StatusCode }

// BestResponse picks the best response from a slice of final responses,
// according to RFC 3261 Section 16.7.
func BestResponse(responses []*SIPResponse) *SIPResponse {
	if len(responses) == 0 {
		return nil
	}

	var a2xx, a6xx, other []*SIPResponse
	for _, r := range responses {
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			a2xx = append(a2xx, r)
		} else if r.StatusCode >= 600 && r.StatusCode < 700 {
			a6xx = append(a6xx, r)
		} else if r.StatusCode >= 300 {
			other = append(other, r)
		}
	}

	if len(a2xx) > 0 {
		return a2xx[0] // First 2xx wins
	}
	if len(a6xx) > 0 {
		sort.Sort(sort.Reverse(ByStatusCode(a6xx)))
		return a6xx[0]
	}
	if len(other) > 0 {
		sort.Sort(sort.Reverse(ByStatusCode(other)))
		return other[0]
	}
	return responses[0]
}
