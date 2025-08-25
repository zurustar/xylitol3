package sip

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sip-server/internal/storage"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Registration は、ユーザーの登録された場所の情報を保持します。
type Registration struct {
	ContactURI string
	ExpiresAt  time.Time
}

// SessionState は、アクティブなセッションタイマーを持つセッションの状態を保持します。
type SessionState struct {
	DialogID  string
	Interval  int
	ExpiresAt time.Time
	Refresher string // "uac" または "uas"
	timer     *time.Timer
}

// SIPServer は、SIPサーバーの依存関係を保持します。
type SIPServer struct {
	storage       *storage.Storage
	txManager     *TransactionManager
	registrations map[string][]Registration
	regMutex      sync.RWMutex
	nonces        map[string]time.Time
	nonceMutex    sync.Mutex
	realm         string
	listenAddr    string // このサーバーがリッスンしているアドレス（例: "1.2.3.4:5060"）
	sessions      map[string]*SessionState
	sessionMutex  sync.RWMutex
	dialogs       sync.Map          // アクティブなB2BUAダイアログをダイアログIDでキー付けして保存します
	b2buaByTx     map[string]*B2BUA // AレッグのトランザクションIDでキー付け
	b2buaMutex    sync.RWMutex
	udpConn       net.PacketConn
	tcpListener   *net.TCPListener

	// ガイダンス設定
	guidanceUser      string
	guidanceAudioFile string
	guidanceMutex     sync.RWMutex
}

// NewSIPServer は、新しいSIPサーバーインスタンスを作成します。
func NewSIPServer(s *storage.Storage, realm string, guidanceUser string, guidanceAudioFile string) *SIPServer {
	return &SIPServer{
		storage:           s,
		txManager:         NewTransactionManager(),
		registrations:     make(map[string][]Registration),
		nonces:            make(map[string]time.Time),
		realm:             realm,
		sessions:          make(map[string]*SessionState),
		b2buaByTx:         make(map[string]*B2BUA),
		guidanceUser:      guidanceUser,
		guidanceAudioFile: guidanceAudioFile,
	}
}

// UpdateGuidanceSettings は、ガイダンス設定を安全に更新します。
func (s *SIPServer) UpdateGuidanceSettings(user, audioFile string) {
	s.guidanceMutex.Lock()
	defer s.guidanceMutex.Unlock()
	s.guidanceUser = user
	s.guidanceAudioFile = audioFile
	log.Printf("Updated guidance settings. User: %s, AudioFile: %s", user, audioFile)
}

// GetGuidanceSettings は、現在のガイダンス設定を安全に取得します。
func (s *SIPServer) GetGuidanceSettings() (string, string) {
	s.guidanceMutex.RLock()
	defer s.guidanceMutex.RUnlock()
	return s.guidanceUser, s.guidanceAudioFile
}

// NewClientTx は、リクエストメソッドに基づいて新しいクライアントトランザクションを作成します。
func (s *SIPServer) NewClientTx(req *SIPRequest, transport Transport) (ClientTransaction, error) {
	var clientTx ClientTransaction
	var err error
	switch req.Method {
	case "INVITE", "UPDATE":
		clientTx, err = NewInviteClientTx(req, transport)
	default:
		clientTx, err = NewNonInviteClientTx(req, transport)
	}
	if err != nil {
		return nil, err
	}
	s.txManager.Add(clientTx)
	return clientTx, nil
}

// getDialogID は、ダイアログの一意の識別子を生成します。
// タグの順序はルックアップにとって重要です。
func getDialogID(callID, fromTag, toTag string) string {
	return fmt.Sprintf("%s:%s:%s", callID, fromTag, toTag)
}

