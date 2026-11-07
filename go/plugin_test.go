package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeSender struct {
	mu    sync.Mutex
	calls []pluginapi.HTTPRequest
	reply func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

func (f *fakeSender) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	return f.reply(req)
}

func (f *fakeSender) last() pluginapi.HTTPRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func jsonReply(status int, body string) pluginapi.HTTPResponse {
	return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}

func testCredential(site string) []byte {
	raw, _ := json.Marshal(credential{
		Site:    site,
		Access:  "at-test-token",
		Refresh: "rt-old",
		Expires: time.Now().Add(2 * time.Hour).UnixMilli(),
		UID:     "user-1",
		Name:    "Ada",
	})
	return raw
}

// resetPluginState clears the caches one test's fixture would otherwise leak into
// the next.
func resetPluginState() {
	listingCache.Lock()
	listingCache.entries = nil
	listingCache.Unlock()
	gateMu.Lock()
	gates = map[string]*credentialGate{}
	gateMu.Unlock()
	flowMu.Lock()
	pendingFlows = map[string]pendingFlow{}
	flowMu.Unlock()
	_ = configure(nil)
}

func TestStartLoginOpensTheBuildsSignInPage(t *testing.T) {
	resetPluginState()
	dir := t.TempDir()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, "/v2/plugin/auth/state?platform=workbuddy") {
			t.Fatalf("start URL = %s", req.URL)
		}
		return jsonReply(200, `{"code":0,"data":{"state":"vendor-state","authUrl":"https://copilot.tencent.com/login/index?from=desktop"}}`), nil
	}}
	start, err := startLogin(pluginapi.AuthLoginStartRequest{
		Provider: siteWorkBuddy,
		Host:     pluginapi.HostConfigSummary{AuthDir: dir},
	}, sender)
	if err != nil {
		t.Fatal(err)
	}
	page, err := url.Parse(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	if page.Host != "copilot.tencent.com" || page.Query().Get("version") != defaultAppVersion {
		t.Fatalf("page = %s", start.URL)
	}
	if len(page.Query().Get("loginSessionId")) != 32 {
		t.Fatalf("loginSessionId = %q", page.Query().Get("loginSessionId"))
	}
	if start.Provider != credentialProvider || start.State == "" || !start.ExpiresAt.After(time.Now()) {
		t.Fatalf("start = %+v", start)
	}
	key, err := loadStateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := openState(key, start.State)
	if err != nil {
		t.Fatal(err)
	}
	if state.Site != siteWorkBuddy {
		t.Fatalf("state site = %q", state.Site)
	}
	if vendor, ok := peekFlow(state.Nonce); !ok || vendor != "vendor-state" {
		t.Fatalf("pending flow = %q %v", vendor, ok)
	}
	call := sender.last()
	if call.Headers.Get("X-No-Authorization") != "true" || call.Headers.Get("User-Agent") != "WorkBuddy/"+defaultAppVersion {
		t.Fatalf("start headers = %v", call.Headers)
	}
}

func TestStartLoginRejectsAnUnknownEntry(t *testing.T) {
	resetPluginState()
	_, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: "qoder", Host: pluginapi.HostConfigSummary{AuthDir: t.TempDir()}}, &fakeSender{})
	if err == nil || !strings.Contains(err.Error(), "unknown login entry") {
		t.Fatalf("err = %v", err)
	}
}

