package sip

import (
	"fmt"
	"strconv"
	"strings"
)

// SIPResponse は、SIPレスポンスメッセージを表します。
type SIPResponse struct {
	Proto      string
	StatusCode int
	Reason     string
	Headers    map[string]string
	Body       []byte
}

// String は、SIPレスポンスの文字列表現を返します。
func (r *SIPResponse) String() string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("%s %d %s\r\n", r.Proto, r.StatusCode, r.Reason))
	for key, value := range r.Headers {
		// マップからContent-Lengthを無視し、Bodyに基づいて追加します。
		if strings.Title(key) != "Content-Length" {
			builder.WriteString(fmt.Sprintf("%s: %s\r\n", strings.Title(key), value))
		}
	}
	builder.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(r.Body)))
	builder.WriteString("\r\n")
	builder.Write(r.Body)
	return builder.String()
}

// GetHeader は、ヘッダーの値を返します。名前は正規の形式であることを期待します。
func (r *SIPResponse) GetHeader(name string) string {
	return r.Headers[strings.Title(name)]
}

// SessionExpires は、レスポンスからSession-Expiresヘッダーを解析します。
func (r *SIPResponse) SessionExpires() (*SessionExpires, error) {
	seHeader := r.GetHeader("Session-Expires")
	if seHeader == "" {
		// コンパクト形式 'x' もチェックします
		seHeader = r.GetHeader("x")
	}
	if seHeader == "" {
		return nil, nil // エラーではなく、ヘッダーが存在しないだけです
	}
	return ParseSessionExpires(seHeader)
}

// MinSE は、レスポンスからMin-SEヘッダーを解析します。見つからない場合は-1を返します。
func (r *SIPResponse) MinSE() (int, error) {
	minSEHeader := r.GetHeader("Min-SE")
	if minSEHeader == "" {
		return -1, nil // エラーではなく、ヘッダーが存在しないだけです
	}
	// Min-SEにはdelta-seconds値しかありません
	minSE, err := strconv.Atoi(strings.TrimSpace(minSEHeader))
	if err != nil {
		return -1, fmt.Errorf("could not parse Min-SE value: %w", err)
	}
	return minSE, nil
}

// TopVia は、レスポンスから最上位のViaヘッダーを解析して返します。
func (r *SIPResponse) TopVia() (*Via, error) {
	viaHeader := r.GetHeader("Via")
	if viaHeader == "" {
		return nil, fmt.Errorf("no Via header found in response")
	}

	topViaValue := viaHeader
	if idx := strings.Index(viaHeader, ","); idx != -1 {
		topViaValue = strings.TrimSpace(viaHeader[:idx])
	}

	return ParseVia(topViaValue)
}

// PopVia は、レスポンスから最上位のViaヘッダーを削除します。
func (r *SIPResponse) PopVia() {
	viaHeader := r.GetHeader("Via")
	if viaHeader == "" {
		return
	}

	if idx := strings.Index(viaHeader, ","); idx != -1 {
		// さらにViaヘッダーがあるため、ヘッダーをリストの残りの部分に設定します。
		r.Headers["Via"] = strings.TrimSpace(viaHeader[idx+1:])
	} else {
		// これが唯一のViaヘッダーだったので、ヘッダーを完全に削除します。
		delete(r.Headers, "Via")
	}
}

