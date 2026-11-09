package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func testCheckinCredential(uid, site string) credential {
	return credential{
		UID: uid, Site: site, Access: "test-access-token",
		Expires: time.Now().Add(time.Hour).UnixMilli(),
	}
}

func TestNextCheckinTimeUsesBeijingClock(t *testing.T) {
	cases := []struct {
		now, want string
	}{
		{"2026-11-09T08:30:00+08:00", "2026-11-09T09:00:00+08:00"},
		{"2026-11-09T09:00:00+08:00", "2026-11-09T21:00:00+08:00"},
		{"2026-11-09T22:00:00+08:00", "2026-11-10T09:00:00+08:00"},
	}
	for _, tc := range cases {
		now, _ := time.Parse(time.RFC3339, tc.now)
		if got := nextCheckinTime(now).Format(time.RFC3339); got != tc.want {
			t.Errorf("at %s: next=%s, want %s", tc.now, got, tc.want)
		}
	}
}

func TestAutoCheckinClaimedOncePerBeijingDay(t *testing.T) {
	c := testCheckinCredential(t.Name(), siteWorkBuddy)
	start := time.Date(2026, 11, 9, 10, 0, 0, 0, checkinLocation)
	calls := 0
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		if req.Headers.Get("Authorization") != "Bearer "+c.Access {
			t.Errorf("missing bearer on %s", req.URL)
		}
		switch {
		case strings.HasSuffix(req.URL, checkinStatusPath):
			return jsonReply(200, `{"code":0,"data":{"active":true,"today_checked_in":false}}`), nil
		case strings.HasSuffix(req.URL, checkinClaimPath):
			return jsonReply(200, `{"code":0,"data":{"credit":100}}`), nil
		default:
			t.Errorf("unexpected endpoint: %s", req.URL)
			return jsonReply(500, `{}`), nil
		}
	}}
	result, err := autoCheckinAccount(sender, c, start)
	if err != nil || result != "claimed" {
		t.Fatalf("first attempt: %s %v", result, err)
	}
	result, err = autoCheckinAccount(sender, c, start.Add(time.Hour))
	if err != nil || result != "already" || calls != 2 {
		t.Fatalf("same day: %s %v; calls=%d", result, err, calls)
	}
	result, err = autoCheckinAccount(sender, c, start.Add(24*time.Hour))
	if err != nil || result != "claimed" || calls != 4 {
		t.Fatalf("next day: %s %v; calls=%d", result, err, calls)
	}
}

func TestAutoCheckinSkipsAlreadyChecked(t *testing.T) {
	c := testCheckinCredential(t.Name(), siteWorkBuddy)
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, checkinStatusPath) {
			t.Errorf("unwanted claim %s", req.URL)
		}
		return jsonReply(200, `{"code":0,"data":{"active":true,"todayCheckedIn":true}}`), nil
	}}
	result, err := autoCheckinAccount(sender, c, time.Now())
	if err != nil || result != "already" || sender.count() != 1 {
		t.Fatalf("result=%s err=%v calls=%d", result, err, sender.count())
	}
}

func TestAutoCheckinHandlesAlreadyClaimedBusinessCode(t *testing.T) {
	c := testCheckinCredential(t.Name(), siteWorkBuddy)
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, checkinStatusPath) {
			return jsonReply(200, `{"code":0,"data":{"active":true,"today_checked_in":false}}`), nil
		}
		return jsonReply(400, `{"code":10001,"msg":"今天已签到，请明天再来"}`), nil
	}}
	result, err := autoCheckinAccount(sender, c, time.Now())
	if err != nil || result != "already" {
		t.Fatalf("result=%s err=%v", result, err)
	}
}

func TestAutoCheckinRetriesWhenStatusFails(t *testing.T) {
	c := testCheckinCredential(t.Name(), siteWorkBuddy)
	calls := 0
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		if calls == 1 {
			return pluginapi.HTTPResponse{}, errors.New("offline")
		}
		if strings.HasSuffix(req.URL, checkinStatusPath) {
			return jsonReply(200, `{"code":0,"data":{"active":true,"today_checked_in":false}}`), nil
		}
		return jsonReply(200, `{"code":0,"data":null}`), nil
	}}
	if _, err := autoCheckinAccount(sender, c, time.Now()); err == nil {
		t.Fatal("expected transient status failure")
	}
	result, err := autoCheckinAccount(sender, c, time.Now())
	if err != nil || result != "claimed" || calls != 3 {
		t.Fatalf("result=%s err=%v calls=%d", result, err, calls)
	}
}

func TestAutoCheckinExcludesInternationalAccounts(t *testing.T) {
	c := testCheckinCredential(t.Name(), siteWorkBuddyAI)
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		t.Fatalf("international account should not call CN endpoint: %s", req.URL)
		return pluginapi.HTTPResponse{}, nil
	}}
	result, err := autoCheckinAccount(sender, c, time.Now())
	if err != nil || result != "unsupported" || sender.count() != 0 {
		t.Fatalf("result=%s err=%v calls=%d", result, err, sender.count())
	}
}

func TestAutoCheckinRequiresExplicitEligibility(t *testing.T) {
	c := testCheckinCredential(t.Name(), siteWorkBuddy)
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, checkinClaimPath) {
			t.Fatal("unexpected claim")
		}
		return jsonReply(200, `{"code":0,"data":{"active":false,"today_checked_in":false}}`), nil
	}}
	result, err := autoCheckinAccount(sender, c, time.Now())
	if err != nil || result != "inactive" || sender.count() != 1 {
		t.Fatalf("result=%s err=%v calls=%d", result, err, sender.count())
	}
}