// Run は、SIPサーバーを起動し、UDPとTCPの両方のポートでリッスンします。
func (s *SIPServer) Run(ctx context.Context, addr string) error {
	g, gCtx := errgroup.WithContext(ctx)

	// ログ記録のために共通のホスト/ポートを取得するためにアドレスを解決します。
	// ベースラインとしてUDPアドレス解決を使用します。
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("could not resolve UDP address: %w", err)
	}
	s.listenAddr = udpAddr.String()
	if udpAddr.IP.IsUnspecified() {
		// 0.0.0.0でリッスンしている場合、ヘッダーにはループバックアドレスを使用します。
		// これはローカルテスト用と想定しています。実際のデプロイでは、
		// パブリックIPを設定する必要があります。
		s.listenAddr = fmt.Sprintf("127.0.0.1:%d", udpAddr.Port)
	}

	// --- UDPリスナー ---
	g.Go(func() error {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return fmt.Errorf("could not listen on UDP: %w", err)
		}
		defer pc.Close()
		s.udpConn = pc
		log.Printf("SIP server listening on UDP %s", pc.LocalAddr())

		go func() {
			<-gCtx.Done()
			pc.Close()
		}()

		buf := make([]byte, 4096)
		for {
			n, clientAddr, err := pc.ReadFrom(buf)
			if err != nil {
				if gCtx.Err() != nil {
					return nil // 正常なシャットダウン
				}
				log.Printf("Error reading from UDP: %v", err)
				continue
			}

			message := string(buf[:n])
			transport := NewUDPTransport(pc, clientAddr)
			go s.dispatchMessage(transport, message)
		}
	})

	// --- TCPリスナー ---
	g.Go(func() error {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("could not listen on TCP: %w", err)
		}
		defer listener.Close()
		s.tcpListener = listener.(*net.TCPListener)
		log.Printf("SIP server listening on TCP %s", listener.Addr())

		go func() {
			<-gCtx.Done()
			listener.Close()
		}()

		for {
			conn, err := listener.Accept()
			if err != nil {
				if gCtx.Err() != nil {
					return nil // 正常なシャットダウン
				}
				log.Printf("Error accepting TCP connection: %v", err)
				continue
			}
			go s.handleConnection(gCtx, conn)
		}
	})

	log.Printf("SIP server started with UDP and TCP listeners on %s", s.listenAddr)
	return g.Wait()
}

// handleConnection は、TCP接続からSIPメッセージをループで読み取り、それらをディスパッチします。
func (s *SIPServer) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	transport := NewTCPTransport(conn)
	reader := bufio.NewReader(conn)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var headers strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					log.Printf("Error reading from TCP connection %s: %v", conn.RemoteAddr(), err)
				}
				return
			}
			headers.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		headerStr := headers.String()

		contentLength := parseContentLength(headerStr)
		body := make([]byte, contentLength)
		if contentLength > 0 {
			if _, err := io.ReadFull(reader, body); err != nil {
				log.Printf("Error reading body from TCP connection %s: %v", conn.RemoteAddr(), err)
				return
			}
		}

		message := headerStr + string(body)
		go s.dispatchMessage(transport, message)
	}
}

// parseContentLength は、SIPヘッダーからContent-Length値を抽出するためのヘルパーです。
func parseContentLength(headerStr string) int {
	lines := strings.Split(headerStr, "\r\n")
	for _, line := range lines {
		lowerLine := strings.ToLower(line)
		if strings.HasPrefix(lowerLine, "content-length:") || strings.HasPrefix(lowerLine, "l:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				lengthStr := strings.TrimSpace(parts[1])
				length, err := strconv.Atoi(lengthStr)
				if err == nil {
					return length
				}
			}
		}
	}
	return 0
}

// dispatchMessage は、受信メッセージがリクエストかレスポンスかを判断し、
// 適切なハンドラーにルーティングします。
func (s *SIPServer) dispatchMessage(transport Transport, rawMsg string) {
	// 最初の行を覗き見て、リクエストかレスポンスかを確認します。
	// リクエストはメソッド（例：INVITE、ACK）で始まり、レスポンスは「SIP/2.0」で始まります。
	if strings.HasPrefix(rawMsg, "SIP/2.0") {
		s.handleResponse(transport, rawMsg)
	} else {
		s.handleRequest(transport, rawMsg)
	}
}

// handleResponse は、受信したSIPレスポンスを解析し、
// 適切なクライアントトランザクションに渡します。
func (s *SIPServer) handleResponse(transport Transport, rawMsg string) {
	res, err := ParseSIPResponse(rawMsg)
	if err != nil {
		log.Printf("Error parsing SIP response: %v", err)
		return
	}

	topVia, err := res.TopVia()
	if err != nil {
		log.Printf("Could not get Via from response: %v", err)
		return
	}
	branchID := topVia.Branch()

	tx, ok := s.txManager.Get(branchID)
	if !ok {
		log.Printf("No matching client transaction for response with branch %s", branchID)
		return
	}

	if clientTx, ok := tx.(ClientTransaction); ok {
		log.Printf("Passing response to client transaction %s", branchID)
		clientTx.ReceiveResponse(res)
	} else {
		log.Printf("Transaction %s is not a client transaction", branchID)
	}
}

