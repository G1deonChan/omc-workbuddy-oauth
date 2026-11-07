package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// workbuddyModelSeed is a catalogue entry the plugin ships for a build whose own
// listing cannot be read. It mirrors the fields the request builder needs.
type workbuddyModelSeed struct {
	Key     string
	Name    string
	Context int64
	Efforts []string
}

// cnFallbackCatalogue is the China build's list at the time of writing: the tiers
// and vendor models its desktop offers. The account's own list takes over as soon
// as it is read.
var cnFallbackCatalogue = []workbuddyModelSeed{
	{Key: "auto", Name: "Auto", Context: 168_000},
	{Key: "hy4-preview-f", Name: "Hy4 preview", Context: 1_000_000, Efforts: []string{"high"}},
	{Key: "hy3", Name: "Hy3", Context: 192_000, Efforts: []string{"low", "high"}},
	{Key: "hy3-x", Name: "Hy3-X", Context: 192_000, Efforts: []string{"low", "high"}},
	{Key: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", Context: 1_000_000, Efforts: []string{"none", "low", "high", "max"}},
	{Key: "glm-5.3", Name: "GLM-5.3", Context: 1_000_000, Efforts: []string{"low", "high", "max"}},
	{Key: "glm-5.3-flash", Name: "GLM-5.3-Flash", Context: 1_000_000, Efforts: []string{"low", "high", "max"}},
	{Key: "glm-5.2", Name: "GLM-5.2", Context: 1_000_000, Efforts: []string{"high", "xhigh"}},
	{Key: "glm-5.1", Name: "GLM-5.1", Context: 200_000},
	{Key: "glm-5v-turbo", Name: "GLM-5v-Turbo", Context: 200_000},
	{Key: "minimax-m3", Name: "MiniMax-M3", Context: 512_000},
	{Key: "kimi-k3-1", Name: "Kimi-K3", Context: 1_000_000, Efforts: []string{"low", "high", "xhigh"}},
	{Key: "kimi-k2.7", Name: "Kimi-K2.7-Code", Context: 256_000},
	{Key: "kimi-k2.6", Name: "Kimi-K2.6", Context: 256_000},
	{Key: "deepseek-v4-pro", Name: "Deepseek-V4-Pro", Context: 1_000_000, Efforts: []string{"none", "high", "xhigh"}},
}

// aiFallbackCatalogue is the international build's list: the named tiers its
// client shows and the vendor models behind them.
var aiFallbackCatalogue = []workbuddyModelSeed{
	{Key: "default-model", Name: "Default", Context: 176_000},
	{Key: "fast-model", Name: "Fast", Context: 200_000},
	{Key: "balanced-model", Name: "Balanced", Context: 256_000},
	{Key: "primary-model", Name: "Primary", Context: 272_000},
	{Key: "deep-model", Name: "Deep", Context: 176_000},
	{Key: "gpt-5.5", Name: "GPT-5.5", Context: 1_000_000},
	{Key: "gpt-5.4", Name: "GPT-5.4", Context: 272_000},
	{Key: "gpt-5.3-codex", Name: "GPT-5.3-Codex", Context: 272_000},
	{Key: "gemini-3.1-pro", Name: "Gemini-3.1-Pro", Context: 400_000},
	{Key: "gemini-3.5-flash", Name: "Gemini-3.5-Flash", Context: 1_000_000},
	{Key: "glm-5.3", Name: "GLM-5.3", Context: 1_000_000, Efforts: []string{"low", "high", "max"}},
	{Key: "glm-5.2", Name: "GLM-5.2", Context: 1_000_000, Efforts: []string{"high", "xhigh"}},
	{Key: "hy3", Name: "Hy3", Context: 192_000, Efforts: []string{"low", "high"}},
	{Key: "kimi-k3", Name: "Kimi-K3", Context: 1_000_000},
	{Key: "kimi-k2.6", Name: "Kimi-K2.6", Context: 256_000},
	{Key: "minimax-m3", Name: "MiniMax-M3", Context: 512_000},
}

// workbuddyModel is one normalized listing entry.
type workbuddyModel struct {
	Key     string
	Name    string
	Context int64
	Output  int64
	Images  bool
	Efforts []string
	Default string
}

// configEntry is one model of WorkBuddy's product config, in the shape its own
// client reads.
type configEntry struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	MaxInputTokens    int64  `json:"maxInputTokens"`
	MaxOutputTokens   int64  `json:"maxOutputTokens"`
	SupportsImages    bool   `json:"supportsImages"`
	SupportsReasoning bool   `json:"supportsReasoning"`
	OnlyReasoning     bool   `json:"onlyReasoning"`
	Reasoning         *struct {
		SupportedEfforts   []string `json:"supportedEfforts"`
		Effort             string   `json:"effort"`
		DefaultEffort      string   `json:"defaultEffort"`
		CanDisableThinking *bool    `json:"canDisableThinking"`
	} `json:"reasoning"`
}

