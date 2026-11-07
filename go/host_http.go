package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type httpSender interface {
	Do(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

type cpaSender struct{ callbackID string }

// transportError keeps what the host said about a request that never got an
// answer. The cause is the difference between a caller that went away, a host
// timeout and a broken connection, and each of those deserves its own status.
type transportError struct{ cause string }

func (e *transportError) Error() string { return e.cause }

func (s cpaSender) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	var out pluginapi.HTTPResponse
	err := hostRPC("host.http.do", map[string]any{"host_callback_id": s.callbackID, "request": req}, &out)
	if err != nil {
		return out, &transportError{cause: err.Error()}
	}
	return out, nil
}

// transportFailure maps a request that got no answer onto the status a client
// should see. A cancellation is the caller's own, not the credential's: CPA reads
// 499 as "the client closed the request" and does not rest the account for it.
func transportFailure(err error) error {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "context canceled"),
		strings.Contains(text, "client closed"),
		strings.Contains(text, "client disconnected"),
		strings.Contains(text, "broken pipe"),
		strings.Contains(text, "connection reset"):
		return pluginabi.NewError("client_closed",
			"the caller closed the request before WorkBuddy answered; a thinking model can take longer than a caller's timeout, so stream it instead",
			499)
	case strings.Contains(text, "deadline exceeded"),
		strings.Contains(text, "timeout"),
		strings.Contains(text, "timed out"):
		return pluginabi.NewError("upstream_timeout", "WorkBuddy did not answer within the host's time limit", http.StatusGatewayTimeout)
	default:
		return pluginabi.NewError("transport_error", "WorkBuddy upstream transport failed", 503)
	}
}

// wbError is a refusal WorkBuddy answered with: its HTTP status, the code in its
// envelope when it sent one, and its message.
type wbError struct {
	Status  int
	Code    string
	Message string
}

func (e *wbError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("HTTP %d, code %s: %s", e.Status, e.Code, e.Message)
	case e.Code != "":
		return fmt.Sprintf("HTTP %d, code %s", e.Status, e.Code)
	case e.Message != "":
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	default:
		return fmt.Sprintf("HTTP %d", e.Status)
	}
}

