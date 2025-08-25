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
// 2つのコールレッグを制御します: Aレッグ (UACからB2BUA) と Bレッグ (B2BUAからUAS)。
type B2BUA struct {
	server       *SIPServer
	aLegTx       ServerTransaction // Aレッグ: 発呼者から
	bLegTx       ClientTransaction // Bレッグ: 被呼者へ
	aLegDialogID string
	bLegDialogID string
	aLegAck      *SIPRequest
	bLegAck      *SIPRequest
	done         chan bool
	mu           sync.RWMutex
	cancel       func()
	StartTime    time.Time
}

// NewB2BUAは新しいB2BUAインスタンスを作成して初期化します。
func NewB2BUA(s *SIPServer, aLegTx ServerTransaction) *B2BUA {
	return &B2BUA{
		server:    s,
		aLegTx:    aLegTx,
		done:      make(chan bool),
		StartTime: time.Now(),
	}
}

// RunはB2BUAロジックを開始します。Bレッグの作成と、
// 2つのレッグ間のコール状態の管理を担当します。
func (b *B2BUA) Run(targetURI string) {
	log.Printf("B2BUA started for A-leg tx %s, targeting %s", b.aLegTx.ID(), targetURI)
	defer close(b.done)

	aLegReq := b.aLegTx.OriginalRequest()

	// セッションタイマーロジック - RFC 4028のセクション8.1
	if aLegReq.Method == "INVITE" || aLegReq.Method == "UPDATE" {
		const b2buaMinSE = 1800 // 推奨通り30分。

		se, err := aLegReq.SessionExpires()
		if err != nil {
			b.aLegTx.Respond(BuildResponse(400, "Bad Request", aLegReq, map[string]string{"Warning": "Malformed Session-Expires header"}))
			return
		}

		if se != nil { // セッションタイマーが要求されました。
			if se.Delta < b2buaMinSE {
				// UACはタイマー対応なので、拒否できます。
				log.Printf("Session-Expires %d is too small. Rejecting with 422.", se.Delta)
				extraHeaders := map[string]string{"Min-SE": strconv.Itoa(b2buaMinSE)}
				b.aLegTx.Respond(BuildResponse(422, "Session Interval Too Small", aLegReq, extraHeaders))
				return
			}
		}
	}


	// 1. Bレッグ用に新しいINVITEリクエストを作成します。
	bLegReq := b.createBLegInvite(aLegReq, targetURI)
	if bLegReq == nil {
		b.aLegTx.Respond(BuildResponse(500, "Server Internal Error", aLegReq, nil))
		return
	}

	// 2. Bレッグのトランスポートを決定します。
	contactURI, err := ParseSIPURI(targetURI)
	if err != nil {
		log.Printf("B2BUA: Invalid target URI %s: %v", targetURI, err)
		b.aLegTx.Respond(BuildResponse(400, "Bad Request", aLegReq, map[string]string{"Warning": "Invalid target URI"}))
		return
	}
	destPort := "5060"
	if contactURI.Port != "" {
		destPort = contactURI.Port
	}
	destHost := contactURI.Host
	proto := strings.ToUpper(b.aLegTx.Transport().GetProto())

	var outboundTransport Transport
	if proto == "UDP" {
		destAddr, err := net.ResolveUDPAddr("udp", destHost+":"+destPort)
		if err != nil {
			log.Printf("B2BUA: Could not resolve destination UDP address '%s': %v", targetURI, err)
			b.aLegTx.Respond(BuildResponse(503, "Service Unavailable", aLegReq, nil))
			return
		}
		outboundTransport = NewUDPTransport(b.server.udpConn, destAddr)
	} else {
		destAddr, err := net.ResolveTCPAddr("tcp", destHost+":"+destPort)
		if err != nil {
			log.Printf("B2BUA: Could not resolve destination TCP address '%s': %v", targetURI, err)
			b.aLegTx.Respond(BuildResponse(503, "Service Unavailable", aLegReq, nil))
			return
		}
		conn, err := net.DialTCP("tcp", nil, destAddr)
		if err != nil {
			log.Printf("B2BUA: Could not connect to destination TCP address '%s': %v", targetURI, err)
			b.aLegTx.Respond(BuildResponse(503, "Service Unavailable", aLegReq, nil))
			return
		}
		outboundTransport = NewTCPTransport(conn)
	}

	// 3. Bレッグのクライアントトランザクションを作成して実行します。
	bLegTx, err := NewInviteClientTx(bLegReq, outboundTransport)
	if err != nil {
		log.Printf("B2BUA: Failed to create B-leg client transaction: %v", err)
		b.aLegTx.Respond(BuildResponse(500, "Server Internal Error", aLegReq, nil))
		return
	}
	b.mu.Lock()
	b.bLegTx = bLegTx
	b.mu.Unlock()
	b.server.txManager.Add(bLegTx)

	log.Printf("B2BUA: A-leg tx %s created B-leg tx %s", b.aLegTx.ID(), b.bLegTx.ID())

	// 4. 2つのレッグ間を仲介します。
	b.mediate(bLegTx, b.aLegTx)
}

