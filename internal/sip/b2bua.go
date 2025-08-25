package sip

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// B2BUAは単一の通話を管理し、バックツーバックユーザーエージェントとして機能します。
// 2つの仮想UAを制御します: VirtualUAS (UACからB2BUA) と VirtualUAC (B2BUAからUAS)。
type B2BUA struct {
	server             *SIPServer
	virtualUASTx       ServerTransaction // 仮想UASレッグ: 発呼者から
	virtualUACTx       ClientTransaction // 仮想UACレッグ: 被呼者へ
	virtualUASDialogID string
	virtualUACDialogID string
	virtualUASAck      *SIPRequest
	virtualUACAck      *SIPRequest
	done               chan bool
	mu                 sync.RWMutex
	cancel             func()
	StartTime          time.Time
}

// NewB2BUAは新しいB2BUAインスタンスを作成して初期化します。
func NewB2BUA(s *SIPServer, virtualUASTx ServerTransaction) *B2BUA {
	return &B2BUA{
		server:       s,
		virtualUASTx: virtualUASTx,
		done:         make(chan bool),
		StartTime:    time.Now(),
	}
}

// RunはB2BUAロジックを開始します。仮想UACレッグの作成と、
// 2つのレッグ間のコール状態の管理を担当します。
func (b *B2BUA) Run(targetURI string) {
	log.Printf("B2BUA started for Virtual-UAS-leg tx %s, targeting %s", b.virtualUASTx.ID(), targetURI)
	defer close(b.done)

	virtualUASReq := b.virtualUASTx.OriginalRequest()

	// セッションタイマーロジック - RFC 4028のセクション8.1
	if virtualUASReq.Method == "INVITE" || virtualUASReq.Method == "UPDATE" {
		const b2buaMinSE = 1800 // 推奨通り30分。

		se, err := virtualUASReq.SessionExpires()
		if err != nil {
			b.virtualUASTx.Respond(BuildResponse(400, "Bad Request", virtualUASReq, map[string]string{"Warning": "Malformed Session-Expires header"}))
			return
		}

		if se != nil { // セッションタイマーが要求されました。
			if se.Delta < b2buaMinSE {
				// UACはタイマー対応なので、拒否できます。
				log.Printf("Session-Expires %d is too small. Rejecting with 422.", se.Delta)
				extraHeaders := map[string]string{"Min-SE": strconv.Itoa(b2buaMinSE)}
				b.virtualUASTx.Respond(BuildResponse(422, "Session Interval Too Small", virtualUASReq, extraHeaders))
				return
			}
		}
	}

	// 1. 仮想UACレッグ用に新しいINVITEリクエストを作成します。
	virtualUACReq := b.createVirtualUACInvite(virtualUASReq, targetURI)
	if virtualUACReq == nil {
		b.virtualUASTx.Respond(BuildResponse(500, "Server Internal Error", virtualUASReq, nil))
		return
	}

	// 2. 仮想UACレッグのトランスポートを決定します。
	contactURI, err := ParseSIPURI(targetURI)
	if err != nil {
		log.Printf("B2BUA: Invalid target URI %s: %v", targetURI, err)
		b.virtualUASTx.Respond(BuildResponse(400, "Bad Request", virtualUASReq, map[string]string{"Warning": "Invalid target URI"}))
		return
	}
	destPort := "5060"
	if contactURI.Port != "" {
		destPort = contactURI.Port
	}
	destHost := contactURI.Host
	proto := strings.ToUpper(b.virtualUASTx.Transport().GetProto())

	var outboundTransport Transport
	if proto == "UDP" {
		destAddr, err := net.ResolveUDPAddr("udp", destHost+":"+destPort)
		if err != nil {
			log.Printf("B2BUA: Could not resolve destination UDP address '%s': %v", targetURI, err)
			b.virtualUASTx.Respond(BuildResponse(503, "Service Unavailable", virtualUASReq, nil))
			return
		}
		outboundTransport = NewUDPTransport(b.server.udpConn, destAddr)
	} else {
		destAddr, err := net.ResolveTCPAddr("tcp", destHost+":"+destPort)
		if err != nil {
			log.Printf("B2BUA: Could not resolve destination TCP address '%s': %v", targetURI, err)
			b.virtualUASTx.Respond(BuildResponse(503, "Service Unavailable", virtualUASReq, nil))
			return
		}
		conn, err := net.DialTCP("tcp", nil, destAddr)
		if err != nil {
			log.Printf("B2BUA: Could not connect to destination TCP address '%s': %v", targetURI, err)
			b.virtualUASTx.Respond(BuildResponse(503, "Service Unavailable", virtualUASReq, nil))
			return
		}
		outboundTransport = NewTCPTransport(conn)
	}

	// 3. 仮想UACレッグのクライアントトランザクションを作成して実行します。
	virtualUACTx, err := NewInviteClientTx(virtualUACReq, outboundTransport)
	if err != nil {
		log.Printf("B2BUA: Failed to create Virtual-UAC-leg client transaction: %v", err)
		b.virtualUASTx.Respond(BuildResponse(500, "Server Internal Error", virtualUASReq, nil))
		return
	}
	b.mu.Lock()
	b.virtualUACTx = virtualUACTx
	b.mu.Unlock()
	b.server.txManager.Add(virtualUACTx)

	log.Printf("B2BUA: Virtual-UAS-leg tx %s created Virtual-UAC-leg tx %s", b.virtualUASTx.ID(), b.virtualUACTx.ID())

	// 4. 2つのレッグ間を仲介します。
	b.mediate(virtualUACTx, b.virtualUASTx)
}

