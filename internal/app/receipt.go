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
	"strings"
	"sync"
	"syscall"
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

// receiptChain is a hash chain persisted as newline-delimited JSON. Each append
// covers the previous digest, so an edited or dropped line breaks verification.
// A cross-process lock covers load, hash, write and head update; the on-disk
// tail is authoritative. Directory 0700, file 0600; symlinks are refused.
type receiptChain struct {
	mu     sync.Mutex
	head   []byte
	seq    int
	frames map[string]bool // FrameIDs already chained by an inbound decision receipt
}

var errReceiptUnsafeFile = errors.New("receipt.chain is not a regular file")

// receiptLockPath is stable even when the store is replaced.
func receiptLockPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, routerDir, "receipt.chain.lock"), nil
}

// withReceiptLock serialises the whole read/hash/append/sidecar sequence across
// processes. The in-process mutex is still needed for callers sharing a chain.
func withReceiptLock(fn func() error) error {
	path, err := receiptLockPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return errReceiptUnsafeFile
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

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
// It records the newest digest, so an empty, shorter or rewritten store is
// truncation/loss, not a fresh chain.
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
	var out *receiptChain
	if err := withReceiptLock(func() error { out = loadReceiptChainLocked(); return nil }); err != nil {
		return nil
	}
	return out
}

// loadReceiptChainLocked rebuilds state while the caller holds the flock.
func loadReceiptChainLocked() *receiptChain {
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
	// A kill during a line write leaves an unterminated tail. It can never be
	// a valid receipt: cut back to the last complete line under the lock.
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		cut := bytes.LastIndexByte(raw, '\n') + 1
		if err := os.Truncate(path, int64(cut)); err != nil {
			return nil
		}
		raw = raw[:cut]
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
	// digests[i] is the chain head after line i+1.
	digests := make([][]byte, 0, len(lines))
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
		digests = append(digests, prev)
	}
	if populated {
		hp, err := receiptHeadPath()
		if err != nil {
			return nil
		}
		recorded, err := os.ReadFile(hp)
		if err != nil {
			return nil
		}
		rec := strings.TrimSpace(string(recorded))
		// The line is written before the sidecar is updated, so a crash in
		// between leaves the sidecar exactly one line behind. That is an
		// interrupted append, not a rollback: the next append rewrites the
		// sidecar. Any other mismatch (deleted or rewritten tail) fails closed.
		switch {
		case rec == hex.EncodeToString(prev):
		case len(digests) >= 2 && rec == hex.EncodeToString(digests[len(digests)-2]):
		default:
			return nil
		}
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
// The on-disk tail is authoritative: processes sharing the store reload it under
// a cross-process lock, so no writer can reuse another writer's seq or prev.
func (c *receiptChain) appendReceipt(r receipt) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out string
	err := withReceiptLock(func() error {
		disk := loadReceiptChainLocked()
		if disk == nil {
			return errReceiptUnsafeFile
		}
		// An outbound mirror of a frame already chained as a decision is the same
		// request, not a second one. Unmatched server frames (no inbound receipt)
		// still chain, so the outbound side keeps its own decision point.
		if r.Event == "outbound_response" && r.FrameID != "" && disk.frames[r.FrameID] {
			out = hex.EncodeToString(disk.head)
			return nil
		}
		nonce, err := receiptNonce()
		if err != nil {
			return err
		}
		r.Nonce = nonce
		r.Seq = disk.seq + 1
		r.ResponseID = "none" // never fabricated, never "NEVER"
		r.Choice = "redacted" // raw SO answer is never persisted
		prev := hex.EncodeToString(disk.head)
		r.Prev = prev
		body, err := canonicalJSON(r)
		if err != nil {
			return err
		}
		sum := chainHash(disk.head, body)
		path, err := receiptStorePath()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if st, err := os.Lstat(path); err == nil {
			if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
				return errReceiptUnsafeFile
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := f.Chmod(0600); err != nil {
			return err
		}
		line := append(body, '\n')
		if _, err := f.Write(line); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		// The line is durable now: advance before the sidecar steps, or a later
		// failure reuses this seq and breaks every later append.
		c.head, c.seq = sum[:], r.Seq
		if r.Event != "outbound_response" && r.FrameID != "" {
			if c.frames == nil {
				c.frames = map[string]bool{}
			}
			c.frames[r.FrameID] = true
		}
		evidence, err := receiptPath()
		if err != nil {
			return err
		}
		if err := linkReceiptEvidence(path, evidence); err != nil {
			return err
		}
		hp, err := receiptHeadPath()
		if err != nil {
			return err
		}
		if err := writeReceiptHead(hp, sum); err != nil {
			return err
		}
		out = hex.EncodeToString(sum[:])
		return nil
	})
	return out, err
}

// writeReceiptHead replaces the sidecar atomically; a kill during a rewrite
// must not leave a torn digest that looks like a rollback.
func writeReceiptHead(path string, sum [32]byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".receipt-head-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(hex.EncodeToString(sum[:]) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