// createBLegInviteは、Aレッグのリクエストに基づいてBレッグの新しいINVITEリクエストを作成します。
func (b *B2BUA) createBLegInvite(aLegReq *SIPRequest, targetURI string) *SIPRequest {
	bLegReq := &SIPRequest{
		Method:  "INVITE",
		URI:     targetURI,
		Proto:   aLegReq.Proto,
		Headers: make(map[string]string),
		Body:    aLegReq.Body, // SDPボディをそのまま渡す
	}

	// ほとんどのヘッダーをコピーしますが、新しいダイアログ形成ヘッダーを作成します。
	for k, v := range aLegReq.Headers {
		lowerK := strings.ToLower(k)
		// B2BUAが自身で作成するダイアログ固有のヘッダーは除外します。
		if lowerK != "from" && lowerK != "to" && lowerK != "call-id" && lowerK != "cseq" &&
			lowerK != "via" && lowerK != "contact" && lowerK != "record-route" && lowerK != "route" {
			bLegReq.Headers[k] = v
		}
	}

	// 新しいダイアログ識別子を作成します。
	bLegReq.Headers["Call-ID"] = GenerateCallID()

	// 表示名を維持しつつ、新しいタグでFromヘッダーを再構築します。
	fromHeader := aLegReq.GetHeader("From")
	// タグを取り除く簡単で堅牢な方法は、";tag="で分割することです。
	fromBase := strings.Split(fromHeader, ";tag=")[0]
	fromTag := GenerateTag()
	bLegReq.Headers["From"] = fmt.Sprintf("%s;tag=%s", fromBase, fromTag)

	// Toヘッダーはターゲットに基づいている必要があります
	toURI, err := ParseSIPURI(targetURI)
	if err != nil {
		return nil
	}
	bLegReq.Headers["To"] = fmt.Sprintf("<sip:%s@%s>", toURI.User, toURI.Host)

	bLegReq.Headers["CSeq"] = "1 INVITE" // CSeqは新しいダイアログに対して独立しています

	// B2BUAのContactヘッダー。
	bLegReq.Headers["Contact"] = fmt.Sprintf("<sip:%s>", b.server.listenAddr)

	// トランザクション層は、ブランチIDを決定するためにViaヘッダーが存在することを要求します。
	branch := GenerateBranchID()
	via := fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(b.aLegTx.Transport().GetProto()), b.server.listenAddr, branch)
	bLegReq.Headers["Via"] = via

	bLegReq.Headers["Max-Forwards"] = "69" // Max-Forwardsをデクリメント

	return bLegReq
}

