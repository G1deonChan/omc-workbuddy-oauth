package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const paidSummary = `{
	"IsPaidUser": true,
	"SubscriptionPackageCode": "sub-code",
	"CreditGroups": [
		{"Kind": "plan_base", "CycleTotalCapacity": "500", "CycleUsedCapacity": "125"},
		{"Kind": "benefit", "CycleTotalCapacity": "40.5", "CycleUsedCapacity": "0.5"}
	],
	"Packages": [
		{"PackageCode": "sub-code", "CapacityType": "1", "CycleTotalCapacity": "500", "CycleUsedCapacity": "125", "CycleStartTime": "2026-10-01 00:00:00", "CycleEndTime": "2026-11-01 00:00:00"}
	]
}`

func TestSummaryWindowsSplitPlanAndExtraCredits(t *testing.T) {
	summary, err := readSummary([]byte(paidSummary))
	if err != nil {
		t.Fatal(err)
	}
	windows := summaryWindows(summary)
	if len(windows) != 3 {
		t.Fatalf("windows = %+v", windows)
	}
	if windows[0].Name != "Credits" || windows[0].Total != 500 || windows[0].Left != 375 {
		t.Fatalf("plan window = %+v", windows[0])
	}
	if windows[1].Name != "Extra credits" || windows[1].Total != 40.5 || windows[1].Left != 40 {
		t.Fatalf("extra window = %+v", windows[1])
	}
	if windows[2].Name != "Total credits" || windows[2].Left != 415 {
		t.Fatalf("total window = %+v", windows[2])
	}
	if windows[2].Reset != "2026-10-31T16:00:00Z" {
		t.Fatalf("reset = %q", windows[2].Reset)
	}
}

func TestSummaryWindowsReadAFreeAccountsSlice(t *testing.T) {
	summary, err := readSummary([]byte(`{
		"IsPaidUser": false,
		"SubscriptionPackageCode": "",
		"Packages": [
			{"PackageCode": "free-code", "CapacityType": 4, "CycleTotalCapacity": 500, "CycleUsedCapacity": 20, "CycleRemainCapacity": 480, "CycleEndTime": "2026-11-01 00:00:00"},
			{"PackageCode": "gift-code", "CapacityType": 1, "CycleRemainCapacity": "12.5"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	windows := summaryWindows(summary)
	if len(windows) != 2 {
		t.Fatalf("windows = %+v", windows)
	}
	if windows[0].Name != "Credits" || windows[0].Left != 480 || windows[0].Reset != "2026-10-31T16:00:00Z" {
		t.Fatalf("credits = %+v", windows[0])
	}
	if windows[1].Name != "Prepaid credits" || windows[1].Key != "prepaid_credits" {
		t.Fatalf("prepaid = %+v", windows[1])
	}
}

func TestQuotaFetchNormalizesAndNamesThePlan(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, summaryPath) {
			t.Fatalf("quota URL = %s", req.URL)
		}
		if req.Headers.Get("Authorization") != "Bearer at-test-token" || req.Headers.Get("X-Product") != "SaaS" {
			t.Fatalf("quota headers = %v", req.Headers)
		}
		return jsonReply(200, `{"code":0,"data":`+paidSummary+`}`), nil
	}}
	out, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: testCredential(siteWorkBuddy)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if out.Subscription == nil || out.Subscription.Plan != "Paid plan" || out.Subscription.TierName != "WorkBuddy" {
		t.Fatalf("subscription = %+v", out.Subscription)
	}
	if len(out.Groups) != 1 || out.Groups[0].DisplayName != "WorkBuddy" || len(out.Groups[0].Buckets) != 3 {
		t.Fatalf("groups = %+v", out.Groups)
	}
	plan := out.Groups[0].Buckets[0]
	if plan.Window != "Credits" || plan.RemainingFraction != 0.75 || plan.Description != "375 / 500 credits left" {
		t.Fatalf("plan bucket = %+v", plan)
	}
	if len(out.Summary) != 3 || out.Summary[2].Key != "total_credits" || out.Summary[2].Value != 415 {
		t.Fatalf("summary = %+v", out.Summary)
	}
}

func TestQuotaFetchUsesTheEnterpriseMeter(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, enterpriseUsagePath):
			if req.Headers.Get("X-Enterprise-Id") != "ent-1" || req.Headers.Get("X-Tenant-Id") != "ent-1" {
				t.Fatalf("enterprise headers = %v", req.Headers)
			}
			return jsonReply(200, `{"code":0,"data":{"limitNum":"10000","credit":"2500","cycleStartTime":"2026-10-01 00:00:00","cycleResetTime":"2026-11-01 00:00:00"}}`), nil
		default:
			return jsonReply(200, `{"code":0,"data":{"IsPaidUser":true,"Packages":[]}}`), nil
		}
	}}
	storage, _ := json.Marshal(credential{Site: siteWorkBuddy, Access: "at", UID: "u-1", EnterpriseID: "ent-1", Expires: time.Now().Add(time.Hour).UnixMilli()})
	out, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: storage}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if out.Subscription.Plan != "Enterprise" {
		t.Fatalf("plan = %q", out.Subscription.Plan)
	}
	if len(out.Groups) != 1 || len(out.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %+v", out.Groups)
	}
	bucket := out.Groups[0].Buckets[0]
	if bucket.RemainingFraction != 0.75 || bucket.Description != "7,500 / 10,000 credits left" {
		t.Fatalf("bucket = %+v", bucket)
	}
	if bucket.ResetTime != "2026-10-31T16:00:00Z" {
		t.Fatalf("reset = %q", bucket.ResetTime)
	}
}

func TestQuotaFetchDoesNotSpendTheRefreshToken(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.Contains(req.URL, "/v2/plugin/auth/token/refresh") {
			t.Fatal("the quota read spent the credential's refresh token")
		}
		return jsonReply(401, `{"code":401,"msg":"invalid_token"}`), nil
	}}
	_, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: testCredential(siteWorkBuddy)}, sender)
	if err == nil || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("err = %v", err)
	}
	if sender.count() != 1 {
		t.Fatalf("the quota read asked %d times", sender.count())
	}
}

func TestQuotaFetchReportsAFreeAccountWithNoCredits(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonReply(200, `{"code":0,"data":{"IsPaidUser":false,"Packages":[]}}`), nil
	}}
	out, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: testCredential(siteWorkBuddy)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if out.Subscription.Plan != "Free plan" || len(out.Groups) != 1 || len(out.Groups[0].Buckets) != 1 {
		t.Fatalf("quota = %+v", out)
	}
	if out.Groups[0].Buckets[0].Description != "0 credits left" {
		t.Fatalf("bucket = %+v", out.Groups[0].Buckets[0])
	}
}

func TestQuotaFetchSaysWhenTheSignInIsGone(t *testing.T) {
	resetPluginState()
	storage, _ := json.Marshal(credential{Site: siteWorkBuddy, Access: "at", UID: "u-1"})
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonReply(401, `{"code":401,"msg":"invalid_token"}`), nil
	}}
	_, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: storage}, sender)
	if err == nil || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("err = %v", err)
	}
}

func TestFormatAmountReadsAsAPanelDoes(t *testing.T) {
	cases := map[float64]string{0: "0", 500: "500", 12345: "12,345", 40.5: "40.50", 1234567.25: "1,234,567.25"}
	for value, want := range cases {
		if got := formatAmount(value); got != want {
			t.Fatalf("formatAmount(%v) = %q, want %q", value, got, want)
		}
	}
}