// apiEnvelope is WorkBuddy's answer shape: a code, a message and a data object.
// A code other than 0 is a refusal even when the status says 200.
type apiEnvelope struct {
	Code    json.RawMessage `json:"code"`
	Msg     *string         `json:"msg"`
	Message *string         `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e apiEnvelope) message() string {
	for _, p := range []*string{e.Msg, e.Message} {
		if p == nil {
			continue
		}
		if s := strings.TrimSpace(*p); s != "" {
			return s
		}
	}
	return ""
}

func (e apiEnvelope) code() string {
	if len(e.Code) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(e.Code, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(e.Code, &n) == nil {
		return n.String()
	}
	return ""
}

// refused reports a code WorkBuddy sends for a failure: anything but 0.
func (e apiEnvelope) refused() bool {
	c := e.code()
	return c != "" && c != "0"
}

// parseEnvelope reads a WorkBuddy answer, keeping the body when it is not the
// envelope at all.
func parseEnvelope(body []byte) (apiEnvelope, json.RawMessage) {
	var env apiEnvelope
	if json.Unmarshal(body, &env) == nil && (len(env.Code) > 0 || env.Msg != nil || env.Message != nil || len(env.Data) > 0) {
		return env, env.Data
	}
	return env, nil
}

// wbCall makes one request of WorkBuddy's plugin and billing API and returns its
// data object. A transport failure, a non-2xx status or a non-zero code is an
// error; only a 2xx with code 0 is a result.
func wbCall(sender httpSender, method, target string, headers http.Header, body any) (json.RawMessage, error) {
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
	for k, v := range headers {
		h[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
	}
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	resp, err := sender.Do(pluginapi.HTTPRequest{Method: method, URL: target, Headers: h, Body: payload})
	if err != nil {
		return nil, errors.New("WorkBuddy request failed")
	}
	env, data := parseEnvelope(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || env.refused() {
		msg := env.message()
		if msg == "" {
			msg = strings.TrimSpace(string(resp.Body))
			if len(msg) > 200 {
				msg = msg[:200]
			}
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return nil, &wbError{Status: resp.StatusCode, Code: env.code(), Message: msg}
	}
	if len(data) == 0 || string(data) == "null" {
		return resp.Body, nil
	}
	return data, nil
}

// statusOf is the upstream HTTP status of an error, 0 when there was no response.
func statusOf(err error) int {
	var e *wbError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// pluginabiError is a host-facing error with an HTTP status.
func pluginabiError(code, message string, status int) error {
	return pluginabi.NewError(code, message, status)
}

// clientName is the User-Agent WorkBuddy's desktop CLI sends: the platform first
// at the app's version, then the CLI's own product name at the same version, then
// its extension. A request whose User-Agent does not start "WorkBuddy/<app>" is
// refused (10085), so this string is what makes a request WorkBuddy's own.
func clientName(spec siteSpec) string {
	return "WorkBuddy/" + spec.App + " " + spec.AppName + "/" + spec.App + " CLI/" + spec.CLI
}

// domainOf is the X-Domain a token was issued for: the domain WorkBuddy named at
// sign-in, else the build's own host.
func domainOf(spec siteSpec, c credential) string {
	if d := strings.TrimSpace(c.Domain); d != "" {
		return d
	}
	return defaultDomain(spec)
}

// defaultDomain is the host a token issued for this build belongs to.
func defaultDomain(spec siteSpec) string {
	return strings.TrimSuffix(strings.TrimPrefix(spec.Endpoint, "https://"), "/")
}

// signHeaders sets what WorkBuddy's desktop sends with every request it makes for
// an account: the token, the account's own id and domain, the product it is, and
// the client attribution its usage list reads. A personal account sends no
// enterprise headers; an enterprise account sends both as its enterprise id.
func signHeaders(spec siteSpec, c credential, h http.Header) {
	h.Set("Authorization", "Bearer "+c.Access)
	h.Set("X-User-Id", c.UID)
	h.Set("X-Domain", domainOf(spec, c))
	h.Set("X-Product", "SaaS")
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("User-Agent", clientName(spec))
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Purpose", "conversation")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Version", spec.App)
	if id := strings.TrimSpace(c.EnterpriseID); id != "" {
		h.Set("X-Enterprise-Id", id)
		h.Set("X-Tenant-Id", id)
	}
}

// openAISDK and electronNode are what WorkBuddy's CLI sends its chats through:
// the OpenAI SDK it bundles and the Node its Electron runs it on. The CLI's
// client has a 1200-second timeout and no retries of its own.
const (
	openAISDK    = "6.25.0"
	electronNode = "v22.21.1"
)

// sdkPlatform maps Go's platform names to the ones the OpenAI SDK reports.
func sdkPlatform() (os, arch string) {
	os = map[string]string{
		"darwin": "MacOS", "windows": "Windows", "linux": "Linux", "freebsd": "FreeBSD",
		"openbsd": "OpenBSD", "android": "Android",
	}[runtime.GOOS]
	if os == "" {
		os = "Other:" + runtime.GOOS
	}
	arch = map[string]string{
		"amd64": "x64", "arm64": "arm64", "arm": "arm", "386": "x32",
	}[runtime.GOARCH]
	if arch == "" {
		arch = "other:" + runtime.GOARCH
	}
	return os, arch
}

// chatHeaders adds what the CLI puts on a model request alone: the agent type,
// the runtime a build names, and the OpenAI SDK's own headers.
func chatHeaders(spec siteSpec, h http.Header) {
	h.Set("X-Agent-Type", "main")
	h.Set("X-Agent-Runtime", "local-agent")
	h.Set("Accept", "application/json")
	h.Set("X-Stainless-Retry-Count", "0")
	h.Set("X-Stainless-Timeout", "1200")
	h.Set("X-Stainless-Lang", "js")
	h.Set("X-Stainless-Package-Version", openAISDK)
	os, arch := sdkPlatform()
	h.Set("X-Stainless-OS", os)
	h.Set("X-Stainless-Arch", arch)
	h.Set("X-Stainless-Runtime", "node")
	h.Set("X-Stainless-Runtime-Version", electronNode)
}