func TestPollLoginReadsTheAccount(t *testing.T) {
	resetPluginState()
	dir := t.TempDir()
	start := startTestLogin(t, dir, siteWorkBuddyAI)
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(req.URL, "/v2/plugin/auth/token?state=vendor-state"):
			return jsonReply(200, `{"code":0,"data":{"accessToken":"at-1","refreshToken":"rt-1","expiresIn":3600,"refreshExpiresIn":86400,"domain":"www.workbuddy.ai","tokenType":"Bearer"}}`), nil
		case strings.Contains(req.URL, "/v2/plugin/login/account?state=vendor-state"):
			if req.Headers.Get("Authorization") != "Bearer at-1" || req.Headers.Get("X-Domain") != "www.workbuddy.ai" {
				t.Fatalf("account headers = %v", req.Headers)
			}
			return jsonReply(200, `{"code":0,"data":{"uid":"u-9","nickname":"Ada","phoneNumber":"13800138000","enterpriseId":"ent-7","enterpriseName":"Acme"}}`), nil
		}
		t.Fatalf("unexpected URL %s", req.URL)
		return pluginapi.HTTPResponse{}, nil
	}}
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess || poll.Auth.Provider != credentialProvider {
		t.Fatalf("poll = %+v", poll)
	}
	if poll.Auth.Label != "Ada · Acme" || poll.Auth.Metadata["oauth_provider"] != siteWorkBuddyAI {
		t.Fatalf("auth = %+v", poll.Auth)
	}
	var c credential
	if err := json.Unmarshal(poll.Auth.StorageJSON, &c); err != nil {
		t.Fatal(err)
	}
	if c.Site != siteWorkBuddyAI || c.UID != "u-9" || c.Access != "at-1" || c.Refresh != "rt-1" || c.Domain != "www.workbuddy.ai" {
		t.Fatalf("credential = %+v", c)
	}
	if c.Expires <= time.Now().UnixMilli() || c.RefreshExpires <= c.Expires {
		t.Fatalf("expiry = %+v", c)
	}
	if _, ok := peekFlow(mustNonce(t, dir, start.State)); ok {
		t.Fatal("a finished flow stayed pending")
	}
}

func TestPollLoginStaysPending(t *testing.T) {
	resetPluginState()
	dir := t.TempDir()
	start := startTestLogin(t, dir, siteWorkBuddy)
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonReply(200, `{"code":11217,"msg":"the sign-in is not finished"}`), nil
	}}
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusPending || len(poll.Auth.StorageJSON) != 0 {
		t.Fatalf("poll = %+v", poll)
	}
}

func TestPollLoginEndsAFlowItCannotResume(t *testing.T) {
	resetPluginState()
	dir := t.TempDir()
	start := startTestLogin(t, dir, siteWorkBuddy)
	flowMu.Lock()
	pendingFlows = map[string]pendingFlow{}
	flowMu.Unlock()
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, &fakeSender{})
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusError || !strings.Contains(poll.Message, "sign in again") {
		t.Fatalf("poll = %+v", poll)
	}
}

func TestRefreshRotatesTokens(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, "/v2/plugin/auth/token/refresh") {
			t.Fatalf("refresh URL = %s", req.URL)
		}
		if req.Headers.Get("X-Refresh-Token") != "rt-old" || req.Headers.Get("X-Auth-Refresh-Source") != "plugin" {
			t.Fatalf("refresh headers = %v", req.Headers)
		}
		return jsonReply(200, `{"code":0,"data":{"accessToken":"at-2","refreshToken":"rt-2","expiresIn":3600}}`), nil
	}}
	resp, err := refreshCredential(pluginapi.AuthRefreshRequest{AuthID: "workbuddy-x.json", StorageJSON: testCredential(siteWorkBuddy)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	var c credential
	if err := json.Unmarshal(resp.Auth.StorageJSON, &c); err != nil {
		t.Fatal(err)
	}
	if c.Access != "at-2" || c.Refresh != "rt-2" || c.Expires <= time.Now().UnixMilli() {
		t.Fatalf("credential = %+v", c)
	}
	if resp.NextRefreshAfter.IsZero() || resp.Auth.NextRefreshAfter.IsZero() {
		t.Fatal("no next refresh was scheduled")
	}
}

func TestRefreshSpendsOneTokenOnce(t *testing.T) {
	resetPluginState()
	release := make(chan struct{})
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		<-release
		return jsonReply(200, `{"code":0,"data":{"accessToken":"at-2","refreshToken":"rt-2"}}`), nil
	}}
	storage := testCredential(siteWorkBuddy)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := refreshCredential(pluginapi.AuthRefreshRequest{AuthID: "workbuddy-x.json", StorageJSON: storage}, sender); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if sender.count() != 1 {
		t.Fatalf("refresh tokens spent %d times", sender.count())
	}
}