// mediateは2つのレッグ間のレスポンスとリクエストのフローを処理します。
func (b *B2BUA) mediate(bLegTx ClientTransaction, aLegTx ServerTransaction) {
	aLegReq := aLegTx.OriginalRequest()

	for {
		select {
		// Bレッグ（被呼者）からのレスポンスを処理します
		case res, ok := <-bLegTx.Responses():
			if !ok {
				return
			}
			log.Printf("B2BUA: Received response %d from B-leg tx %s", res.StatusCode, bLegTx.ID())

			// 422 Session Interval Too Smallを再試行して処理します
			if res.StatusCode == 422 {
				minSE, err := res.MinSE()
				if err == nil && minSE > 0 {
					log.Printf("B2BUA: Handling 422, retrying with Min-SE: %d", minSE)
					// 更新されたSession-Expiresで新しいリクエストを作成します
					retryReq := bLegTx.OriginalRequest().Clone()
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
					via := fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(b.aLegTx.Transport().GetProto()), b.server.listenAddr, branch)
					retryReq.Headers["Via"] = via

					// 再試行のために新しいBレッグクライアントトランザクションを作成して実行します
					newBLegTx, err := NewInviteClientTx(retryReq, bLegTx.Transport())
					if err != nil {
						log.Printf("B2BUA: Failed to create retry B-leg client transaction: %v", err)
						aLegTx.Respond(BuildResponse(500, "Server Internal Error", aLegReq, nil))
						return
					}
					b.mu.Lock()
					b.bLegTx = newBLegTx
					b.mu.Unlock()
					b.server.txManager.Add(newBLegTx)

					// 新しいトランザクションで仲介を続行します
					b.mediate(newBLegTx, aLegTx)
					return // この仲介ループを終了します
				}
			}


			// 200 OKを受け取った場合、Session-Expiresヘッダーを自分たちで追加する必要があるかもしれません
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				// セッションタイマー: UASがタイマーをサポートしていない (RFC 4028 セクション 8.1.2)
				// 元のリクエストがタイマーを要求したが、2xxレスポンスにタイマーがない場合、
				// B2BUAはそれを挿入しなければなりません(MUST)。
				if se, _ := aLegReq.SessionExpires(); se != nil { // タイマーが要求されました
					if resSE, _ := res.SessionExpires(); resSE == nil { // 2xxレスポンスにタイマーがありません
						log.Printf("B2BUA: UAS did not include Session-Expires in 2xx. Adding it now for A-leg.")
						res.Headers["Session-Expires"] = fmt.Sprintf("%d;refresher=uac", se.Delta)
						if existing, ok := res.Headers["Require"]; ok {
							res.Headers["Require"] = "timer, " + existing
						} else {
							res.Headers["Require"] = "timer"
						}
					}
				}
			}

			// Aレッグに対応するレスポンスを作成します。
			aLegRes := b.createALegResponse(aLegReq, res)
			aLegTx.Respond(aLegRes)

			// これが最終レスポンスの場合、INVITEトランザクションは終了です。
			if res.StatusCode >= 200 {
				if res.StatusCode < 300 {
					// 通話は成功し、ダイアログ状態を設定します。
					b.establishDialogs(aLegRes, res)

					// 交渉された場合、セッションタイマーをアクティブにします。
					if se, err := aLegRes.SessionExpires(); err == nil && se != nil {
						b.server.createOrUpdateSession(aLegTx, aLegRes, se)
					}

					// ダイアログ内のメッセージを待ちます
					b.waitForDialogMessages()
				}
				return // INVITEトランザクションの仲介を終了します
			}

		// Aレッグ（発呼者）からのキャンセルを処理します
		case <-aLegTx.Done():
			log.Printf("B2BUA: A-leg tx %s terminated.", aLegTx.ID())
			// Bレッグがまだアクティブな場合は、キャンセルします。
			select {
			case <-bLegTx.Done():
				// Bレッグはすでに終了しており、何もすることはありません。
			default:
				log.Printf("B2BUA: A-leg terminated, cancelling B-leg tx %s", bLegTx.ID())
				cancelReq := createCancelRequest(bLegTx.OriginalRequest())
				bLegTx.Transport().Write([]byte(cancelReq.String()))
				bLegTx.Terminate()
			}
			return

		case <-time.After(32 * time.Second): // フェイルセーフタイマー
			log.Printf("B2BUA: Timeout waiting for response on B-leg tx %s", bLegTx.ID())
			aLegTx.Respond(BuildResponse(408, "Request Timeout", aLegReq, nil))
			bLegTx.Terminate()
			return
		}
	}
}

// createALegResponseは、Bレッグのレスポンスに基づいてAレッグのレスポンスを作成します。
func (b *B2BUA) createALegResponse(aLegReq *SIPRequest, bLegRes *SIPResponse) *SIPResponse {
	extraHeaders := make(map[string]string)
	// Bレッグのレスポンスからダイアログ固有でないすべてのヘッダーをコピーします。
	for k, v := range bLegRes.Headers {
		lowerK := strings.ToLower(k)
		switch lowerK {
		case "via", "from", "to", "call-id", "cseq", "contact", "record-route":
			// これらはB2BUAまたはBuildResponseによって管理されます。
		default:
			extraHeaders[k] = v
		}
	}

	// Bレッグのレスポンスからステータスコードと理由を再利用します。
	aLegRes := BuildResponse(bLegRes.StatusCode, bLegRes.Reason, aLegReq, extraHeaders)

	// Bレッグのレスポンスが2xxだった場合、独自のContactヘッダーとToタグを追加する必要があります。
	if aLegRes.StatusCode >= 200 && aLegRes.StatusCode < 300 {
		aLegRes.Headers["Contact"] = fmt.Sprintf("<sip:%s>", b.server.listenAddr)
		if getTag(aLegRes.Headers["To"]) == "" {
			aLegRes.Headers["To"] = fmt.Sprintf("%s;tag=%s", aLegRes.Headers["To"], GenerateTag())
		}
	}

	// Via、From、To、Call-ID、およびCSeqヘッダーは、BuildResponseによってすでに正しく設定されています。
	return aLegRes
}

