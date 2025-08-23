package sip

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
)

// md5hash computes the MD5 hash of a string and returns it as a hex string.
func md5hash(data string) string {
	sum := md5.Sum([]byte(data))
	return hex.EncodeToString(sum[:])
}

// CalculateResponse computes the expected digest response for authentication.
// It uses the HA1 hash (which is pre-calculated and stored), nonce, method, and URI.
func CalculateResponse(ha1, method, digestURI, nonce string) string {
	ha2 := md5hash(fmt.Sprintf("%s:%s", method, digestURI))
	return md5hash(fmt.Sprintf("%s:%s:%s", ha1, nonce, ha2))
}
