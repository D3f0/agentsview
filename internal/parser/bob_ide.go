package parser

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/money"
)

// Bob 1.x IDE records the complete model conversation separately from its UI
// event stream. Neither is Roo's history_item.json: task_metadata.json contains
// only files_in_context, and is optional. The API transcript owns roles and text;
// UI events supply execution details, accounting, and otherwise absent content.
func newBobIDESourceSet(roots []string) singleFileSourceSet {
	return NewSingleFileSourceSet(AgentBob, roots,
		WithStreamingFileDiscovery(bobIDEDiscoverEach),
		WithFileWatchRoots(bobIDEWatchRoots),
		WithFileChangedPathClassifier(bobIDEClassifyPath),
		WithFileLookup(bobIDEFindFile),
		WithFileFingerprint(func(ctx context.Context, src singleFileSource) (SourceFingerprint, error) {
			return bobIDEFingerprint(ctx, src.Path)
		}),
		WithFileParse(bobIDEParseFile),
	)
}

var bobIDEFiles = []string{"api_conversation_history.json", "ui_messages.json", "task_metadata.json"}

func bobIDETasksRoot(root string) string {
	if filepath.Base(filepath.Clean(root)) == "tasks" {
		return filepath.Clean(root)
	}
	return filepath.Join(root, "tasks")
}

func bobIDEDiscoverEach(ctx context.Context, root string, yield func(singleFileMatch) error) error {
	return streamDirectoryEntries(ctx, bobIDETasksRoot(root), func(entry os.DirEntry) error {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "_") {
			return nil
		}
		dir := filepath.Join(bobIDETasksRoot(root), entry.Name())
		for _, name := range bobIDEFiles[:2] {
			info, err := os.Stat(filepath.Join(dir, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				return yield(singleFileMatch{Path: filepath.Join(dir, bobIDEFiles[0])})
			}
		}
		return nil
	})
}

func bobIDEWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{Path: bobIDETasksRoot(root), Recursive: true,
			IncludeGlobs: slices.Clone(bobIDEFiles), DebounceKey: "bob:ide:" + root})
	}
	return out
}