// handleRequest は、すべての受信SIPリクエストのエントリポイントです。
// トランザクションを管理し、リクエストを適切なハンドラーにディスパッチします。
func (s *SIPServer) handleRequest(transport Transport, rawMsg string) {
	req, err := ParseSIPRequest(rawMsg)
	if err != nil {
		log.Printf("Error parsing SIP request: %v", err)
		return
	}

	// RFC 3261 セクション 16.3 アイテム 3: ループ検出
	vias, err := req.AllVias()
	if err != nil {
		log.Printf("Error parsing Via headers: %v", err)
		// ループについては確信が持てませんが、リクエストは不正な形式である可能性が高いです。
		// とりあえず、それがクリティカルであれば後で失敗させます。
	} else {
		listenHost, listenPort, _ := net.SplitHostPort(s.listenAddr)
		if listenHost == "" {
			listenHost = s.listenAddr // Fallback if port is missing, though it shouldn't be.
		}

		for _, via := range vias {
			// Viaヘッダーのホストとポートを、私たち自身のリッスンアドレスと比較します。
			viaPort := via.Port
			if viaPort == "" {
				// RFC 3261 セクション 18.2.1 によると、ポートがない場合、デフォルトは
				// sipの場合は5060、sipsの場合は5061です。ここではsipのみを扱います。
				viaPort = "5060"
			}

			if via.Host == listenHost && viaPort == listenPort {
				log.Printf("Loop detected: request Via header contains this proxy's address (%s).", s.listenAddr)
				res := BuildResponse(482, "Loop Detected", req, nil)
				if _, err := transport.Write([]byte(res.String())); err != nil {
					log.Printf("Error sending 482 Loop Detected response: %v", err)
				}
				return // 処理を停止
			}
		}
	}

	// ダイアログ内リクエスト（最初のINVITEを除く）の場合、B2BUAを見つける必要があります。
	if req.Method != "INVITE" {
		// これは簡略化されたダイアログルックアップです。堅牢な実装では、ルーズルーティングを処理します。
		dialogID := getDialogID(req.GetHeader("Call-ID"), GetTag(req.GetHeader("From")), GetTag(req.GetHeader("To")))
		if val, ok := s.dialogs.Load(dialogID); ok {
			b2bua := val.(*B2BUA)
			// これらのリクエストは、B2BUAによってステートフルに処理されるためにトランザクションを必要とします。
			srvTx, err := NewNonInviteServerTx(req, transport)
			if err != nil {
				log.Printf("Error creating server transaction for in-dialog request: %v", err)
				return
			}
			s.txManager.Add(srvTx)
			b2bua.HandleInDialogRequest(req, srvTx)
			return
		}
	}

	// RFC 3261によると、CANCELは特別なホップバイホップのリクエストです。
	if req.Method == "CANCEL" {
		s.handleCancel(transport, req)
		return
	}

	// 2xx以外のレスポンスに対するACKには、まだダイアログがない場合があります。
	if req.Method == "ACK" {
		topVia, err := req.TopVia()
		if err == nil {
			if tx, ok := s.txManager.Get(topVia.Branch()); ok {
				if srvTx, ok := tx.(*InviteServerTx); ok {
					log.Printf("Passing ACK for non-2xx response to Invite Server Tx %s", srvTx.ID())
					srvTx.Receive(req)
					return
				}
			}
		}
		log.Printf("Received ACK that did not match any transaction or dialog. Dropping.")
		return
	}


	topVia, err := req.TopVia()
	if err != nil {
		log.Printf("Could not get Via header: %v", err)
		return
	}
	branchID := topVia.Branch()

	if !strings.HasPrefix(branchID, RFC3261BranchMagicCookie) {
		log.Printf("Received request with non-RFC3261 branch ID: %s. Handling statelessly.", branchID)
		s.handleStateless(transport, req)
		return
	}

	// 再送を確認します。
	if tx, ok := s.txManager.Get(branchID); ok {
		log.Printf("Retransmission of %s received for transaction %s", req.Method, branchID)
		if srvTx, ok := tx.(ServerTransaction); ok {
			srvTx.Receive(req)
		}
		return
	}

	// 新しいリクエストの場合は、新しいトランザクションを作成します。
	log.Printf("New request received, creating transaction %s for method %s", branchID, req.Method)
	var srvTx ServerTransaction
	switch req.Method {
	case "REGISTER":
		srvTx, err = NewNonInviteServerTx(req, transport)
	case "INVITE", "UPDATE":
		srvTx, err = NewInviteServerTx(req, transport)
	case "BYE":
		srvTx, err = NewNonInviteServerTx(req, transport)
	default:
		log.Printf("Handling method %s statelessly.", req.Method)
		s.handleStateless(transport, req)
		return
	}

	if err != nil {
		log.Printf("Error creating server transaction: %v", err)
		return
	}
	s.txManager.Add(srvTx)
	go s.handleTransaction(srvTx)
}

