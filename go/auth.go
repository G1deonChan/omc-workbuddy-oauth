package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// signInTimeout is how long a browser sign-in may stay open. WorkBuddy's own
	// client waits five minutes; the panel is given the same window.
	signInTimeout = 5 * time.Minute
	// refreshLead is how early the host should renew a token.
	refreshLead = 5 * time.Minute
)

// expiredError marks a sign-in WorkBuddy refused or can no longer renew: the
// host must ask the user to sign in again rather than retry the refresh.
type expiredError struct{ message string }

func (e *expiredError) Error() string { return e.message }

func signInExpired(spec siteSpec) error {
	return &expiredError{message: spec.Name + " sign-in has expired; sign in again"}
}

// tokenAnswer is what WorkBuddy's sign-in and refresh endpoints return. Times are
// absolute milliseconds or seconds from now, in either spelling.
type tokenAnswer struct {
	AccessToken      string          `json:"accessToken"`
	RefreshToken     string          `json:"refreshToken"`
	ExpiresAt        json.RawMessage `json:"expiresAt"`
	ExpiresIn        float64         `json:"expiresIn"`
	RefreshExpiresAt json.RawMessage `json:"refreshExpiresAt"`
	RefreshExpiresIn float64         `json:"refreshExpiresIn"`
	Domain           string          `json:"domain"`
	TokenType        string          `json:"tokenType"`
	UID              string          `json:"uid"`
}

// merge takes a token WorkBuddy gave into a credential. A field WorkBuddy left
// out keeps what the credential already had, so a refresh that answers only a new
// access token does not erase the refresh token it was spent from.
func merge(c credential, got tokenAnswer) credential {
	if v := strings.TrimSpace(got.AccessToken); v != "" {
		c.Access = v
	}
	if v := strings.TrimSpace(got.RefreshToken); v != "" {
		c.Refresh = v
	}
	if v := strings.TrimSpace(got.Domain); v != "" {
		c.Domain = v
	}
	if v := strings.TrimSpace(got.TokenType); v != "" {
		c.TokenType = v
	}
	if at := whenSaid(got.ExpiresAt); at > 0 {
		c.Expires = at
	} else if got.ExpiresIn > 0 {
		c.Expires = time.Now().UnixMilli() + int64(got.ExpiresIn*1000)
	}
	if at := whenSaid(got.RefreshExpiresAt); at > 0 {
		c.RefreshExpires = at
	} else if got.RefreshExpiresIn > 0 {
		c.RefreshExpires = time.Now().UnixMilli() + int64(got.RefreshExpiresIn*1000)
	}
	return c
}

