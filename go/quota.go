package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// capacity reads a billing number, which WorkBuddy sends as a string ("438.88")
// and now and then as a number. ok is false for an absent or empty field, which
// is not the same as a zero.
func capacity(raw json.RawMessage) (value float64, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return n, !math.IsNaN(n)
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0, false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if json.Unmarshal([]byte(s), &n) != nil || math.IsNaN(n) {
		return 0, false
	}
	return n, true
}

// packageCredit is one package of the resource summary.
type packageCredit struct {
	PackageCode         string          `json:"PackageCode"`
	CapacityType        json.RawMessage `json:"CapacityType"`
	Status              json.RawMessage `json:"Status"`
	CycleTotalCapacity  json.RawMessage `json:"CycleTotalCapacity"`
	CycleUsedCapacity   json.RawMessage `json:"CycleUsedCapacity"`
	CycleRemainCapacity json.RawMessage `json:"CycleRemainCapacity"`
	CycleStartTime      string          `json:"CycleStartTime"`
	CycleEndTime        string          `json:"CycleEndTime"`
	DeductionEndTime    json.RawMessage `json:"DeductionEndTime"`
}

// creditGroup is the summary's own breakdown of a package kind.
type creditGroup struct {
	Kind               string          `json:"Kind"`
	CycleTotalCapacity json.RawMessage `json:"CycleTotalCapacity"`
	CycleUsedCapacity  json.RawMessage `json:"CycleUsedCapacity"`
}

// resourceSummary is the account's credit summary: what WorkBuddy's own usage
// panel is built from.
type resourceSummary struct {
	IsPaidUser              bool            `json:"IsPaidUser"`
	SubscriptionPackageCode string          `json:"SubscriptionPackageCode"`
	CreditGroups            []creditGroup   `json:"CreditGroups"`
	Packages                []packageCredit `json:"Packages"`
}

// enterpriseUsage is the meter a team account's usage page reads, whose resource
// summary answers with no packages at all.
type enterpriseUsage struct {
	LimitNum       json.RawMessage `json:"limitNum"`
	Credit         json.RawMessage `json:"credit"`
	CycleStartTime string          `json:"cycleStartTime"`
	CycleResetTime string          `json:"cycleResetTime"`
}

// quota describes the provider: one product, no reset from a panel.
func quotaDescription() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{credentialProvider}, DisplayName: "WorkBuddy", SupportsReset: false}
}

func quotaReset() pluginapi.QuotaResetResponse {
	return pluginapi.QuotaResetResponse{Success: false, Message: "WorkBuddy credits reset on WorkBuddy's own usage page"}
}

// meters is the request shape both billing paths share.
func meter(sender httpSender, spec siteSpec, c credential, path string) (json.RawMessage, error) {
	headers := http.Header{}
	signHeaders(spec, c, headers)
	return wbCall(sender, "POST", spec.Endpoint+path, headers, map[string]any{})
}

