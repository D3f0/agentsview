package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type bobSessionJSON struct {
	SessionID   string           `json:"sessionId"`
	ProjectHash string           `json:"projectHash"`
	StartTime   string           `json:"startTime"`
	LastUpdated string           `json:"lastUpdated"`
	Messages    []bobMessageJSON `json:"messages"`
}

type bobMessageJSON struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Content   string            `json:"content"`
	Timestamp string            `json:"timestamp"`
	Model     string            `json:"model"`
	Thoughts  []bobThoughtJSON  `json:"thoughts"`
	Tokens    bobTokensJSON     `json:"tokens"`
	ToolCalls []bobToolCallJSON `json:"toolCalls"`
}

type bobThoughtJSON struct {
	Description string `json:"description"`
	Timestamp   string `json:"timestamp"`
}

type bobTokensJSON struct {
	Input    int `json:"input"`
	Output   int `json:"output"`
	Cached   int `json:"cached"`
	Thoughts int `json:"thoughts"`
}

type bobToolCallJSON struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Args          json.RawMessage `json:"args"`
	Status        string          `json:"status"`
	Timestamp     string          `json:"timestamp"`
	Result        json.RawMessage `json:"result"`
	ResultDisplay string          `json:"resultDisplay"`
}

func parseBobSession(path, machine string) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat Bob session %s: %w", path, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open Bob session %s: %w", path, err)
	}
	defer file.Close()

	var raw bobSessionJSON
	if err := json.NewDecoder(file).Decode(&raw); err != nil {
		return nil, nil, fmt.Errorf("decode Bob session %s: %w", path, err)
	}
	if len(raw.Messages) == 0 {
		return nil, nil, nil
	}

	messages := make([]ParsedMessage, 0, len(raw.Messages))
	firstMessage := ""
	startedAt := parseTimestamp(raw.StartTime)
	endedAt := parseTimestamp(raw.LastUpdated)
	userCount := 0
	for _, entry := range raw.Messages {
		ts := parseTimestamp(entry.Timestamp)
		if !ts.IsZero() {
			if startedAt.IsZero() {
				startedAt = ts
			}
			if endedAt.IsZero() || ts.After(endedAt) {
				endedAt = ts
			}
		}

		switch entry.Type {
		case "user":
			content := strings.TrimSpace(entry.Content)
			if content == "" {
				continue
			}
			if firstMessage == "" {
				firstMessage = truncate(strings.ReplaceAll(content, "\n", " "), 300)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       len(messages),
				Role:          RoleUser,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
			})
			userCount++

		case "bob-shell":
			content, thinking := bobVisibleContent(entry.Content)
			toolCalls := bobToolCalls(entry.ToolCalls)
			if content == "" && len(toolCalls) == 0 {
				continue
			}
			model := entry.Model
			if model == "" {
				model = "premium"
			}
			pm := ParsedMessage{
				Ordinal:       len(messages),
				Role:          RoleAssistant,
				Content:       content,
				ThinkingText:  thinking,
				Timestamp:     ts,
				Model:         model,
				HasThinking:   thinking != "",
				HasToolUse:    len(toolCalls) > 0,
				ContentLength: len(content),
				ToolCalls:     toolCalls,
			}
			applyBobTokenUsage(&pm, entry.Tokens)
			messages = append(messages, pm)
		}
	}
	if len(messages) == 0 {
		return nil, nil, nil
	}

	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	project := raw.ProjectHash
	if project == "" {
		project = "bob"
	}
	sessionID := "bob:" + stem
	sess := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentBob,
		SourceSessionID:  raw.SessionID,
		FirstMessage:     firstMessage,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File:             FileInfo{Path: path, Size: info.Size(), Mtime: info.ModTime().UnixNano()},
	}
	accumulateMessageTokenUsage(sess, messages)
	return sess, messages, nil
}

func bobVisibleContent(content string) (string, string) {
	var thinking []string
	var visible strings.Builder
	for {
		start := strings.Index(content, "<thinking>")
		if start < 0 {
			visible.WriteString(content)
			break
		}
		visible.WriteString(content[:start])
		rest := content[start+len("<thinking>"):]
		end := strings.Index(rest, "</thinking>")
		if end < 0 {
			visible.WriteString(content[start:])
			break
		}
		if text := strings.TrimSpace(rest[:end]); text != "" {
			thinking = append(thinking, text)
		}
		content = rest[end+len("</thinking>"):]
	}
	think := strings.Join(thinking, "\n\n")
	text := strings.TrimSpace(visible.String())
	if think != "" {
		block := "[Thinking]\n" + think + "\n[/Thinking]"
		if text != "" {
			text = block + "\n\n" + text
		} else {
			text = block
		}
	}
	return text, think
}

