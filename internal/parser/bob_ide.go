package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/money"
)

const bobIDEHistory = "api_conversation_history.json"

// The API history is authoritative for complete turns. The UI companion adds
// execution results and billing, not a second copy of the conversation.
func newBobIDESourceSet(roots []string) singleFileSourceSet {
	return NewSingleFileSourceSet(AgentBob, roots,
		WithStreamingFileDiscovery(func(ctx context.Context, root string, yield func(singleFileMatch) error) error {
			return streamDirectoryEntries(ctx, filepath.Join(root, "tasks"), func(entry os.DirEntry) error {
				if !entry.IsDir() || strings.HasPrefix(entry.Name(), "_") {
					return nil
				}
				path := filepath.Join(root, "tasks", entry.Name(), bobIDEHistory)
				info, err := os.Lstat(path)
				if os.IsNotExist(err) {
					return nil
				}
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					return nil
				}
				return yield(singleFileMatch{Path: path})
			})
		}),
		WithFileWatchRoots(func(roots []string) []WatchRoot {
			out := make([]WatchRoot, 0, len(roots))
			for _, root := range roots {
				out = append(out, WatchRoot{Path: filepath.Join(root, "tasks"), Recursive: true, IncludeGlobs: []string{bobIDEHistory, "ui_messages.json"}, DebounceKey: "bob:ide:" + root})
			}
			return out
		}),
		WithFileChangedPathClassifier(bobIDEClassifyPath),
		WithFileLookup(func(_ context.Context, root, id string) (singleFileMatch, bool) {
			return bobIDEClassifyPath(root, filepath.Join(root, "tasks", id, bobIDEHistory), false)
		}),
		WithFileFingerprint(bobIDEFingerprint),
		WithFileParse(bobIDEParseFile),
	)
}

func bobIDEClassifyPath(root, path string, allowMissing bool) (singleFileMatch, bool) {
	rel, err := filepath.Rel(filepath.Join(root, "tasks"), filepath.Clean(path))
	if err != nil {
		return singleFileMatch{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == ".." || strings.HasPrefix(parts[0], "_") || (parts[1] != bobIDEHistory && parts[1] != "ui_messages.json") {
		return singleFileMatch{}, false
	}
	canonical := filepath.Join(root, "tasks", parts[0], bobIDEHistory)
	return singleFileMatch{Path: canonical}, allowMissing || IsRegularFile(canonical)
}

func bobIDEFingerprint(ctx context.Context, src singleFileSource) (SourceFingerprint, error) {
	h := sha256.New()
	var fp SourceFingerprint
	for _, name := range []string{bobIDEHistory, "ui_messages.json"} {
		if err := ctx.Err(); err != nil {
			return fp, err
		}
		path := filepath.Join(filepath.Dir(src.Path), name)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) && name != bobIDEHistory {
			fmt.Fprintf(h, "%s:missing\x00", name)
			continue
		}
		if err != nil {
			return fp, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return fp, err
		}
		fp.Size += info.Size()
		fp.MTimeNS = max(fp.MTimeNS, info.ModTime().UnixNano())
		fmt.Fprintf(h, "%s:%d\x00", name, len(data))
		h.Write(data)
	}
	fp.Hash = hex.EncodeToString(h.Sum(nil))
	return fp, nil
}

type bobIDEEntry struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Timestamp int64           `json:"ts"`
}

type bobIDEBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func bobIDEParseFile(ctx context.Context, src singleFileSource, req ParseRequest) ([]ParseResult, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, nil, err
	}
	var entries []bobIDEEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, nil, fmt.Errorf("decode Bob IDE history: %w", err)
	}
	messages := make([]ParsedMessage, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		role := RoleType(entry.Role)
		if role != RoleUser && role != RoleAssistant && role != RoleSystem {
			continue
		}
		msg := ParsedMessage{Role: role, Timestamp: time.UnixMilli(entry.Timestamp).UTC(), Ordinal: len(messages)}
		var text string
		if json.Unmarshal(entry.Content, &text) == nil {
			msg.Content = text
		} else {
			var blocks []bobIDEBlock
			if err := json.Unmarshal(entry.Content, &blocks); err != nil {
				return nil, nil, fmt.Errorf("decode Bob IDE content: %w", err)
			}
			var texts []string
			for _, block := range blocks {
				switch block.Type {
				case "text":
					texts = append(texts, block.Text)
				case "image":
					texts = append(texts, "[image]")
				case "thinking":
					msg.ThinkingText += block.Text
				case "tool_use":
					msg.ToolCalls = append(msg.ToolCalls, ParsedToolCall{ToolUseID: block.ID, ToolName: block.Name, Category: NormalizeToolCategory(block.Name), InputJSON: string(block.Input)})
				case "tool_result":
					status := "completed"
					if block.IsError {
						status = "errored"
					}
					msg.ToolResults = append(msg.ToolResults, ParsedToolResult{ToolUseID: block.ToolUseID, Source: "bob-ide", Status: status, ContentRaw: string(block.Content), ContentLength: len(DecodeContent(string(block.Content)))})
				}
			}
			msg.Content = strings.Join(texts, "\n")
		}
		msg.HasThinking = msg.ThinkingText != ""
		msg.HasToolUse = len(msg.ToolCalls) > 0
		msg.ContentLength = len(msg.Content)
		if msg.Content == "" && !msg.HasToolUse && len(msg.ToolResults) == 0 && !msg.HasThinking {
			continue
		}
		if len(msg.ToolResults) > 0 {
			msg.IsSystem = true
			msg.SourceSubtype = SourceSubtypeToolResult
		}
		messages = append(messages, msg)
	}
	uiPath := filepath.Join(filepath.Dir(src.Path), "ui_messages.json")
	uiMessages, _, _, err := parseRooCodeMessages(ctx, uiPath, "")
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if len(messages) == 0 {
		messages = uiMessages
	} else {
		// UI events precede the API record finalized for that response. Attach
		// tool/result metadata to that response; never match transcript bodies.
		nativeCalls := make([]bool, len(messages))
		for i := range messages {
			nativeCalls[i] = len(messages[i].ToolCalls) > 0
		}
		for _, ui := range uiMessages {
			if len(ui.ToolCalls) == 0 {
				if len(ui.ToolResults) > 0 || ui.Role == RoleSystem {
					messages = append(messages, ui)
				}
				continue
			}
			idx := -1
			for i := range messages {
				if messages[i].Role == RoleAssistant && !messages[i].Timestamp.Before(ui.Timestamp) {
					idx = i
					break
				}
			}
			if idx < 0 {
				ui.Content = ""
				ui.ContentLength = 0
				messages = append(messages, ui)
				continue
			}
			// Native tool blocks already carry identity. UI calls have no matching
			// native ID in the observed XML format, so only enrich XML responses.
			if idx < len(nativeCalls) && !nativeCalls[idx] {
				messages[idx].ToolCalls = append(messages[idx].ToolCalls, ui.ToolCalls...)
				messages[idx].HasToolUse = true
			}
		}
	}
	id := filepath.Base(filepath.Dir(src.Path))
	events, peak, output, err := bobIDEUsage(ctx, uiPath, "bob:"+id)
	if err != nil {
		return nil, nil, err
	}
	if len(messages) == 0 && len(events) == 0 {
		return nil, nil, nil
	}
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].Timestamp.Before(messages[j].Timestamp) })
	session := ParsedSession{ID: "bob:" + id, SourceSessionID: id, Agent: AgentBob, Machine: req.Machine, Project: req.Source.ProjectHint, SourceVersion: "bob-ide-task-v1", File: FileInfo{Path: src.Path, Hash: req.Fingerprint.Hash, Size: req.Fingerprint.Size, Mtime: req.Fingerprint.MTimeNS}}
	if session.Project == "" {
		session.Project = "bob"
	}
	for i := range messages {
		msg := &messages[i]
		msg.Ordinal = i
		if session.StartedAt.IsZero() || msg.Timestamp.Before(session.StartedAt) {
			session.StartedAt = msg.Timestamp
		}
		if msg.Timestamp.After(session.EndedAt) {
			session.EndedAt = msg.Timestamp
		}
		for ci := range msg.ToolCalls {
			for ei := range msg.ToolCalls[ci].ResultEvents {
				event := &msg.ToolCalls[ci].ResultEvents[ei]
				if event.Timestamp.IsZero() {
					event.Timestamp = msg.Timestamp
				}
				if event.Source == "" {
					event.Source = "bob-ide"
				}
				if event.Timestamp.After(session.EndedAt) {
					session.EndedAt = event.Timestamp
				}
			}
		}
		if msg.Role == RoleUser && !msg.IsSystem {
			session.UserMessageCount++
			if session.FirstMessage == "" {
				session.FirstMessage = truncate(strings.ReplaceAll(msg.Content, "\n", " "), 300)
			}
		}
	}
	session.MessageCount = len(messages)
	session.UsageEvents = events
	for _, event := range events {
		ts := parseTimestamp(event.OccurredAt)
		if session.StartedAt.IsZero() || ts.Before(session.StartedAt) {
			session.StartedAt = ts
		}
		if ts.After(session.EndedAt) {
			session.EndedAt = ts
		}
	}
	session.PeakContextTokens, session.HasPeakContextTokens = peak, len(events) > 0
	session.TotalOutputTokens, session.HasTotalOutputTokens = output, len(events) > 0
	session.aggregateTokenPresenceKnown = len(events) > 0
	return []ParseResult{{Session: session, Messages: messages, UsageEvents: events}}, nil, nil
}