func TestRefreshRefusedEndsTheSignIn(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonReply(401, `{"code":401,"msg":"invalid_grant"}`), nil
	}}
	_, err := refreshCredential(pluginapi.AuthRefreshRequest{AuthID: "a.json", StorageJSON: testCredential(siteWorkBuddy)}, sender)
	var expired *expiredError
	if !errors.As(err, &expired) {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestModelsComeFromTheProductConfig(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, "/v3/config") {
			t.Fatalf("config URL = %s", req.URL)
		}
		if !strings.HasPrefix(req.Headers.Get("User-Agent"), "WorkBuddy/") {
			t.Fatalf("config User-Agent = %q", req.Headers.Get("User-Agent"))
		}
		return jsonReply(200, `{"code":0,"data":{
			"agents":[{"name":"cli","models":["glm-5.3","claude-opus-5.5","plain-1"]},{"name":"other","models":["ignored"]}],
			"models":[
				{"id":"glm-5.3","name":"GLM-5.3","maxInputTokens":1000000,"maxOutputTokens":32000,"supportsImages":true,"supportsReasoning":true,"reasoning":{"supportedEfforts":["low","high"],"defaultEffort":"high","canDisableThinking":true}},
				{"id":"claude-opus-5.5","name":"Claude Opus 5.5","maxInputTokens":200000,"supportsImages":true},
				{"id":"plain-1","name":"Plain","maxInputTokens":64000}
			]}}`), nil
	}}
	out, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential(siteWorkBuddy)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "glm-5.3,plain-1" {
		t.Fatalf("models = %v", ids)
	}
	glm := out.Models[0]
	if glm.ContextLength != 1000000 || glm.OutputTokenLimit != 32000 || glm.Thinking == nil {
		t.Fatalf("glm = %+v", glm)
	}
	if strings.Join(glm.Thinking.Levels, ",") != "none,low,high" {
		t.Fatalf("levels = %v", glm.Thinking.Levels)
	}
	if len(glm.SupportedInputModalities) != 2 {
		t.Fatalf("glm modalities = %v", glm.SupportedInputModalities)
	}
	if len(out.Models[1].SupportedInputModalities) != 1 || out.Models[1].Thinking != nil {
		t.Fatalf("plain = %+v", out.Models[1])
	}
}

func TestModelsKeepGLMTextOnlyOnTheAI(t *testing.T) {
	resetPluginState()
	for _, m := range fallbackModels(mustSpec(t, siteWorkBuddyAI)) {
		if m.Key == "glm-5.3" && m.Images {
			t.Fatal("the international build's GLM models are advertised with images")
		}
	}
	if !textOnly(mustSpec(t, siteWorkBuddyAI), "glm-5.3") || textOnly(mustSpec(t, siteWorkBuddy), "glm-5.3") {
		t.Fatal("text-only is not per build")
	}
	if !messagesOnly(mustSpec(t, siteWorkBuddy), "claude-opus-5.5") || messagesOnly(mustSpec(t, siteWorkBuddyAI), "claude-opus-5.5") {
		t.Fatal("messages-only is not per build")
	}
}