// establishDialogsは2xxレスポンスの後にダイアログ識別子を保存します。
func (b *B2BUA) establishDialogs(aLegRes, bLegRes *SIPResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.aLegDialogID = getDialogID(
		aLegRes.GetHeader("Call-ID"),
		getTag(aLegRes.GetHeader("From")),
		getTag(aLegRes.GetHeader("To")),
	)
	b.bLegDialogID = getDialogID(
		bLegRes.GetHeader("Call-ID"),
		getTag(bLegRes.GetHeader("From")),
		getTag(bLegRes.GetHeader("To")),
	)

	b.server.dialogs.Store(b.aLegDialogID, b)
	b.server.dialogs.Store(b.bLegDialogID, b)

	log.Printf("B2BUA: Established dialogs. A-leg: %s, B-leg: %s", b.aLegDialogID, b.bLegDialogID)
}

// waitForDialogMessagesは、BYEやre-INVITEなどのダイアログ内リクエストを処理するためのメインループです。
func (b *B2BUA) waitForDialogMessages() {
	// このロジックの部分は、BYE、re-INVITEなどを処理するために実装されます。
	// 現時点では、B2BUAがキャンセルされるまで待機するだけです。
	<-b.done
}

// Cancelは、アップストリームトランザクションがキャンセルされたときに呼び出されます。
func (b *B2BUA) Cancel() {
	log.Printf("B2BUA: Received external cancel for A-leg tx %s", b.aLegTx.ID())

	b.mu.RLock()
	bLegTx := b.bLegTx
	b.mu.RUnlock()

	// BレッグのINVITEトランザクションがまだ処理中の場合、キャンセルします。
	if bLegTx != nil {
		select {
		case <-bLegTx.Done():
			// トランザクションはすでに終了しており、キャンセルするものはありません。
		default:
			log.Printf("B2BUA: Sending CANCEL to B-leg tx %s", bLegTx.ID())
			cancelReq := createCancelRequest(bLegTx.OriginalRequest())
			if _, err := bLegTx.Transport().Write([]byte(cancelReq.String())); err != nil {
				log.Printf("B2BUA: Error sending CANCEL to B-leg: %v", err)
			}
			bLegTx.Terminate()
		}
	}

	// 元のINVITEに487で応答します。
	if err := b.aLegTx.Respond(BuildResponse(487, "Request Terminated", b.aLegTx.OriginalRequest(), nil)); err != nil {
		log.Printf("B2BUA: Error sending 487 response to A-leg: %v", err)
	}

	b.cleanup()
}

// HandleInDialogRequestは、このB2BUAによって管理されている既存のダイアログに属するリクエストを処理します。
func (b *B2BUA) HandleInDialogRequest(req *SIPRequest, tx ServerTransaction) {
	log.Printf("B2BUA: Handling in-dialog %s for dialog %s", req.Method, b.aLegDialogID)

	b.mu.Lock()
	aLegDialogID := b.aLegDialogID
	b.mu.Unlock()

	// リクエストがAレッグから来たかBレッグから来たかを判断します。
	// これは簡略化されたチェックです。堅牢な実装では、両方のダイアログIDをチェックします。
	reqDialogID := getDialogID(req.GetHeader("Call-ID"), getTag(req.GetHeader("From")), getTag(req.GetHeader("To")))

	if reqDialogID == aLegDialogID {
		b.handleALegRequest(req, tx)
	} else {
		b.handleBLegRequest(req, tx)
	}
}

// handleALegRequestは、発呼者（Aレッグ）からのダイアログ内リクエストを処理します。
func (b *B2BUA) handleALegRequest(req *SIPRequest, tx ServerTransaction) {
	switch req.Method {
	case "ACK":
		// 200 OKに対するACKがAレッグで受信されます。Bレッグ用に新しいACKを生成する必要があります。
		log.Printf("B2BUA: Received ACK for A-leg dialog %s", b.aLegDialogID)
		b.mu.Lock()
		b.aLegAck = req
		b.mu.Unlock()

		// BレッグのACKを作成して送信します
		if b.bLegTx != nil && b.bLegTx.LastResponse() != nil {
			bLegAck := b.createBLegAck(b.bLegTx.LastResponse())
			if bLegAck != nil {
				b.bLegTx.Transport().Write([]byte(bLegAck.String()))
				log.Printf("B2BUA: Sent ACK for B-leg dialog %s", b.bLegDialogID)
			}
		}

	case "BYE":
		// AレッグからのBYE。Aレッグに200 OKで応答し、次にBレッグにBYEを送信します。
		log.Printf("B2BUA: Received BYE for A-leg dialog %s", b.aLegDialogID)
		tx.Respond(BuildResponse(200, "OK", req, nil))

		// BレッグのBYEを作成して送信します
		bLegBye := b.createForwardedRequest(req, b.bLegDialogID)
		if bLegBye != nil {
			b.sendRequestOnBLeg(bLegBye)
		}
		b.cleanup()
	}
}

