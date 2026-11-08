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
	windows := summaryWindows(summary, summaryReset(summary))
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
	windows := summaryWindows(summary, summaryReset(summary))
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
	// The subscription's cycle and the free slice's are different instants, so a
	// reading that picked the slice for a paid account would show the wrong reset.
	cycleEnd := time.Now().AddDate(0, 0, 20).Truncate(time.Second)
	sliceEnd := time.Now().AddDate(0, 0, 3).Truncate(time.Second)
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, summaryPath):
			if req.Headers.Get("Authorization") != "Bearer at-test-token" || req.Headers.Get("X-Product") != "SaaS" {
				t.Fatalf("quota headers = %v", req.Headers)
			}
			return jsonReply(200, `{"code":0,"data":`+paidSummary+`}`), nil
		case strings.HasSuffix(req.URL, packagesPath):
			return jsonReply(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"Accounts":[
				{"PackageCode":"sub-code","CapacityType":2,"CycleEndTime":"`+beijingWall(cycleEnd)+`"},
				{"PackageCode":"free-code","CapacityType":4,"CycleEndTime":"`+beijingWall(sliceEnd)+`"}
			]}}}}`), nil
		default:
			t.Fatalf("quota URL = %s", req.URL)
			return pluginapi.HTTPResponse{}, nil
		}
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
	want := cycleEnd.UTC().Format(time.RFC3339)
	if plan.ResetTime != want {
		t.Fatalf("plan reset = %q, want %q", plan.ResetTime, want)
	}
	if out.Groups[0].Buckets[1].ResetTime != "" {
		t.Fatalf("extra credits reset = %q, want none", out.Groups[0].Buckets[1].ResetTime)
	}
	if out.Groups[0].Buckets[2].ResetTime != want {
		t.Fatalf("total reset = %q, want %q", out.Groups[0].Buckets[2].ResetTime, want)
	}
	if len(out.Summary) != 3 || out.Summary[2].Key != "total_credits" || out.Summary[2].Value != 415 {
		t.Fatalf("summary = %+v", out.Summary)
	}
}

// summaryWithoutCycles is a real /billing/meter/get-user-resource-summary payload
// (2026-11-08, reduced to the fields read here). Its packages carry amounts only:
// no CycleStartTime, no CycleEndTime, no CapacityType. That is why the reset
// cannot be read from it, however the packages are split.
const summaryWithoutCycles = `{
	"IsPaidUser": false,
	"SubscriptionPackageCode": "",
	"Packages": [
		{"PackageCode": "TCACA_code_006_DbXS0lrypC", "CycleTotalCapacity": "250", "CycleRemainCapacity": "0", "CycleUsedCapacity": "250"},
		{"PackageCode": "TCACA_code_035_ArVxJcGDsm", "CycleTotalCapacity": "100", "CycleRemainCapacity": "0", "CycleUsedCapacity": "100"}
	],
	"CreditGroups": [
		{"Kind": "plan_base", "CycleTotalCapacity": "100", "CycleUsedCapacity": "100"},
		{"Kind": "benefit", "CycleTotalCapacity": "280", "CycleUsedCapacity": "280"}
	]
}`

func TestQuotaFetchReadsAFreeAccountsResetFromTheSlice(t *testing.T) {
	resetPluginState()
	cycleEnd := time.Now().AddDate(0, 0, 22).Truncate(time.Second)
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, summaryPath):
			return jsonReply(200, `{"code":0,"data":`+summaryWithoutCycles+`}`), nil
		case strings.HasSuffix(req.URL, packagesPath):
			// A summary that names no subscription is a free account: the plan's
			// own credits are its CapacityType 4 slice, and a bonus pack that ends
			// sooner must not be mistaken for the plan's cycle.
			return jsonReply(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"Accounts":[
				{"PackageCode":"TCACA_code_006_DbXS0lrypC","CapacityType":1,"CycleEndTime":"`+beijingWall(time.Now().AddDate(0, 0, 2))+`"},
				{"PackageCode":"TCACA_code_035_ArVxJcGDsm","CapacityType":4,"CycleEndTime":"`+beijingWall(cycleEnd)+`"}
			]}}}}`), nil
		default:
			t.Fatalf("quota URL = %s", req.URL)
			return pluginapi.HTTPResponse{}, nil
		}
	}}
	out, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: testCredential(siteWorkBuddy)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if out.Subscription == nil || out.Subscription.Plan != "Free plan" {
		t.Fatalf("subscription = %+v", out.Subscription)
	}
	buckets := out.Groups[0].Buckets
	if len(buckets) != 3 {
		t.Fatalf("buckets = %+v", buckets)
	}
	want := cycleEnd.UTC().Format(time.RFC3339)
	if buckets[0].ResetTime != want || buckets[2].ResetTime != want {
		t.Fatalf("resets = %q / %q, want %q", buckets[0].ResetTime, buckets[2].ResetTime, want)
	}
	if buckets[1].ResetTime != "" {
		t.Fatalf("extra credits reset = %q, want none", buckets[1].ResetTime)
	}
}