func bobToolCalls(raw []bobToolCallJSON) []ParsedToolCall {
	if len(raw) == 0 {
		return nil
	}
	calls := make([]ParsedToolCall, 0, len(raw))
	for _, call := range raw {
		if call.Name == "" {
			continue
		}
		input := ""
		if len(call.Args) > 0 && string(call.Args) != "null" {
			input = string(call.Args)
		}
		parsed := ParsedToolCall{
			ToolUseID: call.ID,
			ToolName:  call.Name,
			Category:  NormalizeToolCategory(call.Name),
			InputJSON: input,
		}
		if display := strings.TrimSpace(call.ResultDisplay); display != "" {
			parsed.ResultEvents = []ParsedToolResultEvent{{
				ToolUseID: call.ID,
				Source:    "bob",
				Status:    call.Status,
				Content:   display,
				Timestamp: parseTimestamp(call.Timestamp),
			}}
		}
		calls = append(calls, parsed)
	}
	return calls
}

func applyBobTokenUsage(pm *ParsedMessage, tokens bobTokensJSON) {
	if tokens.Input == 0 && tokens.Output == 0 && tokens.Cached == 0 && tokens.Thoughts == 0 {
		return
	}
	normalized := map[string]int{
		"input_tokens":            tokens.Input,
		"output_tokens":           tokens.Output,
		"cache_read_input_tokens": tokens.Cached,
		"reasoning_tokens":        tokens.Thoughts,
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return
	}
	pm.TokenUsage = raw
	pm.ContextTokens = tokens.Input + tokens.Cached
	pm.OutputTokens = tokens.Output
	pm.HasContextTokens = tokens.Input != 0 || tokens.Cached != 0
	pm.HasOutputTokens = tokens.Output != 0
	pm.tokenPresenceKnown = true
}

func bobSessionIDFromPath(_ string, path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func isBobSourcePath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) >= 3 && parts[0] == "tmp" && parts[len(parts)-2] == "chats" &&
		strings.HasPrefix(parts[len(parts)-1], "session-") &&
		strings.HasSuffix(parts[len(parts)-1], ".json")
}

type bobProviderFactory struct{ def AgentDef }

func newBobProviderFactory(def AgentDef) ProviderFactory {
	return bobProviderFactory{def: cloneAgentDef(def)}
}

func (f bobProviderFactory) Definition() AgentDef       { return cloneAgentDef(f.def) }
func (f bobProviderFactory) Capabilities() Capabilities { return bobProviderCapabilities() }
func (f bobProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &bobProvider{
		ProviderBase: ProviderBase{Def: cloneAgentDef(f.def), Caps: bobProviderCapabilities(), Config: cfg},
		sources:      newBobSourceSet(cfg.Roots),
	}
}

type bobProvider struct {
	ProviderBase
	sources JSONLSourceSet
}

func (p *bobProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}
func (p *bobProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}
func (p *bobProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}
func (p *bobProvider) SourcesForChangedPath(ctx context.Context, req ChangedPathRequest) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}
func (p *bobProvider) FindSource(ctx context.Context, req FindSourceRequest) (SourceRef, bool, error) {
	return p.sources.FindSource(ctx, ProviderFindRequestWithRawSessionID(p.Def, req))
}
func (p *bobProvider) Fingerprint(ctx context.Context, source SourceRef) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}
func (p *bobProvider) Parse(ctx context.Context, req ParseRequest) (ParseOutcome, error) {
	req.Machine = firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	return p.sources.Parse(ctx, req)
}

func newBobSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentBob, roots,
		WithRecursive(),
		WithExtensions(".json"),
		WithContentHashing(),
		WithFollowSymlinkFiles(),
		WithDescendPath(func(root, path string) bool {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return false
			}
			rel = filepath.ToSlash(rel)
			return rel == "." || rel == "tmp" || strings.HasPrefix(rel, "tmp/")
		}),
		WithIncludePath(isBobSourcePath),
		WithSessionIDFromPath(bobSessionIDFromPath),
		WithParseFile(func(_ context.Context, path string, req ParseRequest) ([]ParseResult, []string, error) {
			sess, messages, err := parseBobSession(path, req.Machine)
			if err != nil || sess == nil {
				return nil, nil, err
			}
			if req.Fingerprint.Hash != "" {
				sess.File.Hash = req.Fingerprint.Hash
			}
			return []ParseResult{{Session: *sess, Messages: messages}}, nil, nil
		}),
	)
}

func bobProviderCapabilities() Capabilities {
	return Capabilities{
		Source: func() SourceCapabilities {
			caps := jsonlFileProviderSourceCapabilities()
			caps.StreamingDiscovery = CapabilitySupported
			return caps
		}(),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Model:                CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResultEvents:     CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
		},
	}
}

var _ Provider = (*bobProvider)(nil)
var _ ProviderFactory = bobProviderFactory{}