// whenSaid is the time a token's end field names, in milliseconds: a date, or an
// epoch in seconds or milliseconds; 0 for none.
func whenSaid(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0
		}
		if isDigits(s) {
			return epochMilli(s)
		}
		if at, err := time.Parse(time.RFC3339, s); err == nil {
			return at.UnixMilli()
		}
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if at, err := time.Parse(layout, s); err == nil {
				return at.UnixMilli()
			}
		}
		return 0
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return epochMilli(n.String())
	}
	return 0
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func epochMilli(s string) int64 {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		if n > (1<<62)/10 {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	if n <= 0 {
		return 0
	}
	if n < 1e12 {
		return n * 1000
	}
	return n
}

// loginEntry is the sign-in page's answer: the state to poll with, and the page
// to open.
type loginEntry struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

// startLogin asks WorkBuddy for a sign-in page on the selected build. The page is
// Tencent's own, the one WorkBuddy's desktop opens.
func startLogin(req pluginapi.AuthLoginStartRequest, sender httpSender) (pluginapi.AuthLoginStartResponse, error) {
	var out pluginapi.AuthLoginStartResponse
	if req.Host.AuthDir == "" {
		return out, errors.New("CPA auth directory unavailable")
	}
	spec, err := loginSite(req.Provider, req.Metadata, getConfig().Site)
	if err != nil {
		return out, err
	}
	key, err := loadStateKey(req.Host.AuthDir)
	if err != nil {
		return out, err
	}
	nonce, err := newNonce()
	if err != nil {
		return out, errors.New("cannot generate login nonce")
	}
	expires := time.Now().Add(signInTimeout)
	raw, err := wbCall(sender, "POST",
		spec.Endpoint+"/v2/plugin/auth/state?platform="+url.QueryEscape(spec.Platform),
		http.Header{
			"User-Agent":           {"WorkBuddy/" + spec.App},
			"X-No-Authorization":   {"true"},
			"X-No-User-Id":         {"true"},
			"X-No-Enterprise-Id":   {"true"},
			"X-No-Department-Info": {"true"},
		}, map[string]any{})
	if err != nil {
		return out, fmt.Errorf("%s sign-in page: %s", spec.Name, readableFailure(err))
	}
	var entry loginEntry
	if json.Unmarshal(raw, &entry) != nil || strings.TrimSpace(entry.State) == "" || strings.TrimSpace(entry.AuthURL) == "" {
		return out, errors.New(spec.Name + " returned no sign-in page")
	}
	page, err := signInPage(entry.AuthURL, spec.App)
	if err != nil {
		return out, err
	}
	rememberFlow(nonce, entry.State, expires)
	state, err := sealState(key, loginState{Site: spec.ID, Expires: expires.Unix(), Nonce: nonce})
	if err != nil {
		return out, err
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  credentialProvider,
		URL:       page,
		State:     state,
		ExpiresAt: expires.UTC(),
	}, nil
}

// signInPage is the page WorkBuddy named, ready to open: the desktop adds its own
// version to the query, which the page reads to decide what it offers for that
// build, and a session id of its own.
func signInPage(authURL, version string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(authURL))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", errors.New("WorkBuddy returned an invalid sign-in page")
	}
	query := u.Query()
	query.Set("version", version)
	session := make([]byte, 16)
	if _, err := rand.Read(session); err != nil {
		return "", errors.New("cannot generate login session id")
	}
	query.Set("loginSessionId", hex.EncodeToString(session))
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// loginSite resolves the build: the entry the request names wins, then an
// explicit metadata hint, then the configured fallback. An invalid explicit value
// is an error rather than a silent fallback.
func loginSite(provider string, metadata map[string]any, configured string) (siteSpec, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "":
		// A legacy panel that knows only the plugin key sends no provider.
	case pluginStoreID:
		// Same fallback path: the store identifier is not a login entry.
	case siteWorkBuddy, siteWorkBuddyAI:
		if spec, ok := configuredSpec(strings.ToLower(strings.TrimSpace(provider))); ok {
			return spec, nil
		}
	default:
		return siteSpec{}, errors.New("unknown login entry: use workbuddy or workbuddy-ai")
	}
	site := configured
	if raw, present := metadata["site"]; present {
		value, ok := raw.(string)
		if !ok {
			return siteSpec{}, errors.New("invalid login site: use workbuddy or workbuddy-ai")
		}
		site = strings.ToLower(strings.TrimSpace(value))
	}
	if site == "" {
		site = siteWorkBuddy
	}
	spec, ok := configuredSpec(site)
	if !ok {
		return spec, errors.New("invalid login site: use workbuddy or workbuddy-ai")
	}
	return spec, nil
}

// account is what WorkBuddy's sign-in names about the account it just signed in.
type account struct {
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	PhoneNumber    string `json:"phoneNumber"`
	Email          string `json:"email"`
	EnterpriseID   string `json:"enterpriseId"`
	EnterpriseName string `json:"enterpriseName"`
	Type           string `json:"type"`
}

// pollLogin asks WorkBuddy whether the browser sign-in finished, and on success
// reads the account it belongs to.
func pollLogin(req pluginapi.AuthLoginPollRequest, sender httpSender) (pluginapi.AuthLoginPollResponse, error) {
	pending := pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}
	key, err := loadStateKey(req.Host.AuthDir)
	if err != nil {
		return pending, err
	}
	s, err := openState(key, req.State)
	if err != nil {
		return pending, err
	}
	spec, ok := configuredSpec(s.Site)
	if !ok {
		return pending, errors.New("invalid login state")
	}
	if time.Now().Unix() >= s.Expires {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: spec.Name + " sign-in expired; sign in again"}, nil
	}
	vendorState, ok := peekFlow(s.Nonce)
	if !ok {
		// The vendor state lives in this process; a restart between start and
		// poll leaves nothing to ask, and saying so is clearer than polling a
		// state WorkBuddy no longer knows.
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: spec.Name + " sign-in state is gone; sign in again"}, nil
	}
	token, err := pollToken(sender, spec, vendorState)
	if err != nil {
		if pendingPoll(err) {
			return pending, nil
		}
		var refusal *wbError
		if errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: readableFailure(err)}, nil
		}
		// No answer at all: the host may ask again.
		return pending, err
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return pending, nil
	}
	c := merge(credential{Site: spec.ID}, token)
	who, err := readAccount(sender, spec, vendorState, c)
	if err != nil {
		if errors.Is(err, errAccountPending) && strings.TrimSpace(token.UID) == "" {
			// The token is out but the account isn't named yet. Asking again is
			// what WorkBuddy's own client does; the flow stays pending.
			return pending, nil
		}
		if strings.TrimSpace(token.UID) == "" {
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: readableFailure(err)}, nil
		}
	}
	c.UID = strings.TrimSpace(who.UID)
	if c.UID == "" {
		c.UID = strings.TrimSpace(token.UID)
	}
	c.Name = strings.TrimSpace(who.Nickname)
	c.Phone = strings.TrimSpace(who.PhoneNumber)
	c.Email = strings.TrimSpace(who.Email)
	c.EnterpriseID = strings.TrimSpace(who.EnterpriseID)
	c.EnterpriseName = strings.TrimSpace(who.EnterpriseName)
	if c.UID == "" {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: spec.Name + ": the sign-in named no account"}, nil
	}
	takeFlow(s.Nonce)
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: authFor(c)}, nil
}

