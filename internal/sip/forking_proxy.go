package sip

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ForkingProxy manages the state for a forked SIP request. It creates and
// manages multiple client transactions and aggregates their responses.
type ForkingProxy struct {
	s                   *SIPServer
	upstreamTx          ServerTransaction
	clientTxs           []ClientTransaction
	responses           chan *SIPResponse
	done                chan bool
	quit                chan struct{}
	quitOnce            sync.Once
	mu                  sync.Mutex
	best                *SIPResponse
	finishOnce          sync.Once
	externallyCancelled bool
}

// NewForkingProxy creates a new ForkingProxy.
func NewForkingProxy(s *SIPServer, upstreamTx ServerTransaction) *ForkingProxy {
	return &ForkingProxy{
		s:          s,
		upstreamTx: upstreamTx,
		clientTxs:  make([]ClientTransaction, 0),
		responses:  make(chan *SIPResponse, 10), // Buffered channel
		done:       make(chan bool),
		quit:       make(chan struct{}),
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
		if p.processResponse(res, p.s, &wg) {
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
				select {
				case p.responses <- res:
				case <-p.quit:
					return
				}
			}
		case <-tx.Done():
			return
		case <-p.quit:
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
func (p *ForkingProxy) processResponse(res *SIPResponse, s *SIPServer, wg *sync.WaitGroup) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.best != nil && p.best.StatusCode >= 200 {
		return true // Already have a final response, do nothing.
	}

	// Handle session timer negotiation (RFC 4028, Section 8.1.1)
	if res.StatusCode == 422 {
		originalReq := p.upstreamTx.OriginalRequest()
		if originalReq.Method == "INVITE" && strings.Contains(originalReq.GetHeader("Supported"), "timer") {
			if minSE, err := res.MinSE(); err == nil && minSE > 0 {
				log.Printf("ForkingProxy: Received 422 with Min-SE %d, attempting retry for %s.", minSE, p.upstreamTx.ID())

				via, err := res.TopVia()
				if err != nil {
					log.Printf("ForkingProxy: Could not parse Via from 422 response for %s: %v", p.upstreamTx.ID(), err)
				} else {
					branchID := via.Branch()
					var branchTx ClientTransaction
					for _, tx := range p.clientTxs {
						if tx.ID() == branchID {
							branchTx = tx
							break
						}
					}

					if branchTx != nil {
						retryReq := branchTx.OriginalRequest().Clone()
						cseqStr := retryReq.GetHeader("CSeq")
						if parts := strings.Fields(cseqStr); len(parts) == 2 {
							if cseq, err := strconv.Atoi(parts[0]); err == nil {
								retryReq.Headers["CSeq"] = fmt.Sprintf("%d %s", cseq+1, parts[1])
							}
						}
						retryReq.Headers["Session-Expires"] = strconv.Itoa(minSE)
						retryReq.Headers["Min-SE"] = strconv.Itoa(minSE)
						// The original request's Via is still on top, we need to remove it before sending.
						// The server will add its own Via. This is a bit of a hack.
						// A better implementation would have the transaction layer handle Vias.
						// For now, we'll just create a new client transaction.

						newClientTx, err := s.NewClientTx(retryReq, branchTx.Transport())
						if err != nil {
							log.Printf("ForkingProxy: Failed to create new client transaction for retry: %v", err)
							// Fallback to treating 422 as a generic error
							goto generic_error_handling
						}
						p.clientTxs = append(p.clientTxs, newClientTx)
						wg.Add(1)
						go p.listenToBranch(newClientTx, wg)

						log.Printf("ForkingProxy: Spawned new branch %s for session timer retry.", newClientTx.ID())
						return false // Don't treat 422 as a final response, wait for the new branch.
					}
				}
			}
		}
	}

generic_error_handling:
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		log.Printf("ForkingProxy: Received 2xx response for tx %s. This is the one!", p.upstreamTx.ID())
		p.best = res

		// Session Timer: UAS Does Not Support Timers (RFC 4028 Section 8.1.2)
		// If the original request wanted a timer, but the 2xx response doesn't have one,
		// the proxy MUST insert it.
		originalReq := p.upstreamTx.OriginalRequest()
		if se, _ := originalReq.SessionExpires(); se != nil { // Timer was requested
			if resSE, _ := p.best.SessionExpires(); resSE == nil { // Timer not in 2xx response
				log.Printf("ForkingProxy: UAS did not include Session-Expires in 2xx. Adding it now for tx %s.", p.upstreamTx.ID())
				// The proxy becomes the refresher, on behalf of the UAC.
				p.best.Headers["Session-Expires"] = fmt.Sprintf("%d;refresher=uac", se.Delta)
				// The proxy MUST also add a Require header.
				if existing, ok := p.best.Headers["Require"]; ok {
					p.best.Headers["Require"] = "timer, " + existing
				} else {
					p.best.Headers["Require"] = "timer"
				}
			}
		}

		p.upstreamTx.Respond(p.best)
		p.quitOnce.Do(func() {
			close(p.quit)
		})
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
	p.quitOnce.Do(func() {
		close(p.quit)
	})

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
