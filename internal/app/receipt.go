package app

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
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

	// Redacted decision evidence. Values are populated by emitReceipt; keys are
	// unconditional so every chained line has the same canonical shape.
	Source        string `json:"source"`
	GateKey       string `json:"gate_key"`
	Item          string `json:"item"`
	Phase         string `json:"phase"`
	Choice        string `json:"choice"`
	AdmitDeny     string `json:"admit_deny"`
	Outcome       string `json:"outcome"`
	LatencyMS     int64  `json:"latency_ms"`
	FrameID       string `json:"frame_id"`
	TS            string `json:"ts"`
	Signature     string `json:"signature"`
	ExpiresAt     int64  `json:"expires_at"`
	ItemsSHA256   string `json:"items_sha256"`
	ReceiptSHA256 string `json:"receipt_sha256"`
	BuildSHA      string `json:"build_sha"`
}

// receiptChain is a single-process hash chain persisted as newline-delimited
// JSON. Each append covers the previous digest, so an edited or dropped line
// breaks verification. Directory 0700, file 0600; a symlink or non-regular
// file is refused.
type receiptChain struct {
	mu     sync.Mutex
	head   []byte
	seq    int
	frames map[string]bool // FrameIDs already chained by an inbound decision receipt
}

var errReceiptUnsafeFile = errors.New("receipt.chain is not a regular file")

// receiptPath is the evidence-facing path: the chain lives in the per-user
// evidence directory convention used by the arch-map artifacts.
func receiptPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, routerDir, "evidence", "receipt.chain"), nil
}

// receiptStorePath is the physical chain. The evidence path is a hard link to
// the same inode, so there is one chain and old readers keep working.
func receiptStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, routerDir, "receipt.chain"), nil
}

// linkReceiptEvidence exposes the stored chain at the evidence path without
// copying or fabricating a second ledger. A foreign file there fails closed.
func linkReceiptEvidence(store, evidence string) error {
	if err := os.MkdirAll(filepath.Dir(evidence), 0700); err != nil {
		return err
	}
	st, err := os.Lstat(evidence)
	if os.IsNotExist(err) {
		return os.Link(store, evidence)
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return errReceiptUnsafeFile
	}
	src, err := os.Lstat(store)
	if err != nil || !os.SameFile(st, src) {
		return errReceiptUnsafeFile
	}
	return nil
}

// receiptHeadPath is a durable sidecar written after every successful append.
// Its presence means a chain once held records, so an empty or missing store
// is truncation/loss, not a fresh chain.
func receiptHeadPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, routerDir, "receipt.chain.head"), nil
}

// chainWasPopulated reports whether a prior append ever completed.
func chainWasPopulated() (bool, error) {
	hp, err := receiptHeadPath()
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(hp)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// loadReceiptChain rebuilds in-memory chain state from the persisted tail.
// It fails closed (nil) on an unreadable, tampered or truncated chain.
func loadReceiptChain() *receiptChain {
	path, err := receiptStorePath()
	if err != nil {
		return &receiptChain{}
	}
	populated, err := chainWasPopulated()
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !populated {
			return &receiptChain{}
		}
		return nil
	}
	// A zero-length store is only a fresh chain if no prior append ever
	// completed. A populated chain truncated in place must not reset to seq 1.
	// Whitespace-only content is never a valid chain.
	if len(raw) == 0 {
		if populated {
			return nil
		}
		return &receiptChain{}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var prev []byte
	frames := map[string]bool{}
	for i, line := range lines {
		var r receipt
		if err := json.Unmarshal(line, &r); err != nil || r.Seq != i+1 || r.Prev != hex.EncodeToString(prev) {
			return nil
		}
		if r.Event != "outbound_response" && r.FrameID != "" {
			frames[r.FrameID] = true
		}
		body, err := canonicalJSON(r)
		if err != nil {
			return nil
		}
		sum := chainHash(prev, body)
		prev = sum[:]
	}
	return &receiptChain{head: prev, seq: len(lines), frames: frames}
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
	c.mu.Lock()
	defer c.mu.Unlock()
	// An outbound mirror of a frame already chained as a decision is the same
	// request, not a second one. Unmatched server frames (no inbound receipt)
	// still chain, so the outbound side keeps its own decision point.
	if r.Event == "outbound_response" && r.FrameID != "" && c.frames[r.FrameID] {
		return hex.EncodeToString(c.head), nil
	}
	nonce, err := receiptNonce()
	if err != nil {
		return "", err
	}
	r.Nonce = nonce
	r.Seq = c.seq + 1
	r.ResponseID = "none" // never fabricated, never "NEVER"
	r.Choice = "redacted" // raw SO answer is never persisted
	prev := hex.EncodeToString(c.head)
	r.Prev = prev
	body, err := canonicalJSON(r)
	if err != nil {
		return "", err
	}
	sum := chainHash(c.head, body)
	path, err := receiptStorePath()
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
	// The line is durable now: advance the in-memory chain before the sidecar
	// steps, or a later failure reuses this seq and breaks every later append.
	c.head, c.seq = sum[:], r.Seq
	if r.Event != "outbound_response" && r.FrameID != "" {
		if c.frames == nil {
			c.frames = map[string]bool{}
		}
		c.frames[r.FrameID] = true
	}
	evidence, err := receiptPath()
	if err != nil {
		return "", err
	}
	if err := linkReceiptEvidence(path, evidence); err != nil {
		return "", err
	}
	hp, err := receiptHeadPath()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(hp, []byte(hex.EncodeToString(sum[:])+"\n"), 0600); err != nil {
		return "", err
	}
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