// createVirtualUACInviteは、仮想UASレッグのリクエストに基づいて仮想UACレッグの新しいINVITEリクエストを作成します。
func (b *B2BUA) createVirtualUACInvite(virtualUASReq *SIPRequest, targetURI string) *SIPRequest {
	virtualUACReq := &SIPRequest{
		Method:  "INVITE",
		URI:     targetURI,
		Proto:   virtualUASReq.Proto,
		Headers: make(map[string]string),
		Body:    virtualUASReq.Body, // SDPボディをそのまま渡す
	}

	// ほとんどのヘッダーをコピーしますが、新しいダイアログ形成ヘッダーを作成します。
	for k, v := range virtualUASReq.Headers {
		lowerK := strings.ToLower(k)
		// B2BUAが自身で作成するダイアログ固有のヘッダーは除外します。
		if lowerK != "from" && lowerK != "to" && lowerK != "call-id" && lowerK != "cseq" &&
			lowerK != "via" && lowerK != "contact" && lowerK != "record-route" && lowerK != "route" {
			virtualUACReq.Headers[k] = v
		}
	}

	// 新しいダイアログ識別子を作成します。
	virtualUACReq.Headers["Call-ID"] = GenerateCallID()

	// 表示名を維持しつつ、新しいタグでFromヘッダーを再構築します。
	fromHeader := virtualUASReq.GetHeader("From")
	// タグを取り除く簡単で堅牢な方法は、";tag="で分割することです。
	fromBase := strings.Split(fromHeader, ";tag=")[0]
	fromTag := GenerateTag()
	virtualUACReq.Headers["From"] = fmt.Sprintf("%s;tag=%s", fromBase, fromTag)

	// Toヘッダーはターゲットに基づいている必要があります
	toURI, err := ParseSIPURI(targetURI)
	if err != nil {
		return nil
	}
	virtualUACReq.Headers["To"] = fmt.Sprintf("<sip:%s@%s>", toURI.User, toURI.Host)

	virtualUACReq.Headers["CSeq"] = "1 INVITE" // CSeqは新しいダイアログに対して独立しています

	// B2BUAのContactヘッダー。
	virtualUACReq.Headers["Contact"] = fmt.Sprintf("<sip:%s>", b.server.listenAddr)

	// トランザクション層は、ブランチIDを決定するためにViaヘッダーが存在することを要求します。
	branch := GenerateBranchID()
	via := fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(b.virtualUASTx.Transport().GetProto()), b.server.listenAddr, branch)
	virtualUACReq.Headers["Via"] = via

	virtualUACReq.Headers["Max-Forwards"] = "69" // Max-Forwardsをデクリメント

	return virtualUACReq
}

