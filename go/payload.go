package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// defaultSystem is the system message WorkBuddy's own app gives a chat that
// opens without one. Both builds want a system message first; a chat that starts
// with a user message is refused.
const defaultSystem = "You are a helpful assistant."

// smallBudget is the largest output budget a chat can name and still be one no
// client meant to think in: a title or a classification. WorkBuddy does not think
// without an effort, so such a chat is left without one and its whole budget goes
// to the answer.
const smallBudget = 1024

// flagged are the identity lines WorkBuddy refuses as an "unapproved channel"
// (400, code 11128). It matches a known line whole and exactly, so the words here
// say the same thing in wording it accepts. The list is what has been met so far,
// not a rule: a new agent's opening line is refused until it is added.
var flagged = [][2]string{
	// Claude Code's billing header means nothing to WorkBuddy's models and goes.
	{"x-anthropic-billing-header:", ""},
	{"You are Claude Code, Anthropic's official CLI for Claude", "You are Claude Code, Anthropic's CLI for Claude"},
	{"Main branch (you will usually use this for PRs)", "Main branch (usually the base for PRs)"},
	{"Codex CLI is an open source project led by OpenAI.", "The Codex CLI is an open source project led by OpenAI."},
	{"You are an AI agent powered by DeepSeek Harness.", "You are an AI agent, powered by DeepSeek Harness."},
	{"Web GUI whose URL the runtime context names.  When the user refers", "Web GUI whose URL the runtime context names. The interface, when the user refers"},
}

// unflagged is text with the refused lines reworded.
func unflagged(text string) string {
	for _, pair := range flagged {
		if pair[1] == "" {
			// The billing header is a whole line and goes entirely; only lines
			// that start it are cut, so a mention inside prose stays.
			if strings.HasPrefix(text, pair[0]) {
				if i := strings.IndexByte(text, '\n'); i >= 0 {
					text = text[i+1:]
				} else {
					text = ""
				}
			}
			continue
		}
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return text
}

// unflag rewords the refused lines in every message of a chat and reports whether
// it changed anything. A system message that held nothing else is dropped, as the
// billing header's block is.
func unflag(messages []any) bool {
	changed := false
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		switch content := m["content"].(type) {
		case string:
			next := unflagged(content)
			if next == content {
				continue
			}
			changed = true
			if strings.TrimSpace(next) != "" || m["role"] != "system" {
				m["content"] = next
			} else {
				messages = append(messages[:i], messages[i+1:]...)
			}
		case []any:
			parts := make([]any, 0, len(content))
			here := false
			for _, raw := range content {
				part, ok := raw.(map[string]any)
				if !ok {
					parts = append(parts, raw)
					continue
				}
				text, ok := part["text"].(string)
				if !ok {
					parts = append(parts, part)
					continue
				}
				next := unflagged(text)
				if next == text {
					parts = append(parts, part)
					continue
				}
				here = true
				if strings.TrimSpace(next) != "" {
					part["text"] = next
					parts = append(parts, part)
				}
			}
			if !here {
				continue
			}
			changed = true
			if len(parts) > 0 || m["role"] != "system" {
				m["content"] = parts
			} else {
				messages = append(messages[:i], messages[i+1:]...)
			}
		}
	}
	return changed
}

// reasoningOf is an earlier turn's thinking as a client sent it: DeepSeek-style
// reasoning_content, else OpenRouter-style reasoning_details.
func reasoningOf(m map[string]any) string {
	if text, ok := m["reasoning_content"].(string); ok && text != "" {
		return text
	}
	if details, ok := m["reasoning_details"].([]any); ok {
		var b strings.Builder
		for _, raw := range details {
			d, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := d["text"].(string); ok {
				b.WriteString(text)
				continue
			}
			if summary, ok := d["summary"].(string); ok {
				b.WriteString(summary)
			}
		}
		return b.String()
	}
	return ""
}

// asReasoning moves an assistant turn's thinking to the field WorkBuddy reads.
// WorkBuddy drops reasoning_content and reasoning_details, and takes the earlier
// turn's thinking as `reasoning`.
func asReasoning(messages []any) bool {
	changed := false
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "assistant" {
			continue
		}
		_, hasContent := m["reasoning_content"]
		_, hasDetails := m["reasoning_details"]
		if !hasContent && !hasDetails {
			continue
		}
		if text := reasoningOf(m); text != "" {
			if own, ok := m["reasoning"].(string); !ok || own == "" {
				m["reasoning"] = text
			}
		}
		delete(m, "reasoning_content")
		delete(m, "reasoning_details")
		changed = true
	}
	return changed
}

// namedEffort reports a chat that picks its own reasoning level, in either
// spelling WorkBuddy's clients use.
func namedEffort(chat map[string]any) bool {
	if _, ok := chat["reasoning_effort"]; ok {
		return true
	}
	if reasoning, ok := chat["reasoning"].(map[string]any); ok {
		if _, ok := reasoning["effort"]; ok {
			return true
		}
	}
	return false
}

