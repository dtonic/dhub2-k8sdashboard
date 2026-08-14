// Quickwit scroll cursor encoding. The cursor is an HMAC-signed capability.
package quickwit

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
)

const (
	cursorVersion         = 2
	maxEncodedCursorBytes = 8 << 10
	maxScrollIDBytes      = 4 << 10
	digestBytes           = sha256.Size
	nonceBytes            = 16
)

type cursor struct {
	ScrollID  string `json:"s"`
	QueryHash string `json:"q"`
	Nonce     string `json:"x"`
	Returned  int    `json:"n"`
	Scanned   int    `json:"p"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated,omitempty"`
}

type cursorPayload struct {
	Version int `json:"v"`
	cursor
}

type cursorWire struct {
	cursorPayload
	Signature string `json:"sig"`
}

func encodeCursor(c cursor, key []byte) (string, bool) {
	if len(c.ScrollID) == 0 || len(c.ScrollID) > maxScrollIDBytes {
		return "", false
	}
	p := cursorPayload{Version: cursorVersion, cursor: c}
	raw, _ := json.Marshal(p)
	w := cursorWire{cursorPayload: p, Signature: signCursor(raw, key)}
	wire, _ := json.Marshal(w)
	encoded := base64.RawURLEncoding.EncodeToString(wire)
	return encoded, len(encoded) <= maxEncodedCursorBytes
}

func decodeCursor(s string, maxLines int, key []byte) (cursor, bool) {
	if s == "" {
		return cursor{}, true
	}
	if len(s) > maxEncodedCursorBytes || maxLines <= 0 {
		return cursor{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var w cursorWire
	if err := dec.Decode(&w); err != nil {
		return cursor{}, false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return cursor{}, false
	}
	pRaw, _ := json.Marshal(w.cursorPayload)
	want := signCursor(pRaw, key)
	if !hmac.Equal([]byte(w.Signature), []byte(want)) {
		return cursor{}, false
	}
	if w.Version != cursorVersion || len(w.ScrollID) == 0 || len(w.ScrollID) > maxScrollIDBytes ||
		w.Returned < 0 || w.Scanned <= 0 || w.Scanned >= maxLines || w.Returned > w.Scanned || w.Total < w.Scanned ||
		!fixedBase64(w.QueryHash, digestBytes) || !fixedBase64(w.Nonce, nonceBytes) {
		return cursor{}, false
	}
	return w.cursor, true
}

func signCursor(payload, key []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func fixedBase64(s string, size int) bool {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(raw) == size
}
