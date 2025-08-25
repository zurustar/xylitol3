package sip

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	// RFC3261BranchMagicCookie は、ブランチIDのマジッククッキープレフィックスです。
	RFC3261BranchMagicCookie = "z9hG4bK"
	// T1 はRTTの推定値、500msです。
	T1 = 500 * time.Millisecond
	// T2 は非INVITEの最大再送間隔、4秒です。
	T2 = 4 * time.Second
	// T4 は最大メッセージ寿命、5秒です。
	T4 = 5 * time.Second
)

// GenerateBranchID は、新しいRFC3261準拠のブランチIDを生成します。
func GenerateBranchID() string {
	b := make([]byte, 8) // 16進数16文字
	rand.Read(b)
	return fmt.Sprintf("%s%s", RFC3261BranchMagicCookie, hex.EncodeToString(b))
}

// GenerateNonce は、長さ2*nのランダムな16進文字列を生成します。
func GenerateNonce(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// GenerateCallID は、新しいランダムなCall-IDを生成します。
func GenerateCallID() string {
	return GenerateNonce(16)
}

// GenerateTag は、From/Toヘッダー用の新しいランダムなタグを生成します。
func GenerateTag() string {
	return GenerateNonce(8)
}

// isReliable は、指定されたトランスポートプロトコルが信頼性があるかどうか（例：TCP、SCTP、TLS）をチェックします。
func isReliable(proto string) bool {
	switch strings.ToUpper(proto) {
	case "TCP", "SCTP", "TLS":
		return true
	default:
		return false
	}
}

// --- トランザクションインターフェース ---

// BaseTransaction は、すべてのトランザクションの基本インターフェースです。
type BaseTransaction interface {
	ID() string
	Done() <-chan bool
	Terminate()
	Transport() Transport
}

// ServerTransaction は、サーバー側のトランザクションを表します。
type ServerTransaction interface {
	BaseTransaction
	Receive(*SIPRequest)
	Respond(*SIPResponse) error
	Requests() <-chan *SIPRequest
	OriginalRequest() *SIPRequest
	LastResponse() *SIPResponse
}

// ClientTransaction は、クライアント側のトランザクションを表します。
type ClientTransaction interface {
	BaseTransaction
	Responses() <-chan *SIPResponse
	ReceiveResponse(*SIPResponse)
	OriginalRequest() *SIPRequest
	LastResponse() *SIPResponse
}

// --- トランザクションマネージャー ---

// TransactionManager は、すべてのアクティブなSIPトランザクションを管理します。
type TransactionManager struct {
	transactions map[string]BaseTransaction
	mu           sync.RWMutex
}

// NewTransactionManager は、新しいTransactionManagerを作成します。
func NewTransactionManager() *TransactionManager {
	return &TransactionManager{
		transactions: make(map[string]BaseTransaction),
	}
}

// Get は、ID（ブランチID）でトランザクションを検索します。
func (tm *TransactionManager) Get(branchID string) (BaseTransaction, bool) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	tx, ok := tm.transactions[branchID]
	return tx, ok
}

// Add は、新しいトランザクションをマネージャーに追加します。
func (tm *TransactionManager) Add(tx BaseTransaction) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.transactions[tx.ID()] = tx

	// トランザクションが終了したら、マップからクリーンアップします。
	go func() {
		<-tx.Done()
		tm.Remove(tx.ID())
	}()
}

// Remove は、マネージャーからトランザクションを削除します。
func (tm *TransactionManager) Remove(branchID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	delete(tm.transactions, branchID)
}