type productConfig struct {
	Agents []struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	} `json:"agents"`
	Models []configEntry `json:"models"`
}

// effortOrder is the reasoning levels in the order a client should show them.
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func rankEffort(name string) int {
	for i, v := range effortOrder {
		if v == name {
			return i
		}
	}
	return len(effortOrder)
}

// effortsOf is a model's levels: the config's own list when it names one, else
// the levels WorkBuddy answers when the model reasons at all. A model that cannot
// turn thinking off gets no "none" level.
func effortsOf(d configEntry) (levels []string, def string) {
	said := []string(nil)
	if d.Reasoning != nil {
		said = d.Reasoning.SupportedEfforts
	}
	if len(said) > 0 {
		levels = append([]string(nil), said...)
	} else if d.SupportsReasoning {
		set := map[string]bool{"low": true, "high": true, "max": true}
		if d.Reasoning != nil && d.Reasoning.Effort != "" {
			set[d.Reasoning.Effort] = true
		}
		for _, e := range effortOrder {
			if set[e] {
				levels = append(levels, e)
			}
		}
	}
	if len(levels) == 0 {
		return nil, ""
	}
	sort.Slice(levels, func(i, j int) bool {
		a, b := rankEffort(levels[i]), rankEffort(levels[j])
		if a != b {
			return a < b
		}
		return levels[i] < levels[j]
	})
	off := true
	if d.OnlyReasoning {
		off = false
	}
	if d.Reasoning != nil && d.Reasoning.CanDisableThinking != nil && !*d.Reasoning.CanDisableThinking {
		off = false
	}
	// The flash model turns thinking off for level "none" though the config
	// says onlyReasoning, so it keeps the level anyway.
	if offTaken(d.ID) {
		off = true
	}
	if off && levels[0] != "none" {
		levels = append([]string{"none"}, levels...)
	}
	if !off && levels[0] == "none" {
		levels = levels[1:]
	}
	if d.Reasoning != nil {
		for _, own := range []string{d.Reasoning.DefaultEffort, d.Reasoning.Effort} {
			if own != "" && contains(levels, own) {
				def = own
				break
			}
		}
	}
	return levels, def
}

// offTaken are the models that answer "none" with thinking off although the
// config marks them reasoning-only: deepseek-v4.1-flash and its paid id.
func offTaken(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "deepseek-v4.1-flash", "deepseek-v4.1-flash-sg":
		return true
	}
	return false
}

// textOnly reports a model WorkBuddy AI drops images from whatever its config
// says. The international build answers a chat to a GLM model with a system
// reminder that the model does not take images, having removed them; the China
// build's GLM models read them.
func textOnly(spec siteSpec, id string) bool {
	return spec.AI && strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), "glm-")
}

// messagesOnly reports a model this build serves on Anthropic's Messages API
// rather than chat: the China build's Claude models. The executor speaks chat
// completions only, so those models are not advertised: a chat to one is refused.
func messagesOnly(spec siteSpec, id string) bool {
	return !spec.AI && strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), "claude-")
}

