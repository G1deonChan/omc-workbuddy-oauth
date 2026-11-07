package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// sseFrame is one complete server-sent event: the bytes as WorkBuddy sent them,
// and its data payload on its own.
type sseFrame struct {
	Raw  []byte
	Data []byte
}

// sseBuffer collects a byte stream and cuts it into complete events, so a chunk
// boundary that falls inside an event never reaches a parser or a client.
type sseBuffer struct{ buf []byte }

// push adds bytes and returns every event that is now complete. The trailing
// partial event stays buffered.
func (b *sseBuffer) push(chunk []byte) []sseFrame {
	b.buf = append(b.buf, chunk...)
	var out []sseFrame
	for {
		end, sep := 0, 0
		if i := bytes.Index(b.buf, []byte("\r\n\r\n")); i >= 0 {
			end, sep = i, 4
		}
		if i := bytes.Index(b.buf, []byte("\n\n")); i >= 0 && (sep == 0 || i < end) {
			end, sep = i, 2
		}
		if sep == 0 {
			return out
		}
		raw := b.buf[:end+sep]
		if f, ok := frameOf(raw); ok {
			out = append(out, f)
		}
		b.buf = b.buf[end+sep:]
	}
}

// flush returns whatever is left as one last event, for a stream that ended
// without its blank line.
func (b *sseBuffer) flush() []sseFrame {
	if len(bytes.TrimSpace(b.buf)) == 0 {
		b.buf = nil
		return nil
	}
	raw := b.buf
	b.buf = nil
	if f, ok := frameOf(raw); ok {
		return []sseFrame{f}
	}
	return nil
}

// frameOf reads one event's data lines. An event that carries none (a comment, a
// keep-alive) is not a frame.
func frameOf(raw []byte) (sseFrame, bool) {
	var data [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimPrefix(line, []byte("data:"))
		value = bytes.TrimPrefix(value, []byte(" "))
		data = append(data, value)
	}
	if len(data) == 0 {
		return sseFrame{}, false
	}
	return sseFrame{Raw: append([]byte(nil), raw...), Data: bytes.Join(data, []byte("\n"))}, true
}

// isDone reports the end marker of an event stream.
func isDone(data []byte) bool { return strings.TrimSpace(string(data)) == "[DONE]" }

// toolAcc is one tool call being assembled across chunks.
type toolAcc struct {
	ID        string
	Type      string
	Name      string
	Arguments strings.Builder
}

// completionBuilder assembles WorkBuddy's chat chunks into one OpenAI completion.
// WorkBuddy streams every chat, so a client that asked for one answer gets the
// stream read out for it.
type completionBuilder struct {
	id        string
	model     string
	created   int64
	role      string
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*toolAcc
	order     []int
	finish    string
	usage     json.RawMessage
	answered  bool
}

func newCompletionBuilder(model string) *completionBuilder {
	return &completionBuilder{model: model, tools: map[int]*toolAcc{}}
}

// streamChunk is one chat chunk, in the shape WorkBuddy sends.
type streamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// chunkError is a refusal carried inside an OK stream, or a plain error body.
type chunkError struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
	Code    json.RawMessage `json:"code"`
	Message *string         `json:"msg"`
}

// refusalOf reports the refusal a stream event carries, if any. A chunk with a
// choice is an answer, not a refusal.
func refusalOf(data []byte) error {
	var c chunkError
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	env := apiEnvelope{Code: c.Code, Msg: c.Message}
	if c.Error != nil && strings.TrimSpace(c.Error.Message) != "" {
		return &wbError{Status: 0, Code: env.code(), Message: strings.TrimSpace(c.Error.Message)}
	}
	if env.refused() {
		return &wbError{Status: 0, Code: env.code(), Message: env.message()}
	}
	return nil
}