// quotaFetch reads the account's credits and normalizes them.
func quotaFetch(req pluginapi.QuotaFetchRequest, sender httpSender) (pluginapi.QuotaFetchResponse, error) {
	out := pluginapi.QuotaFetchResponse{}
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return out, pluginabiError("invalid_auth", "invalid WorkBuddy credential", 401)
	}
	spec, ok := configuredSpec(c.Site)
	if !ok {
		return out, pluginabiError("invalid_auth", "invalid WorkBuddy credential", 401)
	}
	// The quota read never renews the credential. WorkBuddy rotates the refresh
	// token on every use, and this call has no way to save the new one back, so a
	// renewal here would spend a token the stored credential can no longer use.
	// The host renews on its own schedule and persists what it gets.
	raw, err := meter(sender, spec, c, summaryPath)
	if err != nil {
		if statusOf(err) == http.StatusUnauthorized || statusOf(err) == http.StatusForbidden {
			return out, pluginabiError("invalid_auth", spec.Name+" sign-in has expired; sign in again", 401)
		}
		return out, pluginabiError("usage_error", fmt.Sprintf("%s usage failed: %s", spec.Name, readableFailure(err)), http.StatusBadGateway)
	}
	summary, err := readSummary(raw)
	if err != nil {
		return out, pluginabiError("usage_error", err.Error(), http.StatusBadGateway)
	}
	windows := summaryWindows(summary)
	if strings.TrimSpace(c.EnterpriseID) != "" {
		if credit, err := readEnterpriseUsage(sender, spec, c); err == nil {
			if w, ok := enterpriseWindows(credit); ok {
				windows = []quotaWindow{w}
			}
		}
	}
	out.Subscription = &pluginapi.QuotaSubscription{Plan: planName(c, summary), TierName: "WorkBuddy"}
	group := pluginapi.QuotaGroup{DisplayName: "WorkBuddy"}
	if len(windows) == 0 {
		// An account with no credits at all is out of credits, not unknown: its
		// plan's pool reads 0 of 0, the way WorkBuddy's own panel shows it.
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: "Credits", Description: "0 credits left"})
		out.Summary = append(out.Summary, quotaMetric("credits", "Credits", "credits", 0))
		out.Groups = append(out.Groups, group)
		return out, nil
	}
	for _, w := range windows {
		bucket := pluginapi.QuotaBucket{
			Window:            w.Name,
			RemainingFraction: clamp01(w.Left / w.Total),
			Description:       fmt.Sprintf("%s / %s credits left", formatAmount(w.Left), formatAmount(w.Total)),
		}
		if w.Reset != "" {
			bucket.ResetTime = w.Reset
		}
		group.Buckets = append(group.Buckets, bucket)
		out.Summary = append(out.Summary, quotaMetric(w.Key, w.Name, "credits", w.Left))
	}
	out.Groups = append(out.Groups, group)
	return out, nil
}

func quotaMetric(key, label, unit string, value float64) pluginapi.QuotaMetric {
	return pluginapi.QuotaMetric{Key: key, Label: label, Value: value, Unit: unit, Format: "number"}
}

func readSummary(raw json.RawMessage) (resourceSummary, error) {
	var summary resourceSummary
	if len(raw) == 0 {
		return summary, errors.New("WorkBuddy returned no usage data")
	}
	if json.Unmarshal(raw, &summary) != nil {
		return summary, errors.New("WorkBuddy returned an invalid usage response")
	}
	return summary, nil
}

// quotaWindow is one credit pool as the card shows it.
type quotaWindow struct {
	Key   string
	Name  string
	Total float64
	Left  float64
	Reset string
}

// summaryWindows splits the summary's credits the way WorkBuddy's own panel does:
// the plan's base credits, every other kind beside them, and — when both exist —
// the account's whole, which is what stops it. A summary that names no groups is
// split by package code, keeping the plan's own packages apart from the rest.
func summaryWindows(summary resourceSummary) []quotaWindow {
	planTotal, planUsed := 0.0, 0.0
	extraTotal, extraUsed := 0.0, 0.0
	prepaid := 0.0
	groups := 0
	for _, g := range summary.CreditGroups {
		total, ok := capacity(g.CycleTotalCapacity)
		if !ok {
			continue
		}
		used, _ := capacity(g.CycleUsedCapacity)
		groups++
		if g.Kind == "plan_base" {
			planTotal += total
			planUsed += used
		} else {
			extraTotal += total
			extraUsed += used
		}
	}
	subscription := strings.TrimSpace(summary.SubscriptionPackageCode)
	for _, p := range summary.Packages {
		total, hasTotal := capacity(p.CycleTotalCapacity)
		if !hasTotal {
			if left, ok := capacity(p.CycleRemainCapacity); ok {
				prepaid += left
			}
			continue
		}
		if groups > 0 {
			continue
		}
		if subscription != "" {
			if p.PackageCode == subscription {
				planTotal += total
				used, _ := capacity(p.CycleUsedCapacity)
				planUsed += used
				continue
			}
		} else if strings.TrimSpace(asString(p.CapacityType)) == "4" {
			// A free account's summary names no subscription: the plan's own
			// credits are the daily slice package.
			planTotal += total
			used, _ := capacity(p.CycleUsedCapacity)
			planUsed += used
			continue
		}
		extraTotal += total
		used, _ := capacity(p.CycleUsedCapacity)
		extraUsed += used
	}
	reset := summaryReset(summary)
	var out []quotaWindow
	switch {
	case planTotal > 0 && extraTotal > 0:
		plan := quotaWindow{Key: "credits", Name: "Credits", Total: planTotal, Left: math.Max(0, planTotal-planUsed), Reset: reset}
		extra := quotaWindow{Key: "extra_credits", Name: "Extra credits", Total: extraTotal, Left: math.Max(0, extraTotal-extraUsed)}
		total := quotaWindow{
			Key:   "total_credits",
			Name:  "Total credits",
			Total: planTotal + extraTotal,
			Left:  math.Max(0, planTotal-planUsed) + math.Max(0, extraTotal-extraUsed),
			Reset: reset,
		}
		out = append(out, plan, extra, total)
	case planTotal+extraTotal > 0:
		left := math.Max(0, planTotal-planUsed) + math.Max(0, extraTotal-extraUsed)
		out = append(out, quotaWindow{Key: "credits", Name: "Credits", Total: planTotal + extraTotal, Left: left, Reset: reset})
	}
	if prepaid > 0 {
		// A prepaid balance has no denominator: it is what is left, so the window
		// is full and its description carries the amount.
		out = append(out, quotaWindow{Key: "prepaid_credits", Name: "Prepaid credits", Total: 1, Left: 1, Reset: ""})
	}
	return out
}