func bobIDEClassifyPath(root, path string, allowMissing bool) (singleFileMatch, bool) {
	rel, err := filepath.Rel(bobIDETasksRoot(root), filepath.Clean(path))
	if err != nil {
		return singleFileMatch{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == ".." || strings.HasPrefix(parts[0], "_") || !slices.Contains(bobIDEFiles, parts[1]) {
		return singleFileMatch{}, false
	}
	canonical := filepath.Join(bobIDETasksRoot(root), parts[0], bobIDEFiles[0])
	if allowMissing || IsRegularFile(canonical) || IsRegularFile(filepath.Join(filepath.Dir(canonical), bobIDEFiles[1])) {
		return singleFileMatch{Path: canonical}, true
	}
	return singleFileMatch{}, false
}

func bobIDEFindFile(ctx context.Context, root, rawID string) (singleFileMatch, bool) {
	if ctx.Err() != nil {
		return singleFileMatch{}, false
	}
	rawID = strings.TrimPrefix(rawID, "ide:")
	if rawID == "" || rawID == "." || rawID == ".." || filepath.Base(rawID) != rawID {
		return singleFileMatch{}, false
	}
	return bobIDEClassifyPath(root, filepath.Join(bobIDETasksRoot(root), rawID, bobIDEFiles[0]), false)
}

// A typed failure prevents malformed/incomplete snapshots from becoming clean
// skips (and makes errors.Is retain filesystem causes for retry decisions).
type bobIDESourceError struct {
	Path string
	Kind string
	Err  error
}

func (e *bobIDESourceError) Error() string {
	return fmt.Sprintf("Bob IDE %s source %s: %v", e.Kind, e.Path, e.Err)
}
func (e *bobIDESourceError) Unwrap() error { return e.Err }

func bobIDERead(ctx context.Context, path string, target any) error {
	if err := ctx.Err(); err != nil {
		return &bobIDESourceError{Path: path, Kind: "unavailable", Err: err}
	}
	file, err := os.Open(path)
	if err != nil {
		kind := "unavailable"
		if errors.Is(err, os.ErrNotExist) {
			kind = "incomplete"
		}
		return &bobIDESourceError{Path: path, Kind: kind, Err: err}
	}
	defer file.Close()
	data, err := io.ReadAll(ctxio.Reader{Context: ctx, Reader: file})
	recordContentRead(ctx, int64(len(data)))
	if err != nil {
		return &bobIDESourceError{Path: path, Kind: "unavailable", Err: err}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return &bobIDESourceError{Path: path, Kind: "malformed", Err: err}
	}
	return nil
}

func bobIDEFingerprint(ctx context.Context, path string) (SourceFingerprint, error) {
	fp := SourceFingerprint{}
	h := sha256.New()
	present := false
	for _, name := range bobIDEFiles {
		if err := ctx.Err(); err != nil {
			return SourceFingerprint{}, &bobIDESourceError{Path: path, Kind: "unavailable", Err: err}
		}
		part := filepath.Join(filepath.Dir(path), name)
		info, err := os.Stat(part)
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(h, "%s:absent\n", name)
			continue
		}
		if err != nil {
			return SourceFingerprint{}, &bobIDESourceError{Path: part, Kind: "unavailable", Err: err}
		}
		if !info.Mode().IsRegular() {
			return SourceFingerprint{}, &bobIDESourceError{Path: part, Kind: "malformed", Err: errors.New("expected regular file")}
		}
		present = true
		fp.Size += info.Size()
		fp.MTimeNS = max(fp.MTimeNS, info.ModTime().UnixNano())
		if err := addSiblingMetadataFingerprintPart(ctx, h, name, part, info); err != nil {
			return SourceFingerprint{}, &bobIDESourceError{Path: part, Kind: "unavailable", Err: err}
		}
	}
	if !present {
		return SourceFingerprint{}, &bobIDESourceError{Path: path, Kind: "incomplete", Err: os.ErrNotExist}
	}
	fp.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fp, nil
}

type bobIDEAPIMessage struct {
	Role    RoleType             `json:"role"`
	TS      int64                `json:"ts"`
	Content []bobIDEContentBlock `json:"content"`
}

type bobIDEContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
}

var bobIDEToolOpen = regexp.MustCompile(`<(?:execute_command|read_file|attempt_completion|apply_diff|write_to_file|update_todo_list|search_files|use_mcp_tool|access_mcp_resource|list_files|browser_action|ask_followup_question|new_task|switch_mode|insert_content|use_skill|fetch_github_issue|list_code_definition_names|search_and_replace|codebase_search|fetch_instructions)>`)
var bobIDEParameter = regexp.MustCompile(`(?s)<([a-z_]+)>(.*?)</([a-z_]+)>`)
var bobIDEExitedCode = regexp.MustCompile(`(?i)\bexited\s+with\s+code\s+([1-9]\d*)`)
var bobIDEResultHeader = regexp.MustCompile(`(?m)^\[([a-z_]+)[^\n]*\] Result:[ \t]*`)
var bobIDEWorkspace = regexp.MustCompile(`(?m)^# Current Workspace Directory \(([^\n]+)\) Files\s*$`)
var bobIDEEnvironment = regexp.MustCompile(`(?s)<environment_details>.*?</environment_details>`)