// mediateは2つのレッグ間のレスポンスとリクエストのフローを処理します。
func (b *B2BUA) mediate(virtualUACTx ClientTransaction, virtualUASTx ServerTransaction) {
	virtualUASReq := virtualUASTx.OriginalRequest()

	for {
		select {
		// 仮想UACレッグ（被呼者）からのレスポンスを処理します
		case res, ok := <-virtualUACTx.Responses():
			if !ok {
				return
			}
			log.Printf("B2BUA: Received response %d from Virtual-UAC-leg tx %s", res.StatusCode, virtualUACTx.ID())

			// 422 Session Interval Too Smallを再試行して処理します
			if res.StatusCode == 422 {
				minSE, err := res.MinSE()
				if err == nil && minSE > 0 {
					log.Printf("B2BUA: Handling 422, retrying with Min-SE: %d", minSE)
					// 更新されたSession-Expiresで新しいリクエストを作成します
					retryReq := virtualUACTx.OriginalRequest().Clone()
					retryReq.Headers["Session-Expires"] = strconv.Itoa(minSE)

					// CSeqをインクリメント
					cseqStr := retryReq.GetHeader("CSeq")
					if parts := strings.Fields(cseqStr); len(parts) == 2 {
						if cseq, err := strconv.Atoi(parts[0]); err == nil {
							retryReq.Headers["CSeq"] = fmt.Sprintf("%d %s", cseq+1, parts[1])
						}
					}

					// 新しいトランザクションのために新しいViaヘッダーを追加します
					branch := GenerateBranchID()
					via := fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(b.virtualUASTx.Transport().GetProto()), b.server.listenAddr, branch)
					retryReq.Headers["Via"] = via

					// 再試行のために新しい仮想UACレッグクライアントトランザクションを作成して実行します
					newVirtualUACTx, err := NewInviteClientTx(retryReq, virtualUACTx.Transport())
					if err != nil {
						log.Printf("B2BUA: Failed to create retry Virtual-UAC-leg client transaction: %v", err)
						virtualUASTx.Respond(BuildResponse(500, "Server Internal Error", virtualUASReq, nil))
						return
					}
					b.mu.Lock()
					b.virtualUACTx = newVirtualUACTx
					b.mu.Unlock()
					b.server.txManager.Add(newVirtualUACTx)

					// 新しいトランザクションで仲介を続行します
					b.mediate(newVirtualUACTx, virtualUASTx)
					return // この仲介ループを終了します
				}
			}

			// 200 OKを受け取った場合、Session-Expiresヘッダーを自分たちで追加する必要があるかもしれません
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				// セッションタイマー: UASがタイマーをサポートしていない (RFC 4028 セクション 8.1.2)
				// 元のリクエストがタイマーを要求したが、2xxレスポンスにタイマーがない場合、
				// B2BUAはそれを挿入しなければなりません(MUST)。
				if se, _ := virtualUASReq.SessionExpires(); se != nil { // タイマーが要求されました
					if resSE, _ := res.SessionExpires(); resSE == nil { // 2xxレスポンスにタイマーがありません
						log.Printf("B2BUA: UAS did not include Session-Expires in 2xx. Adding it now for Virtual-UAS-leg.")
						res.Headers["Session-Expires"] = fmt.Sprintf("%d;refresher=uac", se.Delta)
						if existing, ok := res.Headers["Require"]; ok {
							res.Headers["Require"] = "timer, " + existing
						} else {
							res.Headers["Require"] = "timer"
						}
					}
				}
			}

			// 仮想UASレッグに対応するレスポンスを作成します。
			virtualUASRes := b.createVirtualUASResponse(virtualUASReq, res)
			virtualUASTx.Respond(virtualUASRes)

			// これが最終レスポンスの場合、INVITEトランザクションは終了です。
			if res.StatusCode >= 200 {
				if res.StatusCode < 300 {
					// 通話は成功し、ダイアログ状態を設定します。
					b.establishDialogs(virtualUASRes, res)

					// 交渉された場合、セッションタイマーをアクティブにします。
					if se, err := virtualUASRes.SessionExpires(); err == nil && se != nil {
						b.server.createOrUpdateSession(virtualUASTx, virtualUASRes, se)
					}

					// ダイアログ内のメッセージを待ちます
					b.waitForDialogMessages()
				}
				return // INVITEトランザクションの仲介を終了します
			}

		// 仮想UASレッグ（発呼者）からのキャンセルを処理します
		case <-virtualUASTx.Done():
			log.Printf("B2BUA: Virtual-UAS-leg tx %s terminated.", virtualUASTx.ID())
			// 仮想UACレッグがまだアクティブな場合は、キャンセルします。
			select {
			case <-virtualUACTx.Done():
				// 仮想UACレッグはすでに終了しており、何もすることはありません。
			default:
				log.Printf("B2BUA: Virtual-UAS-leg terminated, cancelling Virtual-UAC-leg tx %s", virtualUACTx.ID())
				cancelReq := createCancelRequest(virtualUACTx.OriginalRequest())
				virtualUACTx.Transport().Write([]byte(cancelReq.String()))
				virtualUACTx.Terminate()
			}
			return

		case <-time.After(32 * time.Second): // フェイルセーフタイマー
			log.Printf("B2BUA: Timeout waiting for response on Virtual-UAC-leg tx %s", virtualUACTx.ID())
			virtualUASTx.Respond(BuildResponse(408, "Request Timeout", virtualUASReq, nil))
			virtualUACTx.Terminate()
			return
		}
	}
}

