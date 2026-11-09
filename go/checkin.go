package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	checkinStatusPath   = "/v2/billing/meter/checkin-activity-status"
	legacyStatusPath    = "/v2/billing/meter/checkin-status"
	checkinClaimPath    = "/v2/billing/meter/daily-checkin"
	checkinStartupDelay = 30 * time.Second
)

// WorkBuddy's China account activity is measured in UTC+08:00, independent
// of the server's timezone. The international site has no daily CN claim.
var checkinLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// State deliberately contains no tokens or personal information. A restart
// rechecks the upstream status, so persistence is unnecessary.
type checkinDayState struct {
	mu  sync.Mutex
	day string
}

var checkinAccounts sync.Map // key (site, user, tenant) -> *checkinDayState
var checkinSchedulerOnce sync.Once
var checkinRunMu sync.Mutex

func checkinKey(c credential) string {
	return c.Site + "\x00" + c.UID + "\x00" + c.EnterpriseID
}

func beijingDay(now time.Time) string {
	return now.In(checkinLocation).Format("2006-01-02")
}

func nextCheckinTime(now time.Time) time.Time {
	local := now.In(checkinLocation)
	for _, h := range []int{9, 21} {
		candidate := time.Date(local.Year(), local.Month(), local.Day(), h, 0, 0, 0, checkinLocation)
		if candidate.After(local) {
			return candidate
		}
	}
	return time.Date(local.Year(), local.Month(), local.Day()+1, 9, 0, 0, 0, checkinLocation)
}

// ensureCheckinScheduler is invoked only after an explicitly enabled config.
// It never refreshes OAuth tokens: rotating refresh credentials must remain
// the host's responsibility, or the saved refresh token could be invalidated.
func ensureCheckinScheduler() {
	checkinSchedulerOnce.Do(func() { go checkinScheduleLoop() })
}

func checkinScheduleLoop() {
	// Recover a missed daily claim after a CPA restart, but give the host time
	// to load credentials and finish plugin registration first.
	time.Sleep(checkinStartupDelay)
	runAutoCheckin()
	for {
		delay := time.Until(nextCheckinTime(time.Now()))
		if delay < time.Second {
			delay = time.Second
		}
		time.Sleep(delay)
		runAutoCheckin()
	}
}

func checkinLog(level, message string, fields map[string]any) {
	// Never include credential contents, account labels, or upstream bodies.
	_ = hostRPC("host.log", map[string]any{
		"level": level, "message": message, "fields": fields,
	}, nil)
}

// runAutoCheckin uses host.auth.list/get, never scans the auth directory or
// keeps copied tokens. Disabled credentials remain eligible: check-in could
// replenish the credits which led the user to disable a credential.
func runAutoCheckin() {
	if !getConfig().CheckinAuto {
		return
	}
	checkinRunMu.Lock()
	defer checkinRunMu.Unlock()

	var listing struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := hostRPC("host.auth.list", map[string]any{}, &listing); err != nil {
		checkinLog("warn", "WorkBuddy check-in: host auth listing unavailable", nil)
		return
	}

	attempted, claimed, already, skipped, failed := 0, 0, 0, 0, 0
	for _, f := range listing.Files {
		if f.AuthIndex == "" || (f.Type != "" && f.Type != credentialProvider) || (f.Provider != "" && f.Provider != credentialProvider) {
			continue
		}
		// Do not try unrelated type-less files. The JSON type is still
		// independently verified after retrieval.
		if f.Type == "" && f.Provider == "" && !strings.HasPrefix(strings.ToLower(f.Name), "workbuddy-") {
			continue
		}
		var file pluginapi.HostAuthGetResponse
		if err := hostRPC("host.auth.get", pluginapi.HostAuthGetRequest{AuthIndex: f.AuthIndex}, &file); err != nil {
			failed++
			continue
		}
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(file.JSON, &typed) != nil || typed.Type != credentialProvider {
			continue
		}
		c, err := readCredential(file.JSON)
		if err != nil || c.Site != siteWorkBuddy {
			continue
		}
		if expiry := c.expiry(); !expiry.IsZero() && !expiry.After(time.Now()) {
			// Do not spend a rotating refresh token here. CPA will renew it.
			failed++
			continue
		}
		attempted++
		outcome, err := autoCheckinAccount(cpaSender{}, c, time.Now())
		if err != nil {
			failed++
			continue
		}
		switch outcome {
		case "claimed":
			claimed++
		case "already":
			already++
		default:
			skipped++
		}
	}
	checkinLog("info", "WorkBuddy automatic daily check-in complete", map[string]any{
		"attempted": attempted, "claimed": claimed, "already": already,
		"skipped": skipped, "failed": failed,
	})
}

// autoCheckinAccount is shared by the periodic job and by the quota-fetch
// fallback. It serializes per account, avoids a second claim on the same
// Beijing day, and uses the host's HTTP bridge rather than an independent
// HTTP client. A failed probe/claim is retried at the next opportunity.
func autoCheckinAccount(sender httpSender, c credential, now time.Time) (string, error) {
	if c.Site != siteWorkBuddy {
		return "unsupported", nil
	}
	key := checkinKey(c)
	value, _ := checkinAccounts.LoadOrStore(key, &checkinDayState{})
	state := value.(*checkinDayState)
	state.mu.Lock()
	defer state.mu.Unlock()

	day := beijingDay(now)
	if state.day == day {
		return "already", nil
	}
	spec, ok := configuredSpec(c.Site)
	if !ok {
		return "", errors.New("unknown WorkBuddy account site")
	}

	statusRaw, err := meter(sender, spec, c, checkinStatusPath, nil)
	if statusOf(err) == http.StatusNotFound {
		statusRaw, err = meter(sender, spec, c, legacyStatusPath, nil)
	}
	if err != nil {
		return "", fmt.Errorf("check-in status request failed")
	}
	var status struct {
		Active           *bool `json:"active"`
		TodayCheckedIn   *bool `json:"today_checked_in"`
		TodayCheckedInJS *bool `json:"todayCheckedIn"`
	}
	if json.Unmarshal(statusRaw, &status) != nil || status.Active == nil {
		return "", errors.New("invalid check-in activity status")
	}
	if !*status.Active {
		state.day = day
		return "inactive", nil
	}
	checked := status.TodayCheckedIn
	if checked == nil {
		checked = status.TodayCheckedInJS
	}
	if checked == nil {
		return "", errors.New("check-in status did not specify today's state")
	}
	if *checked {
		state.day = day
		return "already", nil
	}

	_, err = meter(sender, spec, c, checkinClaimPath, nil)
	if err != nil {
		var upstream *wbError
		if errors.As(err, &upstream) && (upstream.Code == "10001" || strings.Contains(upstream.Message, "已签到") || strings.Contains(strings.ToLower(upstream.Message), "already checked")) {
			state.day = day
			return "already", nil
		}
		return "", errors.New("daily check-in claim failed")
	}
	state.day = day
	return "claimed", nil
}