// BuildResponse は、SIPレスポンスオブジェクトを構築します。
// 元のリクエストから必要なヘッダーをコピーし、新しいヘッダーの追加を許可します。
func BuildResponse(statusCode int, statusText string, req *SIPRequest, extraHeaders map[string]string) *SIPResponse {
	resp := &SIPResponse{
		Proto:      req.Proto,
		StatusCode: statusCode,
		Reason:     statusText,
		Headers:    make(map[string]string),
		Body:       []byte{},
	}

	// 元のリクエストから必須ヘッダーをコピーします。
	// 正規のヘッダーキー形式を使用します。
	headersToCopy := []string{"Via", "From", "To", "Call-ID", "CSeq"}
	for _, h := range headersToCopy {
		if val := req.GetHeader(h); val != "" {
			// RFC 3261で要求されているように、リクエストの'To'ヘッダーにまだタグがない場合、
			// レスポンスの'To'ヘッダーにタグを追加します。
			if h == "To" && !strings.Contains(req.GetHeader("To"), "tag=") {
				// これはデモンストレーション用の単純な静的タグです。実際のサーバーは
				// ダイアログ用に一意のタグを生成します。
				val = fmt.Sprintf("%s;tag=z9hG4bK-response-tag", val)
			}
			resp.Headers[strings.Title(h)] = val
		}
	}

	// 呼び出し元から提供された追加のヘッダー（例：WWW-Authenticate、Contact、Allow）を追加します。
	for key, val := range extraHeaders {
		resp.Headers[strings.Title(key)] = val
	}

	return resp
}

// BuildAck は、INVITEへの特定の最終レスポンスに対するACKリクエストを作成します。
// RFC 3261によると、2xxレスポンスに対するACKは別のトランザクションですが、
// 2xx以外のレスポンスの場合は同じトランザクションの一部です。
func BuildAck(res *SIPResponse, originalReq *SIPRequest) *SIPRequest {
	ack := &SIPRequest{
		Method: "ACK",
		URI:    originalReq.URI,
		Proto:  originalReq.Proto,
		Headers: map[string]string{
			// ACKのViaヘッダーフィールドは、元のリクエストの最上位のVia
			// ヘッダーフィールドと同じでなければなりません（MUST）。
			"Via": originalReq.GetHeader("Via"),
			// ACKのToヘッダーフィールドは、確認応答されるレスポンスのToヘッダーフィールドと
			// 等しくなければなりません（MUST）。
			"To":           res.Headers["To"],
			"From":         originalReq.GetHeader("From"),
			"Call-ID":      originalReq.GetHeader("Call-ID"),
			"CSeq":         strings.Split(originalReq.GetHeader("CSeq"), " ")[0] + " ACK",
			"Max-Forwards": "70",
		},
		Body: []byte{},
	}

	// 元のINVITEにRouteヘッダーがあった場合はコピーします
	if route := originalReq.GetHeader("Route"); route != "" {
		ack.Headers["Route"] = route
	}

	return ack
}

// ParseSIPResponse は、生の文字列をSIPResponse構造体に解析します。
func ParseSIPResponse(raw string) (*SIPResponse, error) {
	lines := strings.Split(raw, "\r\n")
	if len(lines) < 1 {
		return nil, fmt.Errorf("empty response")
	}

	respLine := strings.SplitN(lines[0], " ", 3)
	if len(respLine) < 2 { // Reasonフレーズは空にすることができます
		return nil, fmt.Errorf("invalid response line: %s", lines[0])
	}

	statusCode, err := strconv.Atoi(respLine[1])
	if err != nil {
		return nil, fmt.Errorf("invalid status code '%s' in response line", respLine[1])
	}

	var reason string
	if len(respLine) > 2 {
		reason = respLine[2]
	}

	res := &SIPResponse{
		Proto:      respLine[0],
		StatusCode: statusCode,
		Reason:     reason,
		Headers:    make(map[string]string),
	}

	// ヘッダーの終わり（空行）を見つけます
	headerEndIndex := len(lines)
	bodyIndex := -1
	for i, line := range lines[1:] {
		if line == "" {
			headerEndIndex = i + 1
			bodyIndex = strings.Index(raw, "\r\n\r\n")
			break
		}
	}

	// ヘッダーを解析
	for _, line := range lines[1:headerEndIndex] {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue // 不正な形式のヘッダー
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		res.Headers[strings.Title(key)] = value
	}

	// Content-Lengthが示している場合はボディを取得します
	if contentLengthStr := res.GetHeader("Content-Length"); contentLengthStr != "" {
		if contentLength, err := strconv.Atoi(contentLengthStr); err == nil && contentLength > 0 {
			if bodyIndex != -1 && len(raw) >= bodyIndex+4 {
				res.Body = []byte(raw[bodyIndex+4:])
			}
		}
	}

	return res, nil
}