// createVirtualUASResponseは、仮想UACレッグのレスポンスに基づいて仮想UASレッグのレスポンスを作成します。
func (b *B2BUA) createVirtualUASResponse(virtualUASReq *SIPRequest, virtualUACRes *SIPResponse) *SIPResponse {
	extraHeaders := make(map[string]string)
	// 仮想UACレッグのレスポンスからダイアログ固有でないすべてのヘッダーをコピーします。
	for k, v := range virtualUACRes.Headers {
		lowerK := strings.ToLower(k)
		switch lowerK {
		case "via", "from", "to", "call-id", "cseq", "contact", "record-route":
			// これらはB2BUAまたはBuildResponseによって管理されます。
		default:
			extraHeaders[k] = v
		}
	}

	// 仮想UACレッグのレスポンスからステータスコードと理由を再利用します。
	virtualUASRes := BuildResponse(virtualUACRes.StatusCode, virtualUACRes.Reason, virtualUASReq, extraHeaders)

	// 仮想UACレッグのレスポンスが2xxだった場合、独自のContactヘッダーとToタグを追加する必要があります。
	if virtualUASRes.StatusCode >= 200 && virtualUASRes.StatusCode < 300 {
		virtualUASRes.Headers["Contact"] = fmt.Sprintf("<sip:%s>", b.server.listenAddr)
		if getTag(virtualUASRes.Headers["To"]) == "" {
			virtualUASRes.Headers["To"] = fmt.Sprintf("%s;tag=%s", virtualUASRes.Headers["To"], GenerateTag())
		}
	}

	// Via、From、To、Call-ID、およびCSeqヘッダーは、BuildResponseによってすでに正しく設定されています。
	return virtualUASRes
}

// establishDialogsは2xxレスポンスの後にダイアログ識別子を保存します。
func (b *B2BUA) establishDialogs(virtualUASRes, virtualUACRes *SIPResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.virtualUASDialogID = getDialogID(
		virtualUASRes.GetHeader("Call-ID"),
		getTag(virtualUASRes.GetHeader("From")),
		getTag(virtualUASRes.GetHeader("To")),
	)
	b.virtualUACDialogID = getDialogID(
		virtualUACRes.GetHeader("Call-ID"),
		getTag(virtualUACRes.GetHeader("From")),
		getTag(virtualUACRes.GetHeader("To")),
	)

	b.server.dialogs.Store(b.virtualUASDialogID, b)
	b.server.dialogs.Store(b.virtualUACDialogID, b)

	log.Printf("B2BUA: Established dialogs. Virtual-UAS-leg: %s, Virtual-UAC-leg: %s", b.virtualUASDialogID, b.virtualUACDialogID)
}

// waitForDialogMessagesは、BYEやre-INVITEなどのダイアログ内リクエストを処理するためのメインループです。
func (b *B2BUA) waitForDialogMessages() {
	// このロジックの部分は、BYE、re-INVITEなどを処理するために実装されます。
	// 現時点では、B2BUAがキャンセルされるまで待機するだけです。
	<-b.done
}