// handleBLegRequestは、被呼者（Bレッグ）からのダイアログ内リクエストを処理します。
// 注意：これらはサーバー上で新しいServerTransactionとして到着します。
func (b *B2BUA) handleBLegRequest(req *SIPRequest, tx ServerTransaction) {
	switch req.Method {
	case "BYE":
		// BレッグからのBYE。Bレッグに200 OKで応答し、次にAレッグにBYEを送信します。
		log.Printf("B2BUA: Received BYE for B-leg dialog %s", b.bLegDialogID)
		tx.Respond(BuildResponse(200, "OK", req, nil))

		// AレッグのBYEを作成して送信します
		aLegBye := b.createForwardedRequest(req, b.aLegDialogID)
		if aLegBye != nil {
			// これには、元の発呼者にリクエストを返す必要があります。
			// このロジックには、AレッグUAへのクライアントトランザクションを作成する方法が必要です。
			// この部分は複雑であり、当面は簡略化されます。
			log.Printf("B2BUA: Need to send BYE to A-leg, but client tx to UAC is not implemented yet.")
		}
		b.cleanup()
	}
}

// createBLegAckは、Bレッグの200 OKレスポンスに基づいてBレッグのACKを作成します。
func (b *B2BUA) createBLegAck(bLegRes *SIPResponse) *SIPRequest {
	if bLegRes.StatusCode < 200 || bLegRes.StatusCode >= 300 {
		return nil
	}

	bLegToTag := getTag(bLegRes.GetHeader("To"))
	bLegFromTag := getTag(bLegRes.GetHeader("From"))
	bLegCallID := bLegRes.GetHeader("Call-ID")
	bLegCSeq := bLegRes.GetHeader("CSeq")
	bLegContact := bLegRes.GetHeader("Contact")

	contactURI, err := ParseSIPURI(bLegContact)
	if err != nil {
		log.Printf("B2BUA: Could not parse Contact URI from B-leg 200 OK: %v", err)
		return nil
	}

	ack := &SIPRequest{
		Method: "ACK",
		URI:    contactURI.String(),
		Proto:  "SIP/2.0",
		Headers: map[string]string{
			"Via":        fmt.Sprintf("SIP/2.0/%s %s;branch=%s", strings.ToUpper(b.bLegTx.Transport().GetProto()), b.server.listenAddr, GenerateBranchID()),
			"From":       fmt.Sprintf("%s;tag=%s", b.bLegTx.OriginalRequest().GetHeader("From"), bLegFromTag),
			"To":         fmt.Sprintf("%s;tag=%s", b.bLegTx.OriginalRequest().GetHeader("To"), bLegToTag),
			"Call-ID":    bLegCallID,
			"CSeq":       strings.Replace(bLegCSeq, "INVITE", "ACK", 1),
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

// sendRequestOnBLegはBレッグで新しいリクエストを送信します。
func (b *B2BUA) sendRequestOnBLeg(req *SIPRequest) {
	// これには、Bレッグ用の新しいクライアントトランザクションを作成する必要があります。
	log.Printf("B2BUA: Sending %s to B-leg", req.Method)
	b.bLegTx.Transport().Write([]byte(req.String()))
}

// cleanupは、サーバーのダイアログおよびトランザクションマップからB2BUAを削除します。
func (b *B2BUA) cleanup() {
	b.mu.Lock()
	defer b.mu.Unlock()

	log.Printf("B2BUA: Cleaning up dialogs %s and %s", b.aLegDialogID, b.bLegDialogID)

	// ダイアログマップから削除します
	if b.aLegDialogID != "" {
		b.server.dialogs.Delete(b.aLegDialogID)
	}
	if b.bLegDialogID != "" {
		b.server.dialogs.Delete(b.bLegDialogID)
	}

	// トランザクションマップから削除します
	if b.aLegTx != nil {
		b.server.b2buaMutex.Lock()
		delete(b.server.b2buaByTx, b.aLegTx.ID())
		b.server.b2buaMutex.Unlock()
	}


	select {
	case <-b.done:
		// すでに閉じられています
	default:
		close(b.done)
	}
}
