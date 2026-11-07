package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Model denials outlive the process. WorkBuddy's product config lists models an
// account's plan does not include, and the only way to learn that is to be
// refused, so the refusal is worth keeping: a restart that forgot it would send
// the same model upstream again and earn the same refusal, while CPA's own
// per-model cooldown for that credential still remembers it.
//
// The file lives beside the sign-in state key in CPA's auth directory. It holds
// no tokens, only which account was refused which model and until when, so a
// plain document is enough. Every failure here is silent: a denial that cannot
// be persisted still holds in memory for this process, and losing it costs one
// refusal, never a request.

// denialsFileName is the document's name inside the auth directory.
const denialsFileName = ".workbuddy-oauth-denials.json"

// denialRecord is one persisted refusal.
type denialRecord struct {
	Until    time.Time `json:"until"`
	UserType string    `json:"user_type,omitempty"`
}

// denialStore is the document's shape: account -> model -> record. The account
// key is the build and the uid, the same pair the in-memory map uses.
type denialStore struct {
	Accounts map[string]map[string]denialRecord `json:"accounts"`
}

var denialFileMu sync.Mutex

// denialFilePath is where the denials are kept, or "" when CPA has not told the
// plugin where its auth material lives yet.
func denialFilePath() string {
	dir := knownAuthDir()
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, denialsFileName)
}

// storedAccountKey is the account key a persisted record is filed under. The
// in-memory key uses a NUL separator, which a JSON document should not carry.
func storedAccountKey(c credential) string {
	return c.Site + ":" + c.UID
}

// loadDenials reads the persisted refusals into the in-memory map, dropping the
// ones that have lapsed. A missing or unreadable document is not an error.
func loadDenials() {
	path := denialFilePath()
	if path == "" {
		return
	}
	denialFileMu.Lock()
	defer denialFileMu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		denialMu.Lock()
		denialsLoaded = true
		denialMu.Unlock()
		return
	}
	var store denialStore
	if json.Unmarshal(raw, &store) != nil {
		denialMu.Lock()
		denialsLoaded = true
		denialMu.Unlock()
		return
	}
	now := time.Now()
	denialMu.Lock()
	defer denialMu.Unlock()
	denialsLoaded = true
	for account, models := range store.Accounts {
		for model, record := range models {
			if !record.Until.After(now) {
				continue
			}
			key := strings.Replace(account, ":", "\x00", 1)
			if denials[key] == nil {
				denials[key] = map[string]modelDenial{}
			}
			if existing, ok := denials[key][model]; ok && existing.until.After(record.Until) {
				continue
			}
			denials[key][model] = modelDenial{until: record.Until, userType: record.UserType}
		}
	}
}

// saveDenials writes the refusals that are still ahead of now. The document is
// replaced atomically so a reader never sees half a file.
func saveDenials() {
	path := denialFilePath()
	if path == "" {
		return
	}
	now := time.Now()
	store := denialStore{Accounts: map[string]map[string]denialRecord{}}
	denialMu.Lock()
	for account, models := range denials {
		for model, record := range models {
			if !record.until.After(now) {
				continue
			}
			key := strings.Replace(account, "\x00", ":", 1)
			if store.Accounts[key] == nil {
				store.Accounts[key] = map[string]denialRecord{}
			}
			store.Accounts[key][model] = denialRecord{Until: record.until, UserType: record.userType}
		}
	}
	denialMu.Unlock()
	if len(store.Accounts) == 0 {
		_ = os.Remove(path)
		return
	}
	raw, err := json.Marshal(store)
	if err != nil {
		return
	}
	denialFileMu.Lock()
	defer denialFileMu.Unlock()
	tmp, err := os.CreateTemp(filepath.Dir(path), denialsFileName+".*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name)
		return
	}
	if err = os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return
	}
	if err = os.Rename(name, path); err != nil {
		_ = os.Remove(name)
	}
}