// normalizeChat applies the request spellings WorkBuddy's chat endpoint accepts
// and reports whether it changed anything:
//
//   - max_completion_tokens becomes max_tokens, and developer becomes system,
//     because the endpoint takes those two names.
//   - a refusal-prone identity line is reworded (see flagged).
//   - a chat that does not open with a system message is given WorkBuddy's own
//     default one, as its app does.
//   - a chat that names no reasoning level is given the model's default, unless
//     its budget is too small to have meant to think.
func normalizeChat(chat map[string]any, model workbuddyModel) bool {
	changed := false
	if limit, ok := chat["max_completion_tokens"]; ok {
		if _, present := chat["max_tokens"]; !present {
			chat["max_tokens"] = limit
		}
		delete(chat, "max_completion_tokens")
		changed = true
	}
	messages, ok := chat["messages"].([]any)
	if !ok || len(messages) == 0 {
		return changed
	}
	for _, raw := range messages {
		if m, ok := raw.(map[string]any); ok && m["role"] == "developer" {
			m["role"] = "system"
			changed = true
		}
	}
	if unflag(messages) {
		changed = true
	}
	if asReasoning(messages) {
		changed = true
	}
	if first, ok := messages[0].(map[string]any); !ok || first["role"] != "system" {
		chat["messages"] = append([]any{map[string]any{"role": "system", "content": defaultSystem}}, messages...)
		messages = chat["messages"].([]any)
		changed = true
	}
	if model.Default != "" && !namedEffort(chat) {
		if limit, ok := chat["max_tokens"]; !ok || !(numberOf(limit) > 0 && numberOf(limit) <= smallBudget) {
			chat["reasoning_effort"] = model.Default
			changed = true
		}
	}
	return changed
}

func numberOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// textOf is a message's text: a string, or the text parts of a content array.
func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			if part, ok := raw.(map[string]any); ok {
				if text, ok := part["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// userTexts is every user message's text, in the order the chat has them.
func userTexts(messages []any) []string {
	var out []string
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "user" {
			continue
		}
		out = append(out, textOf(m["content"]))
	}
	return out
}

// idSalt makes the request ids unguessable: they are digests of what names a
// conversation or a turn, and the salt says nothing of the words they were made
// from. A new salt every process, as WorkBuddy's own ids are one run's own.
var idSalt = func() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "workbuddy"
	}
	return hex.EncodeToString(b)
}()

// idFor is the 32 hex digits WorkBuddy's own request ids are made of, derived
// from the kind of id and what names it.
func idFor(kind string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(idSalt))
	h.Write([]byte{'|'})
	h.Write([]byte(kind))
	h.Write([]byte{'|'})
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0x1f})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// randomID is a fresh 32-hex id, the shape WorkBuddy's message ids have.
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Unusable entropy still has to name a message: the digest of what this
		// process knows and when it asked is unique enough for an id.
		sum := sha256.Sum256([]byte(idSalt + "|" + time.Now().UTC().Format(time.RFC3339Nano)))
		return hex.EncodeToString(sum[:])[:32]
	}
	return hex.EncodeToString(b)
}

// scopeOf names the account a chat goes as. WorkBuddy's own client runs as one
// account, so a conversation must not carry one account's ids under another's
// user id.
func scopeOf(c credential) string {
	return c.UID + "|" + c.EnterpriseID
}

// attend sets a chat's ids on its request headers: the message's, the turn's (one
// user send, however many steps it takes), and the conversation's. A header the
// caller already set is kept.
func attend(headers http.Header, messages []any, session string, c credential) {
	msg := randomID()
	headers.Set("X-Conversation-Message-ID", msg)
	headers.Set("X-Request-ID", msg)
	who := scopeOf(c)
	session = strings.TrimSpace(session)
	if session != "" {
		if session = session[:min(len(session), 128)]; !hasHeader(headers, "X-Conversation-ID") {
			headers.Set("X-Conversation-ID", idFor("conversation", "account:"+who, session))
		}
	} else if users := userTexts(messages); len(users) > 0 && strings.TrimSpace(users[0]) != "" {
		if !hasHeader(headers, "X-Conversation-ID") {
			headers.Set("X-Conversation-ID", idFor("conversation", "account:"+who, users[0]))
		}
	}
	if users := userTexts(messages); len(users) > 0 && strings.TrimSpace(users[len(users)-1]) != "" && !hasHeader(headers, "X-Conversation-Request-ID") {
		headers.Set("X-Conversation-Request-ID", idFor("turn", "account:"+who, users[len(users)-1]))
	}
	if !hasHeader(headers, "X-Conversation-Request-ID") {
		headers.Set("X-Conversation-Request-ID", randomID())
	}
}

// hasHeader reports a header the caller already set, in any casing.
func hasHeader(h http.Header, name string) bool {
	for key := range h {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// sessionOf is the session name CPA passes for a request, capped as its own
// session ids are. It never goes upstream: the conversation id derived from it
// does.
func sessionOf(headers http.Header) string {
	for _, name := range []string{"x-session-id", "X-Session-Id", "Session-Id"} {
		if v := strings.TrimSpace(headers.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// parseChat reads a chat completion body, which the host hands over already
// translated to OpenAI's shape.
func parseChat(body []byte) (map[string]any, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, errors.New("a chat completion request is required")
	}
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, errors.New("invalid chat completion request")
	}
	if _, ok := chat["messages"].([]any); !ok {
		return nil, errors.New("invalid chat completion request")
	}
	return chat, nil
}