func TestExecuteAggregatesTheStream(t *testing.T) {
	resetPluginState()
	var upstream pluginapi.HTTPRequest
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/v3/config"):
			return jsonReply(200, `{"code":0,"data":{"agents":[{"name":"cli","models":["glm-5.3"]}],"models":[{"id":"glm-5.3","name":"GLM-5.3","maxInputTokens":1000000,"maxReasoningEfforts":[]}]}}`), nil
		case strings.HasSuffix(req.URL, "/v2/chat/completions"):
			upstream = req
			return pluginapi.HTTPResponse{
				StatusCode: 200,
				Headers:    http.Header{"Content-Type": {"text/event-stream"}},
				Body: []byte("data: {\"id\":\"chat-1\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello \"}}]}\n\n" +
					"data: {\"id\":\"chat-1\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"},\"finish_reason\":\"stop\"}]}\n\n" +
					"data: {\"id\":\"chat-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2,\"total_tokens\":11}}\n\n" +
					"data: [DONE]\n\n"),
			}, nil
		}
		t.Fatalf("unexpected URL %s", req.URL)
		return pluginapi.HTTPResponse{}, nil
	}}
	body, _ := json.Marshal(map[string]any{
		"model":                 "glm-5.3",
		"max_completion_tokens": 512,
		"messages":              []map[string]any{{"role": "user", "content": "say hello"}},
	})
	resp, err := execute(pluginapi.ExecutorRequest{
		AuthID:      "workbuddy-x.json",
		Model:       "glm-5.3",
		Payload:     body,
		StorageJSON: testCredential(siteWorkBuddy),
		Headers:     http.Header{"X-Session-Id": {"sess-1"}},
	}, sender)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || out.Model != "glm-5.3" || len(out.Choices) != 1 {
		t.Fatalf("completion = %s", resp.Payload)
	}
	if out.Choices[0].Message.Content != "Hello world" || out.Choices[0].FinishReason != "stop" {
		t.Fatalf("message = %+v", out.Choices[0])
	}
	if out.Usage["total_tokens"] != float64(11) {
		t.Fatalf("usage = %v", out.Usage)
	}
	var sent map[string]any
	if err := json.Unmarshal(upstream.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["stream"] != true || sent["max_tokens"] != float64(512) || sent["max_completion_tokens"] != nil {
		t.Fatalf("upstream body = %v", sent)
	}
	options, _ := sent["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("stream options = %v", sent["stream_options"])
	}
	messages := sent["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", messages)
	}
	h := upstream.Headers
	if h.Get("Authorization") != "Bearer at-test-token" || h.Get("X-User-Id") != "user-1" || h.Get("X-Domain") != "copilot.tencent.com" {
		t.Fatalf("auth headers = %v", h)
	}
	if h.Get("X-Agent-Type") != "main" || h.Get("X-Agent-Runtime") != "local-agent" || h.Get("X-Stainless-Runtime") != "node" {
		t.Fatalf("agent headers = %v", h)
	}
	if len(h.Get("X-Conversation-ID")) != 32 || h.Get("X-Conversation-Message-ID") != h.Get("X-Request-ID") {
		t.Fatalf("conversation headers = %v", h)
	}
}

func TestExecuteMapsRefusals(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/v3/config"):
			return jsonReply(200, `{"code":0,"data":{"agents":[{"name":"cli","models":["gpt-5.5"]}],"models":[{"id":"gpt-5.5"}]}}`), nil
		default:
			return jsonReply(429, `{"code":14018,"msg":"usage exceeds frequency limit, your usage will reset at 2026-11-02 19:17:02 UTC+8"}`), nil
		}
	}}
	body, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	_, err := execute(pluginapi.ExecutorRequest{Model: "gpt-5.5", Payload: body, StorageJSON: testCredential(siteWorkBuddy)}, sender)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(perr.Message, "reset at") {
		t.Fatalf("message = %q", perr.Message)
	}
}