// feed adds one event's payload. A refusal is returned as an error and ends the
// answer; the partial text stays for the caller to decide on.
func (b *completionBuilder) feed(data []byte) error {
	if isDone(data) {
		return nil
	}
	if err := refusalOf(data); err != nil {
		return err
	}
	var chunk streamChunk
	if json.Unmarshal(data, &chunk) != nil {
		return nil
	}
	if chunk.ID != "" {
		b.id = chunk.ID
	}
	if chunk.Model != "" {
		b.model = chunk.Model
	}
	if chunk.Created > 0 {
		b.created = chunk.Created
	}
	if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
		b.usage = append(json.RawMessage(nil), chunk.Usage...)
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		delta := choice.Delta
		if choice.Message.Role != "" || len(choice.Message.Content) > 0 || len(choice.Message.ToolCalls) > 0 {
			delta.Role = choice.Message.Role
			delta.Content = choice.Message.Content
			delta.ReasoningContent = choice.Message.ReasoningContent
			delta.Reasoning = choice.Message.Reasoning
			delta.ToolCalls = choice.Message.ToolCalls
		}
		if delta.Role != "" {
			b.role = delta.Role
		}
		if text := contentText(delta.Content); text != "" {
			b.content.WriteString(text)
			b.answered = true
		}
		if delta.ReasoningContent != "" {
			b.reasoning.WriteString(delta.ReasoningContent)
			b.answered = true
		} else if delta.Reasoning != "" {
			b.reasoning.WriteString(delta.Reasoning)
			b.answered = true
		}
		for _, call := range delta.ToolCalls {
			acc := b.tools[call.Index]
			if acc == nil {
				acc = &toolAcc{}
				b.tools[call.Index] = acc
				b.order = append(b.order, call.Index)
			}
			if call.ID != "" {
				acc.ID = call.ID
			}
			if call.Type != "" {
				acc.Type = call.Type
			}
			if call.Function.Name != "" {
				acc.Name = call.Function.Name
			}
			if call.Function.Arguments != "" {
				acc.Arguments.WriteString(call.Function.Arguments)
			}
			b.answered = true
		}
		if choice.FinishReason != "" {
			b.finish = choice.FinishReason
		}
	}
	return nil
}

// contentText reads a chunk's content: a string, or the text of a content array.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if text, ok := p["text"].(string); ok {
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

// saidSomething reports an answer worth returning to a client that asked for one
// answer: text, thinking or a tool call.
func (b *completionBuilder) saidSomething() bool { return b.answered }

// toolCalls is the assembled tool calls, in the order they first appeared.
func (b *completionBuilder) toolCalls() []map[string]any {
	out := make([]map[string]any, 0, len(b.order))
	for i, index := range b.order {
		acc := b.tools[index]
		if acc == nil {
			continue
		}
		id := acc.ID
		if id == "" {
			id = "call_" + randomID()[:12]
		}
		kind := acc.Type
		if kind == "" {
			kind = "function"
		}
		out = append(out, map[string]any{
			"id":    id,
			"type":  kind,
			"index": i,
			"function": map[string]any{
				"name":      acc.Name,
				"arguments": acc.Arguments.String(),
			},
		})
	}
	return out
}

// completion is the one answer the chunks add up to.
func (b *completionBuilder) completion() ([]byte, error) {
	if !b.answered {
		return nil, errors.New("WorkBuddy ended without an answer")
	}
	message := map[string]any{"role": "assistant"}
	role := b.role
	if role == "" {
		role = "assistant"
	}
	message["role"] = role
	if b.content.Len() > 0 {
		message["content"] = b.content.String()
	}
	if b.reasoning.Len() > 0 {
		message["reasoning_content"] = b.reasoning.String()
	}
	calls := b.toolCalls()
	if len(calls) > 0 {
		for _, call := range calls {
			delete(call, "index")
		}
		message["tool_calls"] = calls
		if message["content"] == nil {
			message["content"] = nil
		}
	}
	finish := b.finish
	if finish == "" {
		if len(calls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	out := map[string]any{
		"id":      firstNonEmpty(b.id, "chatcmpl-"+randomID()[:24]),
		"object":  "chat.completion",
		"created": b.created,
		"model":   b.model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if b.created == 0 {
		delete(out, "created")
	}
	if len(b.usage) > 0 {
		out["usage"] = b.usage
	}
	return json.Marshal(out)
}

// streamRefusal is a refusal carried inside an OK stream, kept with the frame it
// arrived in so the caller can read WorkBuddy's own code from it.
type streamRefusal struct {
	Data []byte
	Err  error
}

func (e *streamRefusal) Error() string { return e.Err.Error() }
func (e *streamRefusal) Unwrap() error { return e.Err }

// aggregateStream reads a whole chat stream and answers with one completion.
func aggregateStream(body []byte, model string) ([]byte, error) {
	var buf sseBuffer
	builder := newCompletionBuilder(model)
	for _, frame := range append(buf.push(body), buf.flush()...) {
		if err := builder.feed(frame.Data); err != nil {
			return nil, &streamRefusal{Data: frame.Data, Err: err}
		}
	}
	return builder.completion()
}