// Content keeps the complete producer transcript. DialogueText is a separate
// source-aware projection: protocol envelopes are not prose, but a completion
// result or followup question is still a message addressed to the user.
func bobIDEAPIDialogue(role RoleType, text string) string {
	if role != RoleUser && role != RoleAssistant {
		return ""
	}
	text = bobIDEEnvironment.ReplaceAllString(text, "")
	if role == RoleUser {
		if result := bobIDEResultHeader.FindStringIndex(text); result != nil {
			text = text[:result[0]]
		}
		for _, tag := range []string{"task", "user_message", "feedback", "answer"} {
			text = strings.ReplaceAll(text, "<"+tag+">", "")
			text = strings.ReplaceAll(text, "</"+tag+">", "")
		}
		return strings.TrimSpace(text)
	}
	var dialogue strings.Builder
	for offset := 0; offset < len(text); {
		loc := bobIDEToolOpen.FindStringIndex(text[offset:])
		if loc == nil {
			dialogue.WriteString(text[offset:])
			break
		}
		start, end := offset+loc[0], offset+loc[1]
		dialogue.WriteString(text[offset:start])
		name := text[start+1 : end-1]
		close := "</" + name + ">"
		n := strings.Index(text[end:], close)
		if n < 0 {
			dialogue.WriteString(text[start:])
			break
		}
		parameter := ""
		switch name {
		case "attempt_completion":
			parameter = "result"
		case "ask_followup_question":
			parameter = "question"
		}
		if parameter != "" {
			body := text[end : end+n]
			open := "<" + parameter + ">"
			if begin := strings.Index(body, open); begin >= 0 {
				value := body[begin+len(open):]
				if finish := strings.Index(value, "</"+parameter+">"); finish >= 0 {
					dialogue.WriteString(value[:finish])
				}
			}
		}
		offset = end + n + len(close)
	}
	return strings.TrimSpace(dialogue.String())
}

func bobIDEUIDialogue(raw rooCodeMessage, role RoleType) string {
	if role != RoleUser && role != RoleAssistant || raw.Say == "reasoning" {
		return ""
	}
	if raw.Ask == "followup" {
		var payload struct {
			Question string `json:"question"`
		}
		if json.Unmarshal([]byte(raw.Text), &payload) == nil && payload.Question != "" {
			return payload.Question
		}
	}
	switch raw.Say {
	case "text", "user_feedback", "user_feedback_diff", "completion_result":
		return bobIDEAPIDialogue(role, raw.Text)
	}
	if raw.Ask == "followup" || raw.Ask == "completion_result" {
		return bobIDEAPIDialogue(role, raw.Text)
	}
	return ""
}

// Bob's XML inputs are not XML documents: file/diff/command fields can contain
// unescaped markup. Recognize protocol envelopes, retaining the whole original
// text in Content and nested parameter values as strings rather than losing it
// through a strict XML decoder. UI JSON can enrich these inputs independently.
func bobIDEXMLTools(ctx context.Context, text, taskID string, ordinal int) []ParsedToolCall {
	var calls []ParsedToolCall
	for offset := 0; offset < len(text); {
		loc := bobIDEToolOpen.FindStringIndex(text[offset:])
		if loc == nil {
			break
		}
		start, end := offset+loc[0], offset+loc[1]
		name := text[start+1 : end-1]
		close := "</" + name + ">"
		n := strings.Index(text[end:], close)
		if n < 0 {
			offset = end
			continue
		}
		body := text[end : end+n]
		input := map[string]any{}
		for _, param := range bobIDEParameter.FindAllStringSubmatch(body, -1) {
			if param[1] == param[3] {
				input[param[1]] = param[2]
			}
		}
		input["xml"] = body
		encoded, _ := json.Marshal(input, json.Deterministic(true))
		category := NormalizeToolCategory(name)
		if name == "new_task" {
			category = "Task"
		}
		if name == "use_mcp_tool" || name == "access_mcp_resource" {
			category = "MCP"
		}
		call := ParsedToolCall{ToolUseID: fmt.Sprintf("bob:ide:%s:%d:%d", taskID, ordinal, len(calls)),
			ToolName: name, Category: category, InputJSON: string(encoded)}
		call.FilePath, _ = input["path"].(string)
		call.SkillName = inferToolSkillName(ctx, name, call.InputJSON)
		if name == "use_skill" {
			call.SkillName, _ = input["name"].(string)
		}
		calls = append(calls, call)
		offset = end + n + len(close)
	}
	return calls
}

