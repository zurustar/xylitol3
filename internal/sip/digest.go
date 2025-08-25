package sip

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
)

// md5hashは文字列のMD5ハッシュを計算し、16進文字列として返します。
func md5hash(data string) string {
	sum := md5.Sum([]byte(data))
	return hex.EncodeToString(sum[:])
}

// CalculateResponseは認証のための期待されるダイジェストレスポンスを計算します。
// 事前に計算されて保存されているHA1ハッシュ、nonce、method、URIを使用します。
func CalculateResponse(ha1, method, digestURI, nonce string) string {
	ha2 := md5hash(fmt.Sprintf("%s:%s", method, digestURI))
	return md5hash(fmt.Sprintf("%s:%s:%s", ha1, nonce, ha2))
}