// Cancelは、アップストリームトランザクションがキャンセルされたときに呼び出されます。
func (b *B2BUA) Cancel() {
	log.Printf("B2BUA: Received external cancel for Virtual-UAS-leg tx %s", b.virtualUASTx.ID())

	b.mu.RLock()
	virtualUACTx := b.virtualUACTx
	b.mu.RUnlock()

	// 仮想UACレッグのINVITEトランザクションがまだ処理中の場合、キャンセルします。
	if virtualUACTx != nil {
		select {
		case <-virtualUACTx.Done():
			// トランザクションはすでに終了しており、キャンセルするものはありません。
		default:
			log.Printf("B2BUA: Sending CANCEL to Virtual-UAC-leg tx %s", virtualUACTx.ID())
			cancelReq := createCancelRequest(virtualUACTx.OriginalRequest())
			if _, err := virtualUACTx.Transport().Write([]byte(cancelReq.String())); err != nil {
				log.Printf("B2BUA: Error sending CANCEL to Virtual-UAC-leg: %v", err)
			}
			virtualUACTx.Terminate()
		}
	}

	// 元のINVITEに487で応答します。
	if err := b.virtualUASTx.Respond(BuildResponse(487, "Request Terminated", b.virtualUASTx.OriginalRequest(), nil)); err != nil {
		log.Printf("B2BUA: Error sending 487 response to Virtual-UAS-leg: %v", err)
	}

	b.cleanup()
}

// HandleInDialogRequestは、このB2BUAによって管理されている既存のダイアログに属するリクエストを処理します。
func (b *B2BUA) HandleInDialogRequest(req *SIPRequest, tx ServerTransaction) {
	log.Printf("B2BUA: Handling in-dialog %s for dialog %s", req.Method, b.virtualUASDialogID)

	b.mu.Lock()
	virtualUASDialogID := b.virtualUASDialogID
	b.mu.Unlock()

	// リクエストが仮想UASレッグから来たか仮想UACレッグから来たかを判断します。
	// これは簡略化されたチェックです。堅牢な実装では、両方のダイアログIDをチェックします。
	reqDialogID := getDialogID(req.GetHeader("Call-ID"), getTag(req.GetHeader("From")), getTag(req.GetHeader("To")))

	if reqDialogID == virtualUASDialogID {
		b.handleVirtualUASRequest(req, tx)
	} else {
		b.handleVirtualUACRequest(req, tx)
	}
}

// handleVirtualUASRequestは、発呼者（仮想UASレッグ）からのダイアログ内リクエストを処理します。
func (b *B2BUA) handleVirtualUASRequest(req *SIPRequest, tx ServerTransaction) {
	switch req.Method {
	case "ACK":
		// 200 OKに対するACKが仮想UASレッグで受信されます。仮想UACレッグ用に新しいACKを生成する必要があります。
		log.Printf("B2BUA: Received ACK for Virtual-UAS-leg dialog %s", b.virtualUASDialogID)
		b.mu.Lock()
		b.virtualUASAck = req
		b.mu.Unlock()

		// 仮想UACレッグのACKを作成して送信します
		if b.virtualUACTx != nil && b.virtualUACTx.LastResponse() != nil {
			virtualUACAck := b.createVirtualUACAck(b.virtualUACTx.LastResponse())
			if virtualUACAck != nil {
				b.virtualUACTx.Transport().Write([]byte(virtualUACAck.String()))
				log.Printf("B2BUA: Sent ACK for Virtual-UAC-leg dialog %s", b.virtualUACDialogID)
			}
		}

	case "BYE":
		// 仮想UASレッグからのBYE。仮想UASレッグに200 OKで応答し、次に仮想UACレッグにBYEを送信します。
		log.Printf("B2BUA: Received BYE for Virtual-UAS-leg dialog %s", b.virtualUASDialogID)
		tx.Respond(BuildResponse(200, "OK", req, nil))

		// 仮想UACレッグのBYEを作成して送信します
		virtualUACBye := b.createForwardedRequest(req, b.virtualUACDialogID)
		if virtualUACBye != nil {
			b.sendRequestOnVirtualUACLeg(virtualUACBye)
		}
		b.cleanup()
	}
}

// handleVirtualUACRequestは、被呼者（仮想UACレッグ）からのダイアログ内リクエストを処理します。
// 注意：これらはサーバー上で新しいServerTransactionとして到着します。
func (b *B2BUA) handleVirtualUACRequest(req *SIPRequest, tx ServerTransaction) {
	switch req.Method {
	case "BYE":
		// 仮想UACレッグからのBYE。仮想UACレッグに200 OKで応答し、次に仮想UASレッグにBYEを送信します。
		log.Printf("B2BUA: Received BYE for Virtual-UAC-leg dialog %s", b.virtualUACDialogID)
		tx.Respond(BuildResponse(200, "OK", req, nil))

		// 仮想UASレッグのBYEを作成して送信します
		virtualUASBye := b.createForwardedRequest(req, b.virtualUASDialogID)
		if virtualUASBye != nil {
			// これには、元の発呼者にリクエストを返す必要があります。
			// このロジックには、仮想UASレッグのUAへのクライアントトランザクションを作成する方法が必要です。
			// この部分は複雑であり、当面は簡略化されます。
			log.Printf("B2BUA: Need to send BYE to Virtual-UAS-leg, but client tx to UAC is not implemented yet.")
		}
		b.cleanup()
	}
}

