package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxStateLength is the host's OAuth state limit. The sealed blob has to fit in
// it, which is why the WorkBuddy sign-in state is kept beside the flow instead of
// carried in CPA's opaque state: WorkBuddy mints it, so it cannot be derived.
const maxStateLength = 128

// loginState is the small amount of flow context CPA's opaque state carries
// between auth.login.start and auth.login.poll.
type loginState struct {
	Site    string
	Expires int64
	Nonce   []byte // 16 bytes, the key of the pending flow
}

var stateKeyMu sync.Mutex

func loadStateKey(authDir string) ([]byte, error) {
	if authDir == "" {
		return nil, errors.New("CPA auth directory unavailable")
	}
	stateKeyMu.Lock()
	defer stateKeyMu.Unlock()
	if err := os.MkdirAll(authDir, 0700); err != nil {
		return nil, errors.New("cannot create auth directory")
	}
	path := filepath.Join(authDir, ".workbuddy-oauth-state.key")
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, errors.New("invalid state encryption key")
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, errors.New("cannot read state encryption key")
	}
	key = make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, key); err != nil {
		return nil, errors.New("cannot create state encryption key")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		key, err = os.ReadFile(path)
		if err != nil || len(key) != 32 {
			return nil, errors.New("cannot read state encryption key")
		}
		return key, nil
	}
	if err != nil {
		return nil, errors.New("cannot persist state encryption key")
	}
	_, err = f.Write(key)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(path)
		return nil, errors.New("cannot persist state encryption key")
	}
	return key, nil
}

func newNonce() ([]byte, error) {
	b := make([]byte, 16)
	_, err := io.ReadFull(rand.Reader, b)
	return b, err
}

func sealState(key []byte, s loginState) (string, error) {
	if len(s.Nonce) != 16 || s.Expires <= 0 {
		return "", errors.New("unsupported login flow state")
	}
	spec, ok := siteFor(s.Site)
	if !ok {
		return "", errors.New("unsupported login site")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", errors.New("cannot generate login state")
	}
	plain := make([]byte, 21)
	if spec.ID == siteWorkBuddyAI {
		plain[0] = 1
	}
	binary.BigEndian.PutUint32(plain[1:5], uint32(s.Expires))
	copy(plain[5:21], s.Nonce)
	encoded := base64.RawURLEncoding.EncodeToString(append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, plain, []byte("workbuddy-login-v1"))...))
	if len(encoded) > maxStateLength {
		return "", errors.New("login state exceeds the CPA state length limit")
	}
	return encoded, nil
}

func openState(key []byte, encoded string) (loginState, error) {
	var s loginState
	if len(encoded) > maxStateLength {
		return s, errors.New("invalid login state")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 12+21+16 {
		return s, errors.New("invalid login state")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return s, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return s, err
	}
	nonce := raw[:12]
	p, err := gcm.Open(nil, nonce, raw[12:], []byte("workbuddy-login-v1"))
	if err != nil || len(p) != 21 || p[0]&^byte(1) != 0 {
		return s, errors.New("invalid login state")
	}
	s = loginState{
		Site:    siteWorkBuddy,
		Expires: int64(binary.BigEndian.Uint32(p[1:5])),
		Nonce:   append([]byte(nil), p[5:21]...),
	}
	if p[0]&1 != 0 {
		s.Site = siteWorkBuddyAI
	}
	return s, nil
}

// pendingFlow is the WorkBuddy side of a browser sign-in: the state its sign-in
// page was opened with, which the poll must send back verbatim.
type pendingFlow struct {
	VendorState string
	Expires     int64
}

// maxPendingFlows bounds the flows kept in one process. A sign-in that is
// abandoned leaves its entry to be dropped by the next start, so a caller
// cannot grow this map without bound.
const maxPendingFlows = 64

var flowMu sync.Mutex
var pendingFlows = map[string]pendingFlow{}

// rememberFlow records the vendor state of one flow under the nonce CPA carries.
// The vendor state is kept in memory rather than in CPA's opaque state: it is
// long enough that sealing it would not fit the host's 128-character limit. A
// plugin restart therefore ends an unfinished sign-in, which the poll reports as
// expired rather than attempting a flow it cannot resume.
func rememberFlow(nonce []byte, vendorState string, expires time.Time) {
	if len(nonce) != 16 || vendorState == "" {
		return
	}
	now := time.Now()
	key := hex.EncodeToString(nonce)
	flowMu.Lock()
	defer flowMu.Unlock()
	for k, f := range pendingFlows {
		if f.Expires <= now.Unix() {
			delete(pendingFlows, k)
		}
	}
	// At the cap, the flow closest to its own end is the one to drop: a sign-in
	// left open cannot be finished once its window has passed.
	for len(pendingFlows) >= maxPendingFlows {
		oldest, at := "", int64(0)
		for k, f := range pendingFlows {
			if oldest == "" || f.Expires < at {
				oldest, at = k, f.Expires
			}
		}
		delete(pendingFlows, oldest)
	}
	pendingFlows[key] = pendingFlow{VendorState: vendorState, Expires: expires.Unix()}
}

// takeFlow returns and forgets the vendor state of one flow.
func takeFlow(nonce []byte) (string, bool) {
	if len(nonce) != 16 {
		return "", false
	}
	key := hex.EncodeToString(nonce)
	flowMu.Lock()
	defer flowMu.Unlock()
	f, ok := pendingFlows[key]
	if !ok {
		return "", false
	}
	delete(pendingFlows, key)
	if f.Expires <= time.Now().Unix() {
		return "", false
	}
	return f.VendorState, true
}

// peekFlow returns the vendor state of one flow without consuming it, so a
// pending poll can be asked again.
func peekFlow(nonce []byte) (string, bool) {
	if len(nonce) != 16 {
		return "", false
	}
	key := hex.EncodeToString(nonce)
	flowMu.Lock()
	defer flowMu.Unlock()
	f, ok := pendingFlows[key]
	if !ok || f.Expires <= time.Now().Unix() {
		return "", false
	}
	return f.VendorState, true
}
