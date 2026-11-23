package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// receipt is the minimal, redacted audit record. It carries no prompt, key,
// raw confidence or endpoint: only reason-coded routing facts plus a random
// nonce and the hash of the previous record (H(n)=sha256(H(n-1)||canonicalJSON(receipt))).
type receipt struct {
	Seq        int    `json:"seq"`
	Nonce      string `json:"nonce"`
	Prev       string `json:"prev"`
	Event      string `json:"event"`
	Decision   string `json:"decision,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Method     string `json:"method,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	ResponseID string `json:"response_id"` // always "none" - no endpoint request id exists
}

// receiptChain is a single-process hash chain persisted as newline-delimited
// JSON. Each append covers the previous digest, so an edited or dropped line
// breaks verification. Directory 0700, file 0600; a symlink or non-regular
// file is refused.
type receiptChain struct {
	head []byte
	seq  int
}

var errReceiptUnsafeFile = errors.New("receipt.chain is not a regular file")

func receiptPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, routerDir, "receipt.chain"), nil
}

// receiptNonce returns a fresh crypto/rand nonce. Failure fails closed: callers
// omit the receipt rather than mint a predictable one.
func receiptNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// appendReceipt writes one chained receipt. It returns the new head digest.
func (c *receiptChain) appendReceipt(r receipt) (string, error) {
	nonce, err := receiptNonce()
	if err != nil {
		return "", err
	}
	r.Nonce = nonce
	r.Seq = c.seq + 1
	r.ResponseID = "none" // never fabricated, never "NEVER"
	prev := hex.EncodeToString(c.head)
	r.Prev = prev
	body, err := canonicalJSON(r)
	if err != nil {
		return "", err
	}
	sum := chainHash(c.head, body)
	path, err := receiptPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return "", errReceiptUnsafeFile
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return "", err
	}
	line := append(body, '\n')
	if _, err := f.Write(line); err != nil {
		return "", err
	}
	c.head, c.seq = sum[:], r.Seq
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON returns a byte-stable JSON encoding (sorted keys via encoding/json).
func canonicalJSON(v any) ([]byte, error) { return json.Marshal(v) }

// chainHash computes sha256(prev || canonicalJSON(receipt)).
func chainHash(prev, body []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write(prev)
	_, _ = h.Write(body)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