func bobIDEParseFile(ctx context.Context, src singleFileSource, req ParseRequest) ([]ParseResult, []string, error) {
	sess, messages, err := parseBobIDESession(ctx, filepath.Dir(src.Path), req.Source.ProjectHint, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if req.Fingerprint.Hash != "" {
		sess.File = FileInfo{Path: src.Path, Size: req.Fingerprint.Size, Mtime: req.Fingerprint.MTimeNS, Hash: req.Fingerprint.Hash}
	}
	return []ParseResult{{Session: *sess, Messages: messages, UsageEvents: sess.UsageEvents}}, nil, nil
}

func parseBobIDESession(ctx context.Context, taskDir, projectHint, machine string) (*ParsedSession, []ParsedMessage, error) {
	apiPath := filepath.Join(taskDir, bobIDEFiles[0])
	uiPath := filepath.Join(taskDir, bobIDEFiles[1])
	var api []bobIDEAPIMessage
	var ui []rooCodeMessage
	if err := bobIDERead(ctx, apiPath, &api); err != nil {
		return nil, nil, err
	}
	if err := bobIDERead(ctx, uiPath, &ui); err != nil {
		return nil, nil, err
	}
	if api == nil || ui == nil {
		return nil, nil, &bobIDESourceError{Path: apiPath, Kind: "malformed", Err: errors.New("expected conversation and UI arrays")}
	}
	taskID := filepath.Base(taskDir)
	sess := &ParsedSession{ID: "bob:ide:" + taskID, SourceSessionID: taskID, Agent: AgentBob,
		SourceVersion: "bob-ide-v1", Project: projectHint, Machine: machine, Entrypoint: "ide"}
	messages := make([]ParsedMessage, 0, len(api))
	for i, raw := range api {
		if err := ctx.Err(); err != nil {
			return nil, nil, &bobIDESourceError{Path: apiPath, Kind: "unavailable", Err: err}
		}
		if raw.Role != RoleUser && raw.Role != RoleAssistant && raw.Role != RoleSystem && raw.Role != RoleTool || raw.TS <= 0 || raw.Content == nil {
			return nil, nil, &bobIDESourceError{Path: apiPath, Kind: "malformed", Err: fmt.Errorf("invalid API message %d", i)}
		}
		msg := ParsedMessage{Ordinal: i, Role: raw.Role, Timestamp: time.UnixMilli(raw.TS),
			SourceType: "api", IsSystem: raw.Role == RoleSystem}
		var parts, thinking, dialogue []string
		for _, block := range raw.Content {
			switch block.Type {
			case "text":
				parts = append(parts, block.Text)
				dialogue = append(dialogue, block.Text)
				if raw.Role == RoleAssistant {
					msg.ToolCalls = append(msg.ToolCalls, bobIDEXMLTools(ctx, block.Text, taskID, i)...)
				}
				if sess.Cwd == "" && raw.Role == RoleUser {
					for _, environment := range bobIDEEnvironment.FindAllString(block.Text, -1) {
						if match := bobIDEWorkspace.FindStringSubmatch(environment); len(match) > 0 {
							sess.Cwd = match[1]
							break
						}
					}
				}
			case "image":
				parts = append(parts, "[image]")
			case "thinking", "reasoning":
				text := block.Thinking
				if text == "" {
					text = block.Text
				}
				thinking = append(thinking, text)
				parts = append(parts, "[Thinking]\n"+text+"\n[/Thinking]")
			default:
				return nil, nil, &bobIDESourceError{Path: apiPath, Kind: "malformed", Err: fmt.Errorf("unsupported content block %q in message %d", block.Type, i)}
			}
		}
		msg.Content = strings.Join(parts, "\n")
		msg.DialogueText = new(bobIDEAPIDialogue(raw.Role, strings.Join(dialogue, "\n")))
		msg.ContentLength = len(msg.Content)
		msg.ThinkingText = strings.Join(thinking, "\n\n")
		msg.HasThinking = len(thinking) > 0
		msg.HasToolUse = len(msg.ToolCalls) > 0
		messages = append(messages, msg)
	}
	bobIDEPairAPIResults(messages)
	if err := bobIDEApplyUI(ctx, sess, &messages, ui, uiPath); err != nil {
		return nil, nil, err
	}
	slices.SortStableFunc(messages, func(a, b ParsedMessage) int { return a.Timestamp.Compare(b.Timestamp) })
	for i := range messages {
		messages[i].Ordinal = i
		if sess.StartedAt.IsZero() || messages[i].Timestamp.Before(sess.StartedAt) {
			sess.StartedAt = messages[i].Timestamp
		}
		if messages[i].Timestamp.After(sess.EndedAt) {
			sess.EndedAt = messages[i].Timestamp
		}
		if messages[i].Role == RoleUser && !messages[i].IsSystem && messages[i].DialogueText != nil && strings.TrimSpace(*messages[i].DialogueText) != "" {
			sess.UserMessageCount++
			if sess.FirstMessage == "" {
				sess.FirstMessage = truncate(strings.ReplaceAll(*messages[i].DialogueText, "\n", " "), 300)
			}
		}
	}
	for _, event := range ui {
		if event.Timestamp > 0 {
			ts := time.UnixMilli(event.Timestamp)
			if sess.StartedAt.IsZero() || ts.Before(sess.StartedAt) {
				sess.StartedAt = ts
			}
			if ts.After(sess.EndedAt) {
				sess.EndedAt = ts
			}
		}
	}
	sess.SessionName = truncate(sess.FirstMessage, 80)
	if sess.Cwd != "" {
		if project := ExtractProjectFromCwd(sess.Cwd); project != "" {
			sess.Project = project
		}
	}
	sess.MessageCount = len(messages)
	fp, err := bobIDEFingerprint(ctx, apiPath)
	if err != nil {
		return nil, nil, err
	}
	sess.File = FileInfo{Path: apiPath, Size: fp.Size, Mtime: fp.MTimeNS, Hash: fp.Hash}
	return sess, messages, nil
}

func bobIDEPairAPIResults(messages []ParsedMessage) {
	for mi := range messages {
		msg := &messages[mi]
		if msg.Role != RoleUser {
			continue
		}
		locations := bobIDEResultHeader.FindAllStringSubmatchIndex(msg.Content, -1)
		for index, loc := range locations {
			end := len(msg.Content)
			if index+1 < len(locations) {
				end = locations[index+1][0]
			}
			name := msg.Content[loc[2]:loc[3]]
			content := strings.TrimSpace(msg.Content[loc[0]:end])
			paired := false
			for prior := 0; prior < mi && !paired; prior++ {
				for ci := range messages[prior].ToolCalls {
					call := &messages[prior].ToolCalls[ci]
					if call.ToolName != name || len(call.ResultEvents) > 0 {
						continue
					}
					encoded, _ := json.Marshal(content)
					msg.ToolResults = append(msg.ToolResults, ParsedToolResult{ToolUseID: call.ToolUseID, ContentRaw: string(encoded), ContentLength: len(content)})
					status := "completed"
					if rooCommandOutputIsError(content) {
						status = "errored"
					}
					call.ResultEvents = append(call.ResultEvents, ParsedToolResultEvent{ToolUseID: call.ToolUseID, Source: "api", Status: status, Content: content, Timestamp: msg.Timestamp})
					paired = true
					break
				}
			}
		}
	}
}

func bobIDECanonicalTool(name string) string {
	switch name {
	case "readFile":
		return "read_file"
	case "appliedDiff":
		return "apply_diff"
	case "newFileCreated", "editedExistingFile", "writeToFile":
		return "write_to_file"
	case "updateTodoList":
		return "update_todo_list"
	case "searchFiles":
		return "search_files"
	case "listFilesTopLevel", "listFilesRecursive", "listFiles":
		return "list_files"
	case "newTask":
		return "new_task"
	case "insertContent":
		return "insert_content"
	}
	return name
}

func bobIDEApplyUI(ctx context.Context, sess *ParsedSession, messages *[]ParsedMessage, ui []rooCodeMessage, path string) error {
	var apiText strings.Builder
	for _, msg := range *messages {
		apiText.WriteString(msg.Content)
		apiText.WriteByte('\n')
	}
	haystack := apiText.String()
	type callRef struct{ message, call int }
	used := make(map[string]bool)
	pending := make(map[string]callRef)
	var latest *callRef
	for i, raw := range ui {
		if err := ctx.Err(); err != nil {
			return &bobIDESourceError{Path: path, Kind: "unavailable", Err: err}
		}
		if raw.Timestamp <= 0 || (raw.Type != "say" && raw.Type != "ask") {
			return &bobIDESourceError{Path: path, Kind: "malformed", Err: fmt.Errorf("invalid UI event %d", i)}
		}
		ts := time.UnixMilli(raw.Timestamp)
		if raw.Say == "api_req_started" {
			if err := bobIDEUsage(sess, raw, path, i); err != nil {
				return err
			}
			continue
		}
		if rooCodeIsMetadataSay(raw.Say) {
			continue
		}
		_, calls, _ := classifyRooCodeMessage(ctx, raw, false, i)
		for ci := range calls {
			call := calls[ci]
			name := bobIDECanonicalTool(call.ToolName)
			if call.Category == "MCP" && name != "access_mcp_resource" {
				name = "use_mcp_tool"
			}
			ref := callRef{-1, -1}
			for mi := range *messages {
				if (*messages)[mi].SourceType != "api" || (*messages)[mi].Role != RoleAssistant {
					continue
				}
				for ti, candidate := range (*messages)[mi].ToolCalls {
					if candidate.ToolName == name && !used[candidate.ToolUseID] {
						ref = callRef{mi, ti}
						break
					}
				}
				if ref.message >= 0 {
					break
				}
			}
			if ref.message < 0 {
				call.ToolName = name
				call.ToolUseID = fmt.Sprintf("bob:ide:%s:ui:%d:%d", sess.SourceSessionID, i, ci)
				*messages = append(*messages, ParsedMessage{Role: RoleAssistant, DialogueText: new(""), Timestamp: ts, SourceType: "ui", SourceSubtype: raw.Ask, ToolCalls: []ParsedToolCall{call}, HasToolUse: true})
				ref = callRef{len(*messages) - 1, 0}
			}
			native := &(*messages)[ref.message].ToolCalls[ref.call]
			used[native.ToolUseID] = true
			if len(native.ResultEvents) == 0 {
				for _, event := range call.ResultEvents {
					event.ToolUseID = native.ToolUseID
					event.Timestamp = ts
					event.Source = "ui"
					native.ResultEvents = append(native.ResultEvents, event)
				}
			}
			pending[name] = ref
			if len(native.ResultEvents) == 0 {
				latest = &ref
			} else {
				latest = nil
			}
		}
		if len(calls) > 0 {
			continue
		}
		content := raw.Text
		if content == "" && len(raw.Images) > 0 {
			content = strings.TrimSpace(strings.Repeat("[image] ", len(raw.Images)))
		}
		resultName := ""
		switch raw.Say {
		case "command_output":
			resultName = "execute_command"
		case "mcp_server_response":
			resultName = "use_mcp_tool"
			if _, ok := pending[resultName]; !ok {
				resultName = "access_mcp_resource"
			}
		case "browser_action_result":
			resultName = "browser_action"
		case "subtask_result":
			resultName = "new_task"
		case "completion_result":
			resultName = "attempt_completion"
		}
		failure := raw.Ask == "api_req_failed" || rooCodeIsToolErrorEvent(raw.Say, raw.Ask) || rooCodeIsErrorSay(raw.Say)
		ref, ok := pending[resultName]
		if failure && latest != nil {
			ref = *latest
			ok = true
		}
		if ok && (resultName != "" || failure) {
			call := &(*messages)[ref.message].ToolCalls[ref.call]
			status := "completed"
			if failure || rooCommandOutputIsError(content) || bobIDEExitedCode.MatchString(content) || rooToolCallErrored(call) {
				status = "errored"
			}
			call.ResultEvents = append(call.ResultEvents, ParsedToolResultEvent{ToolUseID: call.ToolUseID, Source: "ui", Status: status, Content: content, Timestamp: ts})
			if resultName == "execute_command" && !failure {
				latest = &ref
			} else {
				latest = nil
				delete(pending, call.ToolName)
			}
			continue
		}
		if raw.Say == "text" || raw.Say == "user_feedback" || raw.Ask == "followup" {
			clear(pending)
			latest = nil
		}
		if content != "" && strings.Contains(haystack, content) && raw.Reasoning == "" {
			continue
		}
		if raw.Ask == "followup" {
			var payload struct {
				Question string `json:"question"`
			}
			if json.Unmarshal([]byte(raw.Text), &payload) == nil && payload.Question != "" && strings.Contains(haystack, payload.Question) {
				continue
			}
		}
		if raw.Say == "browser_action" || raw.Ask == "browser_action_launch" {
			if strings.Contains(haystack, "<browser_action>") {
				continue
			}
		}
		if content == "" && raw.Reasoning == "" && !rooCodeIsCompactBoundary(raw.Say) {
			continue
		}
		role, _, _ := classifyRooCodeMessage(ctx, raw, i == 0, i)
		if (resultName != "" && raw.Say != "completion_result") || failure || raw.Say == "browser_session_status" {
			role = RoleSystem
		}
		msg := ParsedMessage{Role: role, Content: content, DialogueText: new(bobIDEUIDialogue(raw, role)), ContentLength: len(content), Timestamp: ts, SourceType: "ui", SourceSubtype: firstNonEmptyJSONLString(raw.Say, raw.Ask), IsSystem: role == RoleSystem, IsCompactBoundary: rooCodeIsCompactBoundary(raw.Say)}
		if raw.Reasoning != "" || raw.Say == "reasoning" {
			msg.ThinkingText = firstNonEmptyJSONLString(raw.Reasoning, content)
			msg.HasThinking = true
			msg.Content = "[Thinking]\n" + msg.ThinkingText + "\n[/Thinking]"
			msg.ContentLength = len(msg.Content)
		}
		*messages = append(*messages, msg)
	}
	return nil
}

func bobIDEUsage(sess *ParsedSession, raw rooCodeMessage, path string, ordinal int) error {
	var usage struct {
		Input      *int            `json:"tokensIn"`
		Output     *int            `json:"tokensOut"`
		CacheRead  *int            `json:"cacheReads"`
		CacheWrite *int            `json:"cacheWrites"`
		Cost       *jsontext.Value `json:"cost"`
	}
	if err := json.Unmarshal([]byte(raw.Text), &usage); err != nil {
		return &bobIDESourceError{Path: path, Kind: "malformed", Err: fmt.Errorf("usage event %d: %w", ordinal, err)}
	}
	if usage.Input == nil && usage.Output == nil && usage.CacheRead == nil && usage.CacheWrite == nil && usage.Cost == nil {
		return nil
	}
	event := ParsedUsageEvent{SessionID: sess.ID, Source: "api_request",
		OccurredAt: time.UnixMilli(raw.Timestamp).Format(time.RFC3339Nano),
		DedupKey:   fmt.Sprintf("bob:ide:%s:request:%d", sess.SourceSessionID, ordinal)}
	for _, field := range []struct {
		value  *int
		target *int
	}{
		{usage.Input, &event.InputTokens}, {usage.Output, &event.OutputTokens},
		{usage.CacheRead, &event.CacheReadInputTokens}, {usage.CacheWrite, &event.CacheCreationInputTokens},
	} {
		if field.value != nil {
			if *field.value < 0 {
				return &bobIDESourceError{Path: path, Kind: "malformed", Err: fmt.Errorf("negative tokens in usage event %d", ordinal)}
			}
			*field.target = *field.value
		}
	}
	if usage.Cost != nil {
		cost, err := money.ParseDollars(string(*usage.Cost))
		if err == nil && cost.Microdollars >= 0 {
			event.Cost = &cost
		}
	}
	if usage.Output != nil {
		sess.TotalOutputTokens += event.OutputTokens
		sess.HasTotalOutputTokens = true
	}
	if usage.Input != nil || usage.CacheRead != nil || usage.CacheWrite != nil {
		sess.PeakContextTokens = max(sess.PeakContextTokens, event.InputTokens+event.CacheReadInputTokens+event.CacheCreationInputTokens)
		sess.HasPeakContextTokens = true
	}
	sess.aggregateTokenPresenceKnown = true
	sess.UsageEvents = append(sess.UsageEvents, event)
	return nil
}