// pollToken asks the sign-in endpoint once. A token ends the wait; the code
// WorkBuddy sends while the page is still open, a transient failure and a
// non-200 answer all keep it pending.
func pollToken(sender httpSender, spec siteSpec, vendorState string) (tokenAnswer, error) {
	var out tokenAnswer
	raw, err := wbCall(sender, "GET",
		spec.Endpoint+"/v2/plugin/auth/token?state="+url.QueryEscape(vendorState),
		http.Header{"User-Agent": {"WorkBuddy/" + spec.App}}, nil)
	if err != nil {
		if pendingPoll(err) {
			return out, nil
		}
		return out, err
	}
	if json.Unmarshal(raw, &out) != nil {
		return tokenAnswer{}, errors.New(spec.Name + " returned an invalid sign-in answer")
	}
	return out, nil
}

// readAccount reads the account a finished sign-in belongs to. WorkBuddy's own
// client asks by the same state.
func readAccount(sender httpSender, spec siteSpec, vendorState string, c credential) (account, error) {
	var who account
	raw, err := wbCall(sender, "GET",
		spec.Endpoint+"/v2/plugin/login/account?state="+url.QueryEscape(vendorState),
		http.Header{
			"Authorization":      {"Bearer " + c.Access},
			"X-Domain":           {domainOf(spec, c)},
			"X-No-User-Id":       {"true"},
			"X-No-Enterprise-Id": {"true"},
		}, nil)
	if err != nil {
		if pendingPoll(err) {
			return who, fmt.Errorf("%w: %s", errAccountPending, readableFailure(err))
		}
		return who, fmt.Errorf("%s account: %s", spec.Name, readableFailure(err))
	}
	if json.Unmarshal(raw, &who) != nil {
		return who, errors.New(spec.Name + " returned an invalid account answer")
	}
	return who, nil
}

// errAccountPending marks a sign-in whose token is out but whose account WorkBuddy
// has not named yet.
var errAccountPending = errors.New("account not named yet")

// pendingPoll reports an answer that means "ask again", not "it failed": a
// transient status, or the code WorkBuddy sends while the sign-in page is open.
func pendingPoll(err error) bool {
	var e *wbError
	if !errors.As(err, &e) {
		return false
	}
	if e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests || e.Status >= 500 {
		return true
	}
	// 11217: the sign-in isn't finished; 12151: the account isn't named yet.
	return e.Code == "11217" || e.Code == "12151"
}

// authFor maps a credential to the auth record CPA persists.
func authFor(c credential) pluginapi.AuthData {
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256([]byte(c.Site + "\x00" + c.UID))
	id := "workbuddy-" + hex.EncodeToString(sum[:12]) + ".json"
	label := c.label()
	return pluginapi.AuthData{
		Provider:         credentialProvider,
		ID:               id,
		FileName:         id,
		Label:            label,
		StorageJSON:      raw,
		Metadata:         map[string]any{"email": label, "type": credentialProvider, "oauth_provider": c.Site},
		NextRefreshAfter: nextRefresh(c),
	}
}

func nextRefresh(c credential) time.Time {
	now := time.Now()
	at := c.expiry()
	if at.IsZero() {
		return now.Add(refreshLead)
	}
	next := at.Add(-refreshLead)
	if next.Before(now.Add(time.Minute)) {
		return now.Add(time.Minute)
	}
	return next
}

// refreshCredential spends the refresh token. WorkBuddy rotates it, so a
// concurrent call for the same credential waits for the first result instead of
// spending the old token twice.
func refreshCredential(req pluginapi.AuthRefreshRequest, sender httpSender) (pluginapi.AuthRefreshResponse, error) {
	var out pluginapi.AuthRefreshResponse
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return out, errors.New("invalid WorkBuddy credential")
	}
	spec, _ := configuredSpec(c.Site)
	if !c.refreshable(time.Now()) {
		return out, signInExpired(spec)
	}
	refreshed, err := renewToken(req.AuthID, sender, spec, c)
	if err != nil {
		return out, err
	}
	raw, _ := json.Marshal(refreshed)
	out = pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:         credentialProvider,
			ID:               req.AuthID,
			Label:            refreshed.label(),
			StorageJSON:      raw,
			Metadata:         map[string]any{"email": refreshed.label(), "type": credentialProvider, "oauth_provider": refreshed.Site},
			NextRefreshAfter: nextRefresh(refreshed),
		},
		NextRefreshAfter: nextRefresh(refreshed),
	}
	return out, nil
}