// createVirtualUACAckは、仮想UACレッグの200 OKレスポンスに基づいて仮想UACレッグのACKを作成します。
func (b *B2BUA) createVirtualUACAck(virtualUACRes *SIPResponse) *SIPRequest {
	if virtualUACRes.StatusCode < 200 || virtualUACRes.StatusCode >= 300 {
		return nil
	}

	virtualUACToTag := getTag(virtualUACRes.GetHeader("To"))
	virtualUACFromTag := getTag(virtualUACRes.GetHeader("From"))
	virtualUACCallID := virtualUACRes.GetHeader("Call-ID")
	virtualUACCSeq := virtualUACRes.GetHeader("CSeq")
	virtualUACContact := virtualUACRes.GetHeader("Contact")

	contactURI, err := ParseSIPURI(virtualUACContact)
	if err != nil {
		log.Printf("B2BUA: Could not parse Contact URI from Virtual-UAC-leg 200 OK: %v", err)
		return nil
	}

	ack := &SIPRequest{
		Method: "ACK",
		URI:    contactURI.String(),
		Proto:  "SIP/2.0",
		Headers: map[string]string{
			"Via":        fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(b.virtualUACTx.Transport().GetProto()), b.server.listenAddr, GenerateBranchID()),
			"From":       fmt.Sprintf("%s;tag=%s", b.virtualUACTx.OriginalRequest().GetHeader("From"), virtualUACFromTag),
			"To":         fmt.Sprintf("%s;tag=%s", b.virtualUACTx.OriginalRequest().GetHeader("To"), virtualUACToTag),
			"Call-ID":    virtualUACCallID,
			"CSeq":       strings.Replace(virtualUACCSeq, "INVITE", "ACK", 1),
			"Max-Forwards": "70",
		},
		Body: []byte{},
	}
	return ack
}

// createForwardedRequestは、反対側のレッグ用に新しいリクエストを作成し、
// 重要な情報をコピーしますが、新しいダイアログコンテキストを生成します。
func (b *B2BUA) createForwardedRequest(origReq *SIPRequest, targetDialogID string) *SIPRequest {
	// これは簡略化されたプレースホルダーです。実際の実装では、ヘッダー、CSeq、
	// およびルーティング情報を慎重に管理する必要があります。
	newReq := origReq.Clone()
	newReq.Headers["Call-ID"] = strings.Split(targetDialogID, ":")[0]
	// ... From/Toタグ、CSeqなどについても同様です。
	return newReq
}

// sendRequestOnVirtualUACLegは仮想UACレッグで新しいリクエストを送信します。
func (b *B2BUA) sendRequestOnVirtualUACLeg(req *SIPRequest) {
	// これには、仮想UACレッグ用の新しいクライアントトランザクションを作成する必要があります。
	log.Printf("B2BUA: Sending %s to Virtual-UAC-leg", req.Method)
	b.virtualUACTx.Transport().Write([]byte(req.String()))
}

// cleanupは、サーバーのダイアログおよびトランザクションマップからB2BUAを削除します。
func (b *B2BUA) cleanup() {
	b.mu.Lock()
	defer b.mu.Unlock()

	log.Printf("B2BUA: Cleaning up dialogs %s and %s", b.virtualUASDialogID, b.virtualUACDialogID)

	// ダイアログマップから削除します
	if b.virtualUASDialogID != "" {
		b.server.dialogs.Delete(b.virtualUASDialogID)
	}
	if b.virtualUACDialogID != "" {
		b.server.dialogs.Delete(b.virtualUACDialogID)
	}

	// トランザクションマップから削除します
	if b.virtualUASTx != nil {
		b.server.b2buaMutex.Lock()
		delete(b.server.b2buaByTx, b.virtualUASTx.ID())
		b.server.b2buaMutex.Unlock()
	}

	select {
	case <-b.done:
		// すでに閉じられています
	default:
		close(b.done)
	}
}