// planName is the plan a card shows. WorkBuddy's summary says whether the account
// is paid, not which plan it bought, so a paid account is named as one rather than
// given a plan name the vendor never said.
func planName(c credential, summary resourceSummary) string {
	if strings.TrimSpace(c.EnterpriseID) != "" {
		return "Enterprise"
	}
	if summary.IsPaidUser {
		return "Paid plan"
	}
	return "Free plan"
}

// summaryReset is when the plan's credits come back: the subscription package's
// cycle end, else the daily slice's. WorkBuddy states both in UTC+8.
func summaryReset(summary resourceSummary) string {
	subscription := strings.TrimSpace(summary.SubscriptionPackageCode)
	best := ""
	for _, p := range summary.Packages {
		at := beijingTime(p.CycleEndTime)
		if at == "" {
			continue
		}
		if subscription != "" {
			if p.PackageCode == subscription {
				return at
			}
			continue
		}
		if strings.TrimSpace(asString(p.CapacityType)) == "4" {
			return at
		}
		if best == "" || at < best {
			best = at
		}
	}
	return best
}

// beijingTime reads WorkBuddy's own timestamps ("2026-10-13 00:39:27", UTC+8) as
// RFC 3339 in UTC.
func beijingTime(value string) string {
	s := strings.TrimSpace(value)
	if s == "" {
		return ""
	}
	at, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.FixedZone("UTC+8", 8*3600))
	if err != nil {
		if parsed, err := time.Parse(time.RFC3339, s); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// readEnterpriseUsage reads the meter a team account's usage page uses.
func readEnterpriseUsage(sender httpSender, spec siteSpec, c credential) (enterpriseUsage, error) {
	var usage enterpriseUsage
	raw, err := meter(sender, spec, c, enterpriseUsagePath)
	if err != nil {
		return usage, err
	}
	if json.Unmarshal(raw, &usage) != nil {
		return usage, errors.New("WorkBuddy returned an invalid enterprise usage response")
	}
	return usage, nil
}

// enterpriseWindows is a team account's credits as one window, with the cycle.
func enterpriseWindows(usage enterpriseUsage) (quotaWindow, bool) {
	total, ok := capacity(usage.LimitNum)
	if !ok || total <= 0 {
		return quotaWindow{}, false
	}
	used, _ := capacity(usage.Credit)
	if used < 0 {
		used = 0
	}
	return quotaWindow{Key: "credits", Name: "Credits", Total: total, Left: math.Max(0, total-used), Reset: beijingTime(usage.CycleResetTime)}, true
}

// asString reads a JSON scalar as text, for the numeric fields WorkBuddy sends as
// strings and as numbers.
func asString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// formatAmount writes a credit count as WorkBuddy's own panel does: whole units
// with thousands separators, two decimals for a part unit.
func formatAmount(v float64) string {
	var text string
	if v == math.Trunc(v) {
		text = fmt.Sprintf("%.0f", v)
	} else {
		text = fmt.Sprintf("%.2f", v)
	}
	whole, fraction, hasFraction := strings.Cut(text, ".")
	var b strings.Builder
	for i := 0; i < len(whole); i++ {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(whole[i])
	}
	if hasFraction {
		b.WriteByte('.')
		b.WriteString(fraction)
	}
	return b.String()
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}
