package sip

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