func modelFromSeed(s workbuddyModelSeed) workbuddyModel {
	levels := append([]string(nil), s.Efforts...)
	return workbuddyModel{Key: s.Key, Name: s.Name, Context: s.Context, Images: true, Efforts: levels}
}

func fallbackModels(spec siteSpec) []workbuddyModel {
	out := make([]workbuddyModel, 0, len(spec.Fallback))
	for _, seed := range spec.Fallback {
		m := modelFromSeed(seed)
		if textOnly(spec, m.Key) {
			m.Images = false
		}
		out = append(out, m)
	}
	return out
}

// parseConfig reads WorkBuddy's product config: the models of its "cli" agent,
// which is the agent a chat client runs as, each with its details.
func parseConfig(body []byte) ([]workbuddyModel, error) {
	var cfg productConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, errors.New("WorkBuddy returned an invalid product config")
	}
	ids := make([]string, 0, len(cfg.Models))
	for _, agent := range cfg.Agents {
		if agent.Name != "cli" {
			continue
		}
		ids = append(ids, agent.Models...)
	}
	if len(ids) == 0 {
		return nil, errors.New("WorkBuddy's product config lists no cli models")
	}
	byID := make(map[string]configEntry, len(cfg.Models))
	for _, m := range cfg.Models {
		byID[m.ID] = m
	}
	out := make([]workbuddyModel, 0, len(ids))
	first := make(map[string]string, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		d := byID[id]
		m := workbuddyModel{Key: id, Name: firstNonEmpty(d.Name, id), Context: d.MaxInputTokens, Output: d.MaxOutputTokens, Images: d.SupportsImages}
		m.Efforts, m.Default = effortsOf(d)
		// Two entries the config names alike (deepseek-v4.1-flash and its -sg
		// variant are both "Deepseek-V4.1-Flash") are told apart by their id.
		if prev, seen := first[m.Name]; seen {
			rest := ""
			if strings.HasPrefix(m.Key, prev+"-") {
				rest = m.Key[len(prev)+1:]
			}
			if rest != "" {
				m.Name = m.Name + " (" + strings.ToUpper(rest) + ")"
			} else {
				m.Name = m.Name + " (" + m.Key + ")"
			}
		} else {
			first[m.Name] = m.Key
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("WorkBuddy's product config lists no usable model")
	}
	return out, nil
}

type listingCacheEntry struct {
	at     time.Time
	models []workbuddyModel
}

var listingCache struct {
	sync.Mutex
	entries map[string]listingCacheEntry
}

const listingTTL = 10 * time.Minute

func listingKey(c credential) string { return c.Site + "\x00" + c.UID }

// fetchListing reads the account's own model list. A failed read keeps the last
// good list for that account, and the build's shipped list when there is none.
func fetchListing(sender httpSender, c credential, spec siteSpec, force bool) []workbuddyModel {
	key := listingKey(c)
	if !force {
		listingCache.Lock()
		if entry, ok := listingCache.entries[key]; ok && time.Since(entry.at) < listingTTL {
			listingCache.Unlock()
			return entry.models
		}
		listingCache.Unlock()
	}
	models, err := readListing(sender, c, spec)
	if err != nil {
		listingCache.Lock()
		entry, ok := listingCache.entries[key]
		listingCache.Unlock()
		if ok && len(entry.models) > 0 {
			return entry.models
		}
		return fallbackModels(spec)
	}
	listingCache.Lock()
	if listingCache.entries == nil {
		listingCache.entries = map[string]listingCacheEntry{}
	}
	listingCache.entries[key] = listingCacheEntry{at: time.Now(), models: models}
	listingCache.Unlock()
	return models
}

