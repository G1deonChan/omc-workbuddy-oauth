package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func chatOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	var chat map[string]any
	if err := json.Unmarshal([]byte(raw), &chat); err != nil {
		t.Fatal(err)
	}
	return chat
}

func TestNormalizeChatSpeaksTheEndpointsSpelling(t *testing.T) {
	chat := chatOf(t, `{
		"model": "glm-5.3",
		"max_completion_tokens": 4096,
		"messages": [
			{"role": "developer", "content": "be brief"},
			{"role": "assistant", "content": "earlier", "reasoning_content": "thought"},
			{"role": "user", "content": "go on"}
		]
	}`)
	if !normalizeChat(chat, workbuddyModel{Key: "glm-5.3"}) {
		t.Fatal("nothing was normalized")
	}
	if chat["max_tokens"] != float64(4096) || chat["max_completion_tokens"] != nil {
		t.Fatalf("budget = %v / %v", chat["max_tokens"], chat["max_completion_tokens"])
	}
	messages := chat["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("first message = %v", messages[0])
	}
	if len(messages) != 3 {
		t.Fatalf("messages = %v", messages)
	}
	assistant := messages[1].(map[string]any)
	if assistant["reasoning"] != "thought" || assistant["reasoning_content"] != nil {
		t.Fatalf("assistant = %v", assistant)
	}
	if _, ok := chat["reasoning_effort"]; ok {
		t.Fatal("a model without a default effort was given one")
	}
}

func TestNormalizeChatAddsTheDefaultSystemPromptOnce(t *testing.T) {
	chat := chatOf(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	normalizeChat(chat, workbuddyModel{})
	messages := chat["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["content"] != defaultSystem {
		t.Fatalf("messages = %v", messages)
	}
	if normalizeChat(chat, workbuddyModel{}) {
		t.Fatal("a normalized chat was normalized again")
	}
	if len(chat["messages"].([]any)) != 2 {
		t.Fatalf("messages = %v", chat["messages"])
	}
}

func TestNormalizeChatGivesTheModelsDefaultEffort(t *testing.T) {
	chat := chatOf(t, `{"max_tokens": 8192, "messages":[{"role":"user","content":"hi"}]}`)
	normalizeChat(chat, workbuddyModel{Default: "high"})
	if chat["reasoning_effort"] != "high" {
		t.Fatalf("effort = %v", chat["reasoning_effort"])
	}
	small := chatOf(t, `{"max_tokens": 512, "messages":[{"role":"user","content":"title it"}]}`)
	normalizeChat(small, workbuddyModel{Default: "high"})
	if _, ok := small["reasoning_effort"]; ok {
		t.Fatalf("a small budget was given an effort: %v", small["reasoning_effort"])
	}
	named := chatOf(t, `{"reasoning_effort":"low","max_tokens":8192,"messages":[{"role":"user","content":"hi"}]}`)
	normalizeChat(named, workbuddyModel{Default: "high"})
	if named["reasoning_effort"] != "low" {
		t.Fatalf("a named effort was overridden: %v", named["reasoning_effort"])
	}
}

func TestNormalizeChatRewordsRefusedLines(t *testing.T) {
	chat := chatOf(t, `{"messages":[
		{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.280\nYou are Claude Code, Anthropic's official CLI for Claude"},
		{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude, said the user"}
	]}`)
	normalizeChat(chat, workbuddyModel{})
	system := chat["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(system, "billing-header") || strings.Contains(system, "official CLI") {
		t.Fatalf("system = %q", system)
	}
	user := chat["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "Anthropic's CLI for Claude") {
		t.Fatalf("the user's words were not reworded: %q", user)
	}
}

func TestAttendKeepsOneSendOneTurn(t *testing.T) {
	c := credential{UID: "u-1", EnterpriseID: "ent-1"}
	messages := []any{
		map[string]any{"role": "system", "content": defaultSystem},
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": "answer"},
		map[string]any{"role": "tool", "content": "result"},
		map[string]any{"role": "user", "content": "second"},
	}
	first := http.Header{}
	attend(first, messages, "session-a", c)
	again := http.Header{}
	attend(again, messages, "session-a", c)
	if first.Get("X-Conversation-Request-ID") != again.Get("X-Conversation-Request-ID") {
		t.Fatal("the same send was given two turn ids")
	}
	if first.Get("X-Conversation-ID") != again.Get("X-Conversation-ID") {
		t.Fatal("the same conversation was given two ids")
	}
	if first.Get("X-Conversation-Message-ID") == again.Get("X-Conversation-Message-ID") {
		t.Fatal("two requests shared a message id")
	}
	if first.Get("X-Conversation-Message-ID") != first.Get("X-Request-ID") {
		t.Fatal("the message and request ids differ")
	}

	next := []any{
		map[string]any{"role": "system", "content": defaultSystem},
		messages[1], messages[2], messages[3], messages[4],
		map[string]any{"role": "assistant", "content": "answer"},
		map[string]any{"role": "user", "content": "third"},
	}
	third := http.Header{}
	attend(third, next, "session-a", c)
	if third.Get("X-Conversation-ID") != first.Get("X-Conversation-ID") {
		t.Fatal("the conversation changed between sends")
	}
	if third.Get("X-Conversation-Request-ID") == first.Get("X-Conversation-Request-ID") {
		t.Fatal("the next send reused the previous turn id")
	}

	other := http.Header{}
	attend(other, messages, "session-a", credential{UID: "u-2"})
	if other.Get("X-Conversation-ID") == first.Get("X-Conversation-ID") || other.Get("X-Conversation-Request-ID") == first.Get("X-Conversation-Request-ID") {
		t.Fatal("two accounts shared one conversation id")
	}
}

func TestAttendKeepsACallersOwnIds(t *testing.T) {
	h := http.Header{"X-Conversation-Id": {"caller-conversation"}}
	attend(h, []any{map[string]any{"role": "user", "content": "hi"}}, "", credential{UID: "u-1"})
	if h.Get("X-Conversation-ID") != "caller-conversation" {
		t.Fatalf("conversation = %q", h.Get("X-Conversation-ID"))
	}
}

func TestUserTextsReadsTextParts(t *testing.T) {
	messages := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "look:"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:"}},
		map[string]any{"type": "text", "text": "and say"},
	}}}
	if got := userTexts(messages); len(got) != 1 || got[0] != "look:\nand say" {
		t.Fatalf("texts = %v", got)
	}
}