func TestExecuteRejectsUnknownModels(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonReply(200, `{"code":0,"data":{"agents":[{"name":"cli","models":["gpt-5.5"]}],"models":[{"id":"gpt-5.5"}]}}`), nil
	}}
	body, _ := json.Marshal(map[string]any{"model": "nope", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	_, err := execute(pluginapi.ExecutorRequest{Model: "nope", Payload: body, StorageJSON: testCredential(siteWorkBuddy)}, sender)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusBadRequest || perr.Code != "unknown_model" {
		t.Fatalf("err = %#v", err)
	}
}

func TestExecuteStreamForwardsFrames(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonReply(200, `{"code":0,"data":{"agents":[{"name":"cli","models":["glm-5.3"]}],"models":[{"id":"glm-5.3"}]}}`), nil
	}}
	chunks := [][]byte{
		[]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n"),
		[]byte("\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"),
	}
	next := 0
	var emitted [][]byte
	finished := make(chan struct{})
	pumpFinished = func(string) { close(finished) }
	openHTTPStream = func(plan requestPlan, callbackID string) (hostStreamResponse, error) {
		return hostStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, StreamID: "stream-1"}, nil
	}
	readStreamChunk = func(id string) (hostReadResponse, error) {
		if next >= len(chunks) {
			return hostReadResponse{Done: true}, nil
		}
		chunk := chunks[next]
		next++
		return hostReadResponse{Payload: chunk, Done: next == len(chunks)}, nil
	}
	emitStreamFrame = func(id string, payload []byte) error {
		emitted = append(emitted, append([]byte(nil), payload...))
		return nil
	}
	defer func() {
		openHTTPStream = openUpstreamStream
		readStreamChunk = readHostStream
		emitStreamFrame = emitStreamChunk
		pumpFinished = nil
	}()
	body, _ := json.Marshal(map[string]any{"model": "glm-5.3", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	out, err := executeStream(pluginapi.ExecutorRequest{Model: "glm-5.3", Payload: body, StorageJSON: testCredential(siteWorkBuddy)}, "cb", "stream-out", sender)
	if err != nil {
		t.Fatal(err)
	}
	if out["headers"] == nil {
		t.Fatal("no response headers")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream pump never returned")
	}
	joined := string(bytesJoin(emitted))
	if !strings.Contains(joined, "\"content\":\"Hi\"") || !strings.Contains(joined, "data: [DONE]") {
		t.Fatalf("frames = %q", joined)
	}
}

func TestExecuteStreamRejectsARefusalInsideAnOKStream(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonReply(200, `{"code":0,"data":{"agents":[{"name":"cli","models":["glm-5.3"]}],"models":[{"id":"glm-5.3"}]}}`), nil
	}}
	openHTTPStream = func(plan requestPlan, callbackID string) (hostStreamResponse, error) {
		return hostStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, StreamID: "stream-1"}, nil
	}
	readStreamChunk = func(id string) (hostReadResponse, error) {
		return hostReadResponse{Payload: []byte("data: {\"code\":11128,\"msg\":\"Illegal API invocation from an unapproved channel\"}\n\n"), Done: true}, nil
	}
	emitStreamFrame = func(id string, payload []byte) error { return nil }
	defer func() {
		openHTTPStream = openUpstreamStream
		readStreamChunk = readHostStream
		emitStreamFrame = emitStreamChunk
	}()
	body, _ := json.Marshal(map[string]any{"model": "glm-5.3", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	_, err := executeStream(pluginapi.ExecutorRequest{Model: "glm-5.3", Payload: body, StorageJSON: testCredential(siteWorkBuddy)}, "cb", "stream-out", sender)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusForbidden {
		t.Fatalf("err = %#v", err)
	}
}

func TestCountTokensIsNotInvented(t *testing.T) {
	_, err := countTokens()
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("err = %#v", err)
	}
}

func startTestLogin(t *testing.T, dir, site string) pluginapi.AuthLoginStartResponse {
	t.Helper()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.Contains(req.URL, "platform="+site) {
			t.Fatalf("start URL = %s", req.URL)
		}
		return jsonReply(200, `{"code":0,"data":{"state":"vendor-state","authUrl":"https://example.test/login"}}`), nil
	}}
	start, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: site, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil {
		t.Fatal(err)
	}
	return start
}

func mustNonce(t *testing.T, dir, state string) []byte {
	t.Helper()
	key, err := loadStateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := openState(key, state)
	if err != nil {
		t.Fatal(err)
	}
	return opened.Nonce
}

func mustSpec(t *testing.T, id string) siteSpec {
	t.Helper()
	spec, ok := siteFor(id)
	if !ok {
		t.Fatalf("no site %q", id)
	}
	return spec
}

func bytesJoin(parts [][]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