func readListing(sender httpSender, c credential, spec siteSpec) ([]workbuddyModel, error) {
	headers := http.Header{}
	signHeaders(spec, c, headers)
	headers.Set("Accept", "application/json")
	raw, err := wbCall(sender, "GET", spec.Endpoint+modelsPath, headers, nil)
	if err != nil {
		return nil, fmt.Errorf("%s model config: %s", spec.Name, readableFailure(err))
	}
	return usableModels(spec, raw)
}

// usableModels drops what this executor cannot serve: models the China build
// serves only on Anthropic's Messages API, whose chat endpoint answers a refusal.
func usableModels(spec siteSpec, body []byte) ([]workbuddyModel, error) {
	models, err := parseConfig(body)
	if err != nil {
		return nil, err
	}
	out := make([]workbuddyModel, 0, len(models))
	for _, m := range models {
		if messagesOnly(spec, m.Key) {
			continue
		}
		if textOnly(spec, m.Key) {
			m.Images = false
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("WorkBuddy's product config lists no usable model")
	}
	return out, nil
}

// findModel looks a requested model up by key, case-insensitively. A miss
// refreshes the list once before falling back to the shipped one.
func findModel(sender httpSender, c credential, spec siteSpec, model string) (workbuddyModel, bool) {
	want := strings.ToLower(strings.TrimSpace(model))
	look := func(models []workbuddyModel) (workbuddyModel, bool) {
		for _, m := range models {
			if strings.ToLower(m.Key) == want {
				return m, true
			}
		}
		return workbuddyModel{}, false
	}
	if m, ok := look(fetchListing(sender, c, spec, false)); ok {
		return m, true
	}
	if m, ok := look(fetchListing(sender, c, spec, true)); ok {
		return m, true
	}
	for _, seed := range spec.Fallback {
		if strings.ToLower(seed.Key) == want {
			return modelFromSeed(seed), true
		}
	}
	return workbuddyModel{}, false
}

func modelInfo(m workbuddyModel) pluginapi.ModelInfo {
	id := strings.ToLower(strings.TrimSpace(m.Key))
	info := pluginapi.ModelInfo{
		ID:                         id,
		Name:                       m.Name,
		DisplayName:                m.Name,
		Object:                     "model",
		OwnedBy:                    credentialProvider,
		Type:                       credentialProvider,
		ContextLength:              m.Context,
		InputTokenLimit:            m.Context,
		OutputTokenLimit:           m.Output,
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
		SupportedGenerationMethods: []string{"chat.completions"},
	}
	if m.Images {
		info.SupportedInputModalities = []string{"text", "image"}
	}
	if len(m.Efforts) > 0 {
		info.Thinking = &pluginapi.ThinkingSupport{Levels: append([]string(nil), m.Efforts...)}
	}
	return info
}

func modelResponse(models []workbuddyModel) pluginapi.ModelResponse {
	out := pluginapi.ModelResponse{Provider: credentialProvider, Models: make([]pluginapi.ModelInfo, 0, len(models))}
	for _, m := range models {
		if strings.TrimSpace(m.Key) == "" {
			continue
		}
		out.Models = append(out.Models, modelInfo(m))
	}
	return out
}

// staticModels publishes the shipped list for the configured fallback build.
func staticModels() pluginapi.ModelResponse {
	spec, ok := siteFor(getConfig().Site)
	if !ok {
		spec, _ = siteFor(siteWorkBuddy)
	}
	return modelResponse(fallbackModels(spec))
}

func modelsForAuth(req pluginapi.AuthModelRequest, sender httpSender) (pluginapi.ModelResponse, error) {
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	spec, ok := siteFor(c.Site)
	if !ok {
		return pluginapi.ModelResponse{}, errors.New("invalid WorkBuddy credential")
	}
	return modelResponse(fetchListing(sender, c, spec, false)), nil
}

// modelPrefix matches a provider prefix a client may put on a model id.
var modelPrefix = regexp.MustCompile(`^[a-z0-9-]+/(.+)$`)

func trimModelPrefix(model string) string {
	if m := modelPrefix.FindStringSubmatch(model); m != nil {
		return m[1]
	}
	return model
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