type refreshFlight struct {
	from   string
	done   chan struct{}
	result credential
	err    error
}

var refreshMu sync.Mutex
var refreshFlights = map[string]*refreshFlight{}

func renewToken(authID string, sender httpSender, spec siteSpec, c credential) (credential, error) {
	key := authID
	if key == "" {
		key = c.Site + "\x00" + c.UID
	}
	refreshMu.Lock()
	if flight := refreshFlights[key]; flight != nil {
		refreshMu.Unlock()
		<-flight.done
		if flight.from == c.Refresh {
			if flight.err != nil {
				return credential{}, flight.err
			}
			return flight.result, nil
		}
		// The saved pair changed while we waited; ask with the caller's own.
		return renewOnce(sender, spec, c)
	}
	flight := &refreshFlight{from: c.Refresh, done: make(chan struct{})}
	refreshFlights[key] = flight
	refreshMu.Unlock()

	flight.result, flight.err = renewOnce(sender, spec, c)
	refreshMu.Lock()
	delete(refreshFlights, key)
	refreshMu.Unlock()
	close(flight.done)
	if flight.err != nil {
		return credential{}, flight.err
	}
	return flight.result, nil
}

func renewOnce(sender httpSender, spec siteSpec, c credential) (credential, error) {
	headers := http.Header{
		"User-Agent":            {"WorkBuddy/" + spec.App},
		"X-Refresh-Token":       {c.Refresh},
		"X-Auth-Refresh-Source": {"plugin"},
		"X-Domain":              {domainOf(spec, c)},
	}
	if id := strings.TrimSpace(c.EnterpriseID); id != "" {
		headers.Set("X-Enterprise-Id", id)
		headers.Set("X-Tenant-Id", id)
	}
	raw, err := wbCall(sender, "POST", spec.Endpoint+"/v2/plugin/auth/token/refresh", headers, map[string]any{})
	if err != nil {
		if status := statusOf(err); status == http.StatusUnauthorized || status == http.StatusForbidden {
			// A refresh WorkBuddy refuses is a sign-in that is gone, whether it
			// says invalid_grant or only 401: nothing else can renew it.
			return credential{}, signInExpired(spec)
		}
		return credential{}, fmt.Errorf("%s token refresh failed: %s", spec.Name, readableFailure(err))
	}
	var got tokenAnswer
	if json.Unmarshal(raw, &got) != nil {
		return credential{}, errors.New(spec.Name + " token refresh returned an invalid answer")
	}
	if strings.TrimSpace(got.AccessToken) == "" {
		return credential{}, errors.New(spec.Name + " token refresh returned no token")
	}
	return merge(c, got), nil
}

// readableFailure is an upstream failure in words a person can act on, without
// token bytes or internal paths.
func readableFailure(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// parseAuth recognizes an existing "type": "workbuddy" auth file and republishes
// it.
func parseAuth(req pluginapi.AuthParseRequest) pluginapi.AuthParseResponse {
	// siteWorkBuddy and credentialProvider are the same key: the China build's
	// entry id is also the credential type every auth file carries.
	switch strings.ToLower(strings.TrimSpace(req.Provider)) {
	case "", credentialProvider, siteWorkBuddyAI, pluginStoreID:
	default:
		return pluginapi.AuthParseResponse{}
	}
	var metadata struct {
		Type  string `json:"type"`
		Email string `json:"email"`
	}
	if json.Unmarshal(req.RawJSON, &metadata) != nil || metadata.Type != credentialProvider {
		return pluginapi.AuthParseResponse{}
	}
	c, err := readCredential(req.RawJSON)
	if err != nil {
		return pluginapi.AuthParseResponse{}
	}
	name := req.FileName
	if name == "" {
		name = req.Path
	}
	label := strings.TrimSpace(metadata.Email)
	if label == "" {
		label = c.label()
	}
	a := pluginapi.AuthData{
		Provider:    credentialProvider,
		ID:          name,
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: append([]byte(nil), req.RawJSON...),
		Metadata:    map[string]any{"email": label, "type": credentialProvider, "oauth_provider": c.Site},
	}
	a.NextRefreshAfter = nextRefresh(c)
	return pluginapi.AuthParseResponse{Handled: true, Auth: a}
}