func TestQuotaFetchAsksThePackageMeterAsWorkBuddysClientDoes(t *testing.T) {
	resetPluginState()
	var body map[string]any
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, packagesPath) {
			if json.Unmarshal(req.Body, &body) != nil {
				t.Fatalf("package body = %s", req.Body)
			}
			return jsonReply(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":0,"Accounts":[]}}}}`), nil
		}
		return jsonReply(200, `{"code":0,"data":`+summaryWithoutCycles+`}`), nil
	}}
	if _, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: testCredential(siteWorkBuddy)}, sender); err != nil {
		t.Fatal(err)
	}
	if body["ProductCode"] != "p_tcaca" {
		t.Fatalf("product = %v", body["ProductCode"])
	}
	if page, _ := body["PageNumber"].(float64); page != 1 {
		t.Fatalf("page number = %v", body["PageNumber"])
	}
	if size, _ := body["PageSize"].(float64); size <= 0 {
		t.Fatalf("page size = %v", body["PageSize"])
	}
	status, _ := body["Status"].([]any)
	if len(status) != 2 || status[0] != float64(0) || status[1] != float64(3) {
		t.Fatalf("status = %v", body["Status"])
	}
	// The range is WorkBuddy's own wall clock, so a UTC host must not shift it.
	begin, _ := body["PackageEndTimeRangeBegin"].(string)
	if _, err := time.ParseInLocation("2006-01-02 15:04:05", begin, beijingZone); err != nil {
		t.Fatalf("range begin = %q", begin)
	}
}

func TestQuotaFetchFallsBackToTheVersionedPackageMeter(t *testing.T) {
	resetPluginState()
	cycleEnd := time.Now().AddDate(0, 0, 15).Truncate(time.Second)
	var asked []string
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		asked = append(asked, req.URL)
		switch {
		case strings.HasSuffix(req.URL, summaryPath):
			return jsonReply(200, `{"code":0,"data":`+summaryWithoutCycles+`}`), nil
		case strings.HasSuffix(req.URL, "/v2"+packagesPath):
			return jsonReply(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"Accounts":[
				{"PackageCode":"TCACA_code_035_ArVxJcGDsm","CapacityType":4,"CycleEndTime":"`+beijingWall(cycleEnd)+`"}
			]}}}}`), nil
		case strings.HasSuffix(req.URL, packagesPath):
			// The China build has served this meter under /v2 alone.
			return jsonReply(404, `{"code":404,"msg":"not found"}`), nil
		default:
			t.Fatalf("quota URL = %s", req.URL)
			return pluginapi.HTTPResponse{}, nil
		}
	}}
	out, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: testCredential(siteWorkBuddy)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.Groups[0].Buckets[0].ResetTime, cycleEnd.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("reset = %q, want %q", got, want)
	}
	if len(asked) != 3 || !strings.HasSuffix(asked[1], packagesPath) || !strings.HasSuffix(asked[2], "/v2"+packagesPath) {
		t.Fatalf("asked %v", asked)
	}
}

func TestQuotaFetchLeavesTheResetUnknownWhenTheCycleIsOver(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, packagesPath) {
			return jsonReply(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"Accounts":[
				{"PackageCode":"TCACA_code_035_ArVxJcGDsm","CapacityType":4,"CycleEndTime":"`+beijingWall(time.Now().AddDate(0, 0, -1))+`"}
			]}}}}`), nil
		}
		return jsonReply(200, `{"code":0,"data":`+summaryWithoutCycles+`}`), nil
	}}
	out, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: testCredential(siteWorkBuddy)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	for _, bucket := range out.Groups[0].Buckets {
		if bucket.ResetTime != "" {
			t.Fatalf("bucket %q reset = %q, want none", bucket.Window, bucket.ResetTime)
		}
	}
}

