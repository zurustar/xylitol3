package sip

import (
	"fmt"
	"strconv"
	"strings"
)

// SIPRequest は、解析されたSIPリクエストを表します。
type SIPRequest struct {
	Method        string
	URI           string
	Proto         string
	Headers       map[string]string
	Authorization map[string]string
	Body          []byte
}

// String は、SIPリクエストの文字列表現を返します。
func (r *SIPRequest) String() string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("%s %s %s\r\n", r.Method, r.URI, r.Proto))
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

// Clone は、SIPRequestのディープコピーを作成します。
func (r *SIPRequest) Clone() *SIPRequest {
	if r == nil {
		return nil
	}
	clone := &SIPRequest{
		Method:        r.Method,
		URI:           r.URI,
		Proto:         r.Proto,
		Headers:       make(map[string]string, len(r.Headers)),
		Authorization: make(map[string]string, len(r.Authorization)),
		Body:          make([]byte, len(r.Body)),
	}
	for k, v := range r.Headers {
		clone.Headers[k] = v
	}
	for k, v := range r.Authorization {
		clone.Authorization[k] = v
	}
	copy(clone.Body, r.Body)
	return clone
}

// SIPURI は、解析されたSIP URIを表します。
type SIPURI struct {
	Scheme string // 例: "sip"
	User   string
	Host   string
	Port   string
	Params map[string]string
}

// String は、URIを文字列に再構築します。
func (u *SIPURI) String() string {
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString(":")
	if u.User != "" {
		b.WriteString(u.User)
		b.WriteString("@")
	}
	b.WriteString(u.Host)
	if u.Port != "" {
		b.WriteString(":")
		b.WriteString(u.Port)
	}
	// 注: これはパラメータの元の順序を保持しません。
	for k, v := range u.Params {
		b.WriteString(";")
		b.WriteString(k)
		if v != "" {
			b.WriteString("=")
			b.WriteString(v)
		}
	}
	return b.String()
}

// ParseSIPURI は、SIP URI文字列をSIPURI構造体に解析します。
// <sip:user@host:port;params> または sip:user@host の形式のURIを処理します。
func ParseSIPURI(uri string) (*SIPURI, error) {
	uri = strings.Trim(uri, "<>") // 山括弧を削除

	schemeParts := strings.SplitN(uri, ":", 2)
	if len(schemeParts) != 2 {
		return nil, fmt.Errorf("invalid URI format (missing scheme): %s", uri)
	}

	s := &SIPURI{
		Scheme: schemeParts[0],
		Params: make(map[string]string),
	}
	remaining := schemeParts[1]

	// メイン部分からパラメータを分離
	mainAndParams := strings.Split(remaining, ";")
	mainPart := mainAndParams[0]

	// パラメータを解析
	if len(mainAndParams) > 1 {
		for _, param := range mainAndParams[1:] {
			kv := strings.SplitN(param, "=", 2)
			key := strings.TrimSpace(kv[0])
			var val string
			if len(kv) == 2 {
				val = strings.TrimSpace(kv[1])
			}
			s.Params[strings.ToLower(key)] = val
		}
	}

	// メイン部分から user@host:port を解析
	userHostPort := strings.SplitN(mainPart, "@", 2)
	var hostPortPart string
	if len(userHostPort) == 2 {
		s.User = userHostPort[0]
		hostPortPart = userHostPort[1]
	} else {
		hostPortPart = userHostPort[0]
	}

	hostPort := strings.SplitN(hostPortPart, ":", 2)
	s.Host = hostPort[0]
	if len(hostPort) == 2 {
		s.Port = hostPort[1]
	}

	if s.Host == "" {
		return nil, fmt.Errorf("host is missing in URI: %s", uri)
	}

	return s, nil
}

// GetHeader は、大文字と小文字を区別せずにヘッダーの値を返します。
func (r *SIPRequest) GetHeader(name string) string {
	return r.Headers[strings.Title(name)]
}

// GetSIPURIHeader は、SIP URIを含むと予想されるヘッダーを解析します。
func (r *SIPRequest) GetSIPURIHeader(name string) (*SIPURI, error) {
	headerValue := r.GetHeader(name)
	if headerValue == "" {
		return nil, fmt.Errorf("header '%s' not found", name)
	}
	// 値はURIだけの場合もあれば、「Bob」<sip:bob@host>のような表示名を持つ場合もあります。
	// 最初に山括弧を探します。
	start := strings.Index(headerValue, "<")
	end := strings.Index(headerValue, ">")
	if start != -1 && end != -1 && end > start {
		headerValue = headerValue[start : end+1]
	}

	return ParseSIPURI(headerValue)
}