// handleTransaction は、サーバートランザクションのトランザクションユーザー（TU）です。
func (s *SIPServer) handleTransaction(srvTx ServerTransaction) {
	select {
	case req := <-srvTx.Requests():
		switch req.Method {
		case "REGISTER":
			res := s.handleRegister(req)
			if err := srvTx.Respond(res); err != nil {
				log.Printf("Error sending response to transaction %s: %v", srvTx.ID(), err)
			}
		case "INVITE", "UPDATE":
			s.handleInvite(req, srvTx)
		default:
			// BYEおよびその他のダイアログ内メソッドはhandleRequestで処理されます
			res := BuildResponse(405, "Method Not Allowed", req, nil)
			if err := srvTx.Respond(res); err != nil {
				log.Printf("Error sending response to transaction %s: %v", srvTx.ID(), err)
			}
		}
	case <-srvTx.Done():
	}
}

// handleInvite は、新しいコールのためにB2BUAを開始するか、
// 既存のダイアログにre-INVITEを渡すかを決定します。
func (s *SIPServer) handleInvite(req *SIPRequest, tx ServerTransaction) {
	// re-INVITEにはToタグがあります。最初のINVITEにはありません。
	if GetTag(req.GetHeader("To")) != "" {
		dialogID := getDialogID(req.GetHeader("Call-ID"), GetTag(req.GetHeader("From")), GetTag(req.GetHeader("To")))
		if val, ok := s.dialogs.Load(dialogID); ok {
			b2bua := val.(*B2BUA)
			log.Printf("Passing re-INVITE to existing B2BUA for dialog %s", dialogID)
			b2bua.HandleInDialogRequest(req, tx)
			return
		}
		log.Printf("Received re-INVITE for unknown dialog %s. Rejecting.", dialogID)
		tx.Respond(BuildResponse(481, "Call/Transaction Does Not Exist", req, nil))
		return
	}

	// これは最初のINVITEなので、新しいB2BUAをセットアップします。
	toURI, err := req.GetSIPURIHeader("To")
	if err != nil {
		tx.Respond(BuildResponse(400, "Bad Request", req, map[string]string{"Warning": "Malformed To header"}))
		return
	}

	// ガイダンス設定を安全に読み取ります
	s.guidanceMutex.RLock()
	guidanceUser := s.guidanceUser
	guidanceAudioFile := s.guidanceAudioFile
	s.guidanceMutex.RUnlock()

	// ガイダンスユーザーへの呼び出しを確認します
	if guidanceUser != "" && toURI.User == guidanceUser {
		log.Printf("Call to guidance user '%s'. Starting guidance app with audio '%s'.", toURI.User, guidanceAudioFile)
		app := NewApp(s, tx, guidanceAudioFile)
		go app.Run()
		return
	}

	if toURI.Host != s.realm {
		log.Printf("Request for unknown realm: %s", toURI.Host)
		tx.Respond(BuildResponse(404, "Not Found", req, nil))
		return
	}

	s.regMutex.RLock()
	registrations, ok := s.registrations[toURI.User]
	s.regMutex.RUnlock()

	activeRegistrations := []Registration{}
	if ok {
		s.regMutex.Lock()
		now := time.Now()
		for _, reg := range registrations {
			if now.Before(reg.ExpiresAt) {
				activeRegistrations = append(activeRegistrations, reg)
			}
		}
		if len(activeRegistrations) > 0 {
			s.registrations[toURI.User] = activeRegistrations
		} else {
			delete(s.registrations, toURI.User)
		}
		s.regMutex.Unlock()
	}

	if len(activeRegistrations) == 0 {
		log.Printf("User '%s' not registered or all contacts expired", toURI.User)
		tx.Respond(BuildResponse(480, "Temporarily Unavailable", req, nil))
		return
	}

	targetContact := activeRegistrations[0].ContactURI
	log.Printf("Found contact for user %s: %s. Setting up B2BUA.", toURI.User, targetContact)

	b2bua := NewB2BUA(s, tx)
	s.b2buaMutex.Lock()
	s.b2buaByTx[tx.ID()] = b2bua
	s.b2buaMutex.Unlock()

	// これ以降、このトランザクションはB2BUAが担当します。
	go b2bua.Run(targetContact)
}