func TestQuotaFetchKeepsTheReadingWhenThePackageMeterFails(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, packagesPath) {
			return jsonReply(500, `{"code":500,"msg":"boom"}`), nil
		}
		return jsonReply(200, `{"code":0,"data":`+summaryWithoutCycles+`}`), nil
	}}
	out, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: testCredential(siteWorkBuddy)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Groups[0].Buckets) != 3 || out.Groups[0].Buckets[2].Description != "0 / 380 credits left" {
		t.Fatalf("buckets = %+v", out.Groups[0].Buckets)
	}
	for _, bucket := range out.Groups[0].Buckets {
		if bucket.ResetTime != "" {
			t.Fatalf("bucket %q reset = %q, want none", bucket.Window, bucket.ResetTime)
		}
	}
}

func TestPackageResetGoesByTheSubscriptionTheSummaryNames(t *testing.T) {
	now := time.Now()
	cycleEnd := beijingWall(now.AddDate(0, 0, 9))
	sliceEnd := beijingWall(now.AddDate(0, 0, 1))
	packages := []packageRecord{
		{PackageCode: "free-code", CapacityType: json.RawMessage(`4`), CycleEndTime: sliceEnd},
		{PackageCode: "sub-code", CapacityType: json.RawMessage(`2`), CycleEndTime: cycleEnd},
	}
	if got, want := packageReset(resourceSummary{SubscriptionPackageCode: "sub-code"}, packages, now), beijingTime(cycleEnd); got != want {
		t.Fatalf("reset = %q, want %q", got, want)
	}
	// A summary that names a subscription its package list does not carry is
	// unknown, not a reason to read the free slice instead.
	if got := packageReset(resourceSummary{SubscriptionPackageCode: "gone"}, packages, now); got != "" {
		t.Fatalf("unlisted subscription reset = %q, want none", got)
	}
	if got, want := packageReset(resourceSummary{}, packages, now), beijingTime(sliceEnd); got != want {
		t.Fatalf("free slice reset = %q, want %q", got, want)
	}
	// A build that lists two slices refills the plan at the last of them; a card
	// counting down to the earlier one would say "recovered" while still short.
	late := beijingWall(now.AddDate(0, 0, 20))
	twoSlices := append(packages, packageRecord{PackageCode: "free-late", CapacityType: json.RawMessage(`4`), CycleEndTime: late})
	if got, want := packageReset(resourceSummary{}, twoSlices, now), beijingTime(late); got != want {
		t.Fatalf("two-slice reset = %q, want %q", got, want)
	}
}

// TestReadPackagesFollowsTheStatedCount pins the walk to the count WorkBuddy
// states, not to the length of one page: a short page from an upstream that
// serves fewer rows than asked for must not hide the packages behind it.
func TestReadPackagesFollowsTheStatedCount(t *testing.T) {
	now := time.Now()
	first := beijingWall(now.AddDate(0, 0, 3))
	second := beijingWall(now.AddDate(0, 0, 9))
	var pages []float64
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		var body map[string]any
		if json.Unmarshal(req.Body, &body) != nil {
			t.Fatalf("package body = %s", req.Body)
		}
		page, _ := body["PageNumber"].(float64)
		pages = append(pages, page)
		switch page {
		case 1:
			return jsonReply(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"Accounts":[
				{"PackageCode":"a","CapacityType":1,"CycleEndTime":"`+first+`"}
			]}}}}`), nil
		case 2:
			return jsonReply(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"Accounts":[
				{"PackageCode":"b","CapacityType":4,"CycleEndTime":"`+second+`"}
			]}}}}`), nil
		}
		t.Fatalf("asked for page %v", page)
		return pluginapi.HTTPResponse{}, nil
	}}
	c, err := readCredential(testCredential(siteWorkBuddy))
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := configuredSpec(c.Site)
	if !ok {
		t.Fatal("site is not configured")
	}
	records, err := readPackages(sender, spec, c, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].PackageCode != "a" || records[1].PackageCode != "b" {
		t.Fatalf("packages = %+v", records)
	}
	if len(pages) != 2 || pages[0] != 1 || pages[1] != 2 {
		t.Fatalf("pages = %v", pages)
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
		case strings.HasSuffix(req.URL, packagesPath):
			return jsonReply(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":0,"Accounts":[]}}}}`), nil
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
	for _, call := range sender.calls {
		if strings.HasSuffix(call.URL, packagesPath) {
			t.Fatalf("a team account asked the personal package meter: %s", call.URL)
		}
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
