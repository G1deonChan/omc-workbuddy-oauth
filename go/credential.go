package main

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// WorkBuddy ships as two builds that sign in to two hosts. Both produce the same
// credential shape ("type": "workbuddy") and share one executor; the site field
// records which build signed the account in.
const (
	siteWorkBuddy   = "workbuddy"
	siteWorkBuddyAI = "workbuddy-ai"

	// credentialProvider is the provider key every credential this plugin writes
	// carries, and the identifier CPA routes executions and quota by.
	credentialProvider = "workbuddy"
)

// siteSpec is one build: the hosts it talks to, the client it claims to be, and
// the models it ships when its own catalogue cannot be read.
type siteSpec struct {
	ID   string
	Name string
	// Endpoint is the build's API host: every plugin and billing path hangs off it.
	Endpoint string
	// Platform is the build name WorkBuddy's sign-in page is asked for.
	Platform string
	// App, CLI and AppName make up the User-Agent WorkBuddy's desktop sends.
	// A request whose User-Agent does not start "WorkBuddy/<app>" is refused
	// with error 10085, so they are configuration rather than constants.
	App     string
	CLI     string
	AppName string
	// AI marks the international build, whose GLM models drop image input.
	AI bool
	// Fallback lists the models the build ships when /v3/config cannot be read.
	Fallback []workbuddyModelSeed
}

var sites = []siteSpec{
	{
		ID:       siteWorkBuddy,
		Name:     "WorkBuddy",
		Endpoint: "https://copilot.tencent.com",
		Platform: "workbuddy",
		App:      defaultAppVersion,
		CLI:      defaultCLIVersion,
		AppName:  "WorkBuddy",
		Fallback: cnFallbackCatalogue,
	},
	{
		ID:       siteWorkBuddyAI,
		Name:     "WorkBuddy AI",
		Endpoint: "https://www.workbuddy.ai",
		Platform: "workbuddy-ai",
		App:      defaultAppVersion,
		CLI:      defaultCLIVersion,
		AppName:  "workbuddy-ai",
		AI:       true,
		Fallback: aiFallbackCatalogue,
	},
}

func siteFor(id string) (siteSpec, bool) {
	for _, s := range sites {
		if s.ID == id {
			return s, true
		}
	}
	return siteSpec{}, false
}

// credential is the persisted auth storage. Tokens are opaque strings; Expires
// is the access token's end in milliseconds since the epoch, zero when WorkBuddy
// named no end.
type credential struct {
	Site           string `json:"site"`
	Access         string `json:"access,omitempty"`
	Refresh        string `json:"refresh,omitempty"`
	Expires        int64  `json:"expires,omitempty"`
	RefreshExpires int64  `json:"refresh_expires,omitempty"`
	Domain         string `json:"domain,omitempty"`
	TokenType      string `json:"token_type,omitempty"`
	UID            string `json:"uid"`
	Email          string `json:"email,omitempty"`
	Name           string `json:"name,omitempty"`
	Phone          string `json:"phone,omitempty"`
	EnterpriseID   string `json:"enterprise_id,omitempty"`
	EnterpriseName string `json:"enterprise_name,omitempty"`
}

func readCredential(b []byte) (credential, error) {
	var c credential
	if json.Unmarshal(b, &c) != nil {
		return c, errors.New("invalid WorkBuddy credential")
	}
	if _, ok := siteFor(c.Site); !ok {
		return c, errors.New("invalid WorkBuddy credential")
	}
	if strings.TrimSpace(c.UID) == "" || strings.TrimSpace(c.Access) == "" {
		return c, errors.New("invalid WorkBuddy credential")
	}
	return c, nil
}

// expiry is when the access token runs out, zero when unknown.
func (c credential) expiry() time.Time {
	if c.Expires <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.Expires).UTC()
}

// refreshable reports a refresh token WorkBuddy will still spend.
func (c credential) refreshable(now time.Time) bool {
	if strings.TrimSpace(c.Refresh) == "" {
		return false
	}
	return c.RefreshExpires <= 0 || now.UnixMilli() < c.RefreshExpires
}

// label is the account name a card shows: the person, and for an enterprise
// (team) account the enterprise too. One person's personal and enterprise
// accounts share the uid and the nickname, so a name without the enterprise
// would let one replace the other in a panel that keeps one account per name.
func (c credential) label() string {
	person := strings.TrimSpace(c.Name)
	if person == "" {
		person = maskPhone(c.Phone)
	}
	if person == "" {
		person = strings.TrimSpace(c.Email)
	}
	if person == "" {
		person = strings.TrimSpace(c.UID)
	}
	team := strings.TrimSpace(c.EnterpriseName)
	if team == "" && strings.TrimSpace(c.EnterpriseID) != "" {
		team = enterpriseFallbackName(c.EnterpriseID)
	}
	if team == "" {
		return person
	}
	if person == "" {
		return team
	}
	return person + " · " + team
}

// enterpriseFallbackName names an enterprise WorkBuddy names no name for by the
// end of its id, which is short enough to read and distinct between accounts.
func enterpriseFallbackName(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	tail := id
	if len(tail) > 6 {
		tail = tail[len(tail)-6:]
	}
	return "enterprise " + tail
}

// maskPhone hides the middle of a mobile number, as WorkBuddy's own panels do.
func maskPhone(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if strings.HasPrefix(digits, "0086") && len(digits)-4 >= 7 {
		digits = digits[4:]
	} else if strings.HasPrefix(digits, "86") && len(digits)-2 >= 7 {
		digits = digits[2:]
	}
	if len(digits) < 11 {
		return ""
	}
	return digits[:3] + "****" + digits[len(digits)-4:]
}

// chatPath is the chat completion endpoint under a build's host.
const chatPath = "/v2/chat/completions"

// modelsPath is the product config the account's model list is read from.
const modelsPath = "/v3/config"

// summaryPath is the billing meter the quota card is read from. It carries no
// /v2 prefix, unlike the chat and the plugin API.
const summaryPath = "/billing/meter/get-user-resource-summary"

// enterpriseUsagePath is the meter WorkBuddy's own usage page reads for a team
// account, whose resource summary answers with no packages at all.
const enterpriseUsagePath = "/billing/meter/get-enterprise-user-usage"