func (s *SIPServer) handleStateless(transport Transport, req *SIPRequest) {
	response := BuildResponse(405, "Method Not Allowed", req, nil)
	if _, err := transport.Write([]byte(response.String())); err != nil {
		log.Printf("Error sending 405 response: %v", err)
	}
}

func (s *SIPServer) handleRegister(req *SIPRequest) *SIPResponse {
	log.Printf("Handling REGISTER for %s", req.GetHeader("To"))

	if len(req.Authorization) == 0 {
		return s.createUnauthorizedResponse(req)
	}

	auth := req.Authorization
	username, okU := auth["username"]
	nonce, okN := auth["nonce"]
	uri, okR := auth["uri"]
	response, okResp := auth["response"]

	if !okU || !okN || !okR || !okResp {
		return BuildResponse(400, "Bad Request", req, map[string]string{"Warning": "Missing digest parameters"})
	}

	if !s.validateNonce(nonce) {
		log.Printf("Invalid or expired nonce for user %s", username)
		return s.createUnauthorizedResponse(req)
	}

	user, err := s.storage.GetUserByUsername(username)
	if err != nil || user == nil {
		log.Printf("Auth failed for user '%s': not found or db error", username)
		return s.createUnauthorizedResponse(req)
	}

	expectedResponse := CalculateResponse(user.Password, req.Method, uri, nonce)
	if response != expectedResponse {
		log.Printf("Auth failed for user '%s': incorrect password", username)
		return s.createUnauthorizedResponse(req)
	}

	log.Printf("User '%s' authenticated successfully", username)
	s.updateRegistration(req)

	return s.createOKResponse(req)
}

func (s *SIPServer) updateRegistration(req *SIPRequest) {
	s.regMutex.Lock()
	defer s.regMutex.Unlock()

	username := req.Authorization["username"]
	contactHeader := req.GetHeader("Contact")
	expires := req.Expires()

	// ワイルドカードによる登録解除を処理します
	if contactHeader == "*" && expires == 0 {
		delete(s.registrations, username)
		log.Printf("Unregistered all contacts for user '%s'", username)
		return
	}

	// RFC 3261によると、REGISTERはカンマ区切りのリストで複数のコンタクトを持つことができます。
	contactHeaders := strings.Split(contactHeader, ",")

	for _, singleContact := range contactHeaders {
		trimmedContact := strings.TrimSpace(singleContact)
		start := strings.Index(trimmedContact, "<")
		end := strings.Index(trimmedContact, ">")
		var contactURI string
		if start != -1 && end != -1 {
			contactURI = trimmedContact[start+1 : end]
		} else {
			contactURI = trimmedContact
		}

		if expires > 0 {
			s.addOrUpdateContact(username, contactURI, expires)
		} else {
			s.removeContact(username, contactURI)
		}
	}
}

func (s *SIPServer) addOrUpdateContact(username, contactURI string, expires int) {
	existingRegs, ok := s.registrations[username]
	if !ok {
		existingRegs = []Registration{}
	}

	found := false
	for i := range existingRegs {
		if existingRegs[i].ContactURI == contactURI {
			existingRegs[i].ExpiresAt = time.Now().Add(time.Duration(expires) * time.Second)
			found = true
			log.Printf("Updated registration for user '%s' at %s, expires in %d seconds", username, contactURI, expires)
			break
		}
	}

	if !found {
		newReg := Registration{
			ContactURI: contactURI,
			ExpiresAt:  time.Now().Add(time.Duration(expires) * time.Second),
		}
		existingRegs = append(existingRegs, newReg)
		log.Printf("Added new registration for user '%s' at %s, expires in %d seconds", username, contactURI, expires)
	}

	s.registrations[username] = existingRegs
}