// Via は、単一のViaヘッダー値を表します。
type Via struct {
	Proto  string // 例: "SIP/2.0/UDP"
	Host   string
	Port   string
	Params map[string]string
}

// GetParam は、大文字と小文字を区別せずにViaヘッダーからパラメータを返します。
func (v *Via) GetParam(name string) (string, bool) {
	val, ok := v.Params[strings.ToLower(name)]
	return val, ok
}

// Branch は、Viaヘッダーからbranchパラメータを返します。
func (v *Via) Branch() string {
	val, _ := v.GetParam("branch")
	return val
}

// ParseVia は、単一のViaヘッダーフィールド値文字列を解析します。
func ParseVia(viaValue string) (*Via, error) {
	parts := strings.Split(viaValue, ";")
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty via value")
	}

	// 最初の部分はプロトコルと送信元
	sentByParts := strings.Fields(parts[0])
	if len(sentByParts) != 2 {
		return nil, fmt.Errorf("malformed sent-by in Via: %s", parts[0])
	}

	via := &Via{
		Proto:  sentByParts[0],
		Params: make(map[string]string),
	}

	hostPort := strings.SplitN(sentByParts[1], ":", 2)
	via.Host = hostPort[0]
	if len(hostPort) == 2 {
		via.Port = hostPort[1]
	}

	// 後続の部分はパラメータ
	for _, param := range parts[1:] {
		kv := strings.SplitN(param, "=", 2)
		key := strings.TrimSpace(kv[0])
		var val string
		if len(kv) == 2 {
			val = strings.TrimSpace(kv[1])
		}
		via.Params[strings.ToLower(key)] = val
	}

	return via, nil
}

// TopVia は、最上位のViaヘッダーを解析して返します。
func (r *SIPRequest) TopVia() (*Via, error) {
	viaHeader := r.GetHeader("Via")
	if viaHeader == "" {
		return nil, fmt.Errorf("no Via header found")
	}

	// リクエストには複数のViaヘッダーが含まれる場合があり、単一のヘッダー行にカンマ区切りのリストとして表されます。
	// 最上位のものは最初のものです。
	// 堅牢なスプリッターを使用して、最初の論理的なViaエントリを正しく見つけます。
	viaValues := splitByCommaOutsideQuotes(viaHeader)
	if len(viaValues) == 0 {
		return nil, fmt.Errorf("no Via header values found after parsing")
	}
	topViaValue := strings.TrimSpace(viaValues[0])

	return ParseVia(topViaValue)
}