func bobIDEUsage(ctx context.Context, path, sessionID string) ([]ParsedUsageEvent, int, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, 0, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, 0, 0, nil
	}
	if err != nil {
		return nil, 0, 0, err
	}
	var entries []rooCodeMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, 0, 0, err
	}
	var events []ParsedUsageEvent
	peak, output := 0, 0
	for i, entry := range entries {
		if entry.Say != "api_req_started" || entry.Partial {
			continue
		}
		var usage struct {
			Input      int             `json:"tokensIn"`
			Output     int             `json:"tokensOut"`
			CacheRead  int             `json:"cacheReads"`
			CacheWrite int             `json:"cacheWrites"`
			Cost       json.RawMessage `json:"cost"`
		}
		if err := json.Unmarshal([]byte(entry.Text), &usage); err != nil {
			return nil, 0, 0, fmt.Errorf("decode Bob IDE usage: %w", err)
		}
		if usage.Input == 0 && usage.Output == 0 && usage.CacheRead == 0 && usage.CacheWrite == 0 && len(usage.Cost) == 0 {
			continue
		}
		event := ParsedUsageEvent{SessionID: sessionID, Source: "bob-ide-request", InputTokens: usage.Input, OutputTokens: usage.Output, CacheReadInputTokens: usage.CacheRead, CacheCreationInputTokens: usage.CacheWrite, OccurredAt: time.UnixMilli(entry.Timestamp).UTC().Format(time.RFC3339Nano), DedupKey: fmt.Sprintf("request:%s:%d:%d", sessionID, entry.Timestamp, i)}
		if len(usage.Cost) > 0 && string(usage.Cost) != "null" {
			cost, err := money.ParseDollars(string(usage.Cost))
			if err != nil {
				return nil, 0, 0, err
			}
			event.Cost, event.CostStatus, event.CostSource = &cost, "exact", "bob-reported"
		}
		peak = max(peak, usage.Input+usage.CacheRead+usage.CacheWrite)
		output += usage.Output
		events = append(events, event)
	}
	return events, peak, output, nil
}