func (s *SIPServer) removeContact(username, contactURI string) {
	existingRegs, ok := s.registrations[username]
	if !ok {
		return
	}

	updatedRegs := []Registration{}
	for _, reg := range existingRegs {
		if reg.ContactURI != contactURI {
			updatedRegs = append(updatedRegs, reg)
		} else {
			log.Printf("Removed registration for user '%s' at %s", username, contactURI)
		}
	}

	if len(updatedRegs) > 0 {
		s.registrations[username] = updatedRegs
	} else {
		delete(s.registrations, username)
	}
}

func (s *SIPServer) createOKResponse(req *SIPRequest) *SIPResponse {
	headers := map[string]string{
		"Contact": fmt.Sprintf("%s;expires=%d", req.GetHeader("Contact"), req.Expires()),
		"Date":    time.Now().UTC().Format(time.RFC1123),
	}
	return BuildResponse(200, "OK", req, headers)
}

func (s *SIPServer) createUnauthorizedResponse(req *SIPRequest) *SIPResponse {
	nonce, _ := s.generateNonce()
	authValue := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5, qop="auth"`, s.realm, nonce)
	headers := map[string]string{
		"WWW-Authenticate": authValue,
	}
	return BuildResponse(401, "Unauthorized", req, headers)
}

func (s *SIPServer) generateNonce() (string, error) {
	nonce := GenerateNonce(8)
	s.nonceMutex.Lock()
	defer s.nonceMutex.Unlock()
	s.nonces[nonce] = time.Now().Add(5 * time.Minute)
	return nonce, nil
}

func (s *SIPServer) validateNonce(nonce string) bool {
	s.nonceMutex.Lock()
	defer s.nonceMutex.Unlock()

	expiration, ok := s.nonces[nonce]
	if !ok {
		return false
	}

	if time.Now().After(expiration) {
		delete(s.nonces, nonce)
		return false
	}

	delete(s.nonces, nonce)
	return true
}

func (s *SIPServer) handleCancel(transport Transport, req *SIPRequest) {
	log.Printf("Handling CANCEL request for Call-ID %s", req.GetHeader("Call-ID"))

	// RFC 3261に従い、CANCELリクエストに即座に200 OKで応答します。
	res200 := BuildResponse(200, "OK", req, nil)
	if _, err := transport.Write([]byte(res200.String())); err != nil {
		log.Printf("Error sending 200 OK for CANCEL: %v", err)
	}

	topVia, err := req.TopVia()
	if err != nil {
		log.Printf("Could not get Via from CANCEL request: %v", err)
		return
	}
	branchID := topVia.Branch()

	s.b2buaMutex.RLock()
	b2bua, ok := s.b2buaByTx[branchID]
	s.b2buaMutex.RUnlock()

	if ok {
		log.Printf("Found B2BUA for tx %s, telling it to cancel.", branchID)
		b2bua.Cancel()
	} else {
		log.Printf("Could not find a B2BUA for CANCEL with branch ID %s. The INVITE transaction might have already completed.", branchID)
	}
}

func createCancelRequest(inviteReq *SIPRequest) *SIPRequest {
	cancelReq := &SIPRequest{
		Method:  "CANCEL",
		URI:     inviteReq.URI,
		Proto:   inviteReq.Proto,
		Headers: make(map[string]string),
		Body:    []byte{},
	}
	// RFC 3261 セクション 16.3 に従って、必須ヘッダーをコピーします
	for _, h := range []string{"From", "To", "Call-ID", "Via"} {
		cancelReq.Headers[h] = inviteReq.Headers[h]
	}

	// CSeq番号は同じでなければなりませんが、メソッドはCANCELです
	cseqStr := inviteReq.GetHeader("CSeq")
	if parts := strings.SplitN(cseqStr, " ", 2); len(parts) == 2 {
		cancelReq.Headers["CSeq"] = parts[0] + " CANCEL"
	} else {
		// 有効なリクエストではこれは起こらないはずです
		cancelReq.Headers["CSeq"] = "1 CANCEL"
	}
	cancelReq.Headers["Max-Forwards"] = "70"
	return cancelReq
}

// GetTag は、ヘッダー値からtagパラメータを抽出します。
func GetTag(headerValue string) string {
	parts := strings.Split(headerValue, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "tag=") {
			return strings.TrimPrefix(part, "tag=")
		}
	}
	return ""
}

func (s *SIPServer) createOrUpdateSession(upstreamTx ServerTransaction, finalResponse *SIPResponse, se *SessionExpires) {
	if se == nil || se.Refresher == "" {
		return
	}

	callID := finalResponse.GetHeader("Call-ID")
	fromTag := GetTag(finalResponse.GetHeader("From"))
	toTag := GetTag(finalResponse.GetHeader("To"))

	if callID == "" || fromTag == "" || toTag == "" {
		log.Printf("Cannot create session state: missing Call-ID or tags in 2xx response.")
		return
	}

	dialogID := getDialogID(callID, fromTag, toTag)

	s.sessionMutex.Lock()
	defer s.sessionMutex.Unlock()

	if existingSession, ok := s.sessions[dialogID]; ok {
		existingSession.timer.Stop()
	}

	session := &SessionState{
		DialogID:  dialogID,
		Interval:  se.Delta,
		ExpiresAt: time.Now().Add(time.Duration(se.Delta) * time.Second),
		Refresher: se.Refresher,
	}

	log.Printf("Session timer established for dialog %s. Interval: %d, Refresher: %s", dialogID, session.Interval, session.Refresher)

	session.timer = time.AfterFunc(time.Duration(session.Interval)*time.Second, func() {
		s.expireSession(dialogID)
	})

	s.sessions[dialogID] = session
}

func (s *SIPServer) expireSession(dialogID string) {
	s.sessionMutex.Lock()
	defer s.sessionMutex.Unlock()

	if _, ok := s.sessions[dialogID]; ok {
		log.Printf("Session timer expired for dialog %s. Removing state.", dialogID)
		delete(s.sessions, dialogID)
	}
}

// SessionInfo は、表示目的でアクティブなセッションのスナップショットを提供します。
type SessionInfo struct {
	Caller   string
	Callee   string
	Duration time.Duration
	CallID   string
}

// GetActiveSessions は、すべてのアクティブなダイアログのSessionInfoのスライスを返します。
func (s *SIPServer) GetActiveSessions() []SessionInfo {
	var sessions []SessionInfo
	s.dialogs.Range(func(key, value interface{}) bool {
		b2bua, ok := value.(*B2BUA)
		if !ok {
			return true // 続行
		}

		b2bua.mu.RLock()
		defer b2bua.mu.RUnlock()

		if b2bua.virtualUASTx == nil || b2bua.virtualUASTx.OriginalRequest() == nil {
			return true
		}

		// 同じセッションを2回表示しないようにするため（Virtual-UAS-legとVirtual-UAC-legの両方のダイアログIDで保存しているため）、
		// Virtual-UAS-legのIDで反復処理している場合にのみアクションを実行します。
		// また、ダイアログが完全に確立されていることも確認します。
		if b2bua.virtualUASDialogID == "" || key.(string) != b2bua.virtualUASDialogID {
			return true
		}

		origReq := b2bua.virtualUASTx.OriginalRequest()
		info := SessionInfo{
			Caller:   origReq.GetHeader("From"),
			Callee:   origReq.GetHeader("To"),
			Duration: time.Since(b2bua.StartTime).Round(time.Second),
			CallID:   origReq.GetHeader("Call-ID"),
		}
		sessions = append(sessions, info)

		return true // 反復処理を続行
	})
	return sessions
}

// ListenAddr は、サーバーのリッスンアドレスを返します。
func (s *SIPServer) ListenAddr() string {
	return s.listenAddr
}

// TransportFor は、指定された宛先へのトランスポートを返します。
// 現在、UDPのみをサポートしています。
func (s *SIPServer) TransportFor(host, port, proto string) (Transport, error) {
	if strings.ToUpper(proto) != "UDP" {
		return nil, fmt.Errorf("transport protocol %s not supported", proto)
	}

	destAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("could not resolve destination UDP address '%s:%s': %w", host, port, err)
	}

	// 既存のUDP接続を再利用します
	return NewUDPTransport(s.udpConn, destAddr), nil
}