// splitByCommaOutsideQuotes は、文字列をカンマで分割しますが、二重引用符内のカンマは無視します。
func splitByCommaOutsideQuotes(s string) []string {
	var result []string
	var start int
	inQuotes := false
	for i, r := range s {
		if r == '"' {
			inQuotes = !inQuotes
		} else if r == ',' && !inQuotes {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	result = append(result, s[start:])
	return result
}

// AllVias は、リクエストからすべてのViaヘッダーを解析して返します。
// リクエストには、別々の行にある複数のViaヘッダー（メッセージパーサーによってカンマ区切りのリストに連結される）か、
// 単一のヘッダー行にカンマ区切りのリストとして含まれる場合があります。
func (r *SIPRequest) AllVias() ([]*Via, error) {
	viaHeader := r.GetHeader("Via")
	if viaHeader == "" {
		return nil, nil // viaヘッダーがないことはエラーではありません
	}

	viaValues := splitByCommaOutsideQuotes(viaHeader)
	vias := make([]*Via, 0, len(viaValues))

	for _, viaValue := range viaValues {
		viaValue = strings.TrimSpace(viaValue)
		if viaValue == "" {
			continue
		}
		via, err := ParseVia(viaValue)
		if err != nil {
			// 部分的なリストとエラーを返す
			return vias, fmt.Errorf("failed to parse via entry '%s': %w", viaValue, err)
		}
		vias = append(vias, via)
	}

	return vias, nil
}

// ParseSIPRequest は、生のSIPリクエストを文字列から解析します。
func ParseSIPRequest(raw string) (*SIPRequest, error) {
	// ヘッダーとボディを区切る二重CRLFを見つけます
	endOfHeaders := strings.Index(raw, "\r\n\r\n")
	if endOfHeaders == -1 {
		// 二重CRLFがない場合、リクエストはボディのないヘッダーだけであるか、
		// ボディを持つはずなのに不正な形式である可能性があります。
		// ここでは、全体をヘッダー部分として扱います。
		endOfHeaders = len(raw)
	}

	headerPart := raw[:endOfHeaders]
	lines := strings.Split(headerPart, "\r\n")
	if len(lines) < 1 {
		return nil, fmt.Errorf("empty request")
	}

	// リクエスト行を解析
	reqLine := strings.Split(lines[0], " ")
	if len(reqLine) != 3 {
		return nil, fmt.Errorf("invalid request line: %s", lines[0])
	}

	req := &SIPRequest{
		Method:  reqLine[0],
		URI:     reqLine[1],
		Proto:   reqLine[2],
		Headers: make(map[string]string),
	}

	// ヘッダーを解析
	for _, line := range lines[1:] {
		if line == "" {
			continue // このロジックでは起こらないはずですが、良い習慣です
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue // 不正な形式のヘッダー
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		headerKey := strings.Title(key)
		req.Headers[headerKey] = value

		if headerKey == "Authorization" {
			req.Authorization = parseAuthHeader(value)
		}
	}

	if req.Method == "" {
		return nil, fmt.Errorf("missing method")
	}

	// ボディがあれば解析
	contentLengthStr := req.GetHeader("Content-Length")
	if contentLengthStr != "" {
		contentLength, err := strconv.Atoi(contentLengthStr)
		if err == nil && contentLength > 0 {
			bodyStart := endOfHeaders + 4 // a.k.a. len("\r\n\r\n")
			if bodyStart+contentLength <= len(raw) {
				req.Body = []byte(raw[bodyStart : bodyStart+contentLength])
			} else {
				// 不正なリクエスト、Content-Lengthが実際のボディサイズより大きい
				return nil, fmt.Errorf("malformed request: content length %d is larger than actual body size", contentLength)
			}
		}
	}

	return req, nil
}

// parseAuthHeader は、Digest Authorizationヘッダーの値を解析します。
func parseAuthHeader(headerValue string) map[string]string {
	result := make(map[string]string)
	if !strings.HasPrefix(headerValue, "Digest ") {
		return result
	}
	headerValue = strings.TrimPrefix(headerValue, "Digest ")

	parts := strings.Split(headerValue, ",")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			key := strings.TrimSpace(kv[0])
			value := strings.Trim(strings.TrimSpace(kv[1]), `"`)
			result[key] = value
		}
	}
	return result
}

// SessionExpires は、Session-Expiresヘッダーの解析された値を表します。
type SessionExpires struct {
	Delta     int
	Refresher string // "uac" または "uas"
}

// ParseSessionExpires は、Session-Expiresヘッダーの値を解析します。
func ParseSessionExpires(headerValue string) (*SessionExpires, error) {
	se := &SessionExpires{}
	parts := strings.Split(headerValue, ";")

	delta, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return nil, fmt.Errorf("could not parse delta-seconds: %w", err)
	}
	se.Delta = delta

	for _, part := range parts[1:] {
		paramParts := strings.SplitN(part, "=", 2)
		if len(paramParts) == 2 {
			key := strings.ToLower(strings.TrimSpace(paramParts[0]))
			val := strings.ToLower(strings.TrimSpace(paramParts[1]))
			if key == "refresher" {
				se.Refresher = val
			}
		}
	}
	return se, nil
}

// SessionExpires は、リクエストからSession-Expiresヘッダーを解析します。
func (r *SIPRequest) SessionExpires() (*SessionExpires, error) {
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

// MinSE は、リクエストからMin-SEヘッダーを解析します。見つからない場合は-1を返します。
func (r *SIPRequest) MinSE() (int, error) {
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

// Expires は、ContactまたはExpiresヘッダーから有効期限を返します。
func (r *SIPRequest) Expires() int {
	expires := 3600 // デフォルトの有効期限
	contactHeader := r.GetHeader("Contact")
	if strings.Contains(contactHeader, "expires=") {
		parts := strings.Split(contactHeader, ";")
		for _, p := range parts {
			if strings.HasPrefix(p, "expires=") {
				expStr := strings.TrimPrefix(p, "expires=")
				if exp, err := strconv.Atoi(expStr); err == nil {
					return exp
				}
			}
		}
	}
	if expStr := r.GetHeader("Expires"); expStr != "" {
		if exp, err := strconv.Atoi(expStr); err == nil {
			expires = exp
		}
	}
	return expires
}
