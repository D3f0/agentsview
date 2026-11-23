package parser

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"

	_ "github.com/mattn/go-sqlite3"
)

const bobDBName = "bob.db"

type bobSessionJSON struct {
	SessionID   string           `json:"sessionId"`
	ProjectHash string           `json:"projectHash"`
	StartTime   string           `json:"startTime"`
	LastUpdated string           `json:"lastUpdated"`
	Messages    []bobMessageJSON `json:"messages"`
}

type bobMessageJSON struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Content   string            `json:"content"`
	Timestamp string            `json:"timestamp"`
	Model     string            `json:"model"`
	Tokens    bobTokensJSON     `json:"tokens"`
	ToolCalls []bobToolCallJSON `json:"toolCalls"`
	Thoughts  []bobThoughtJSON  `json:"thoughts"`
}

type bobTokensJSON struct {
	Input    int  `json:"input"`
	Output   int  `json:"output"`
	Cached   int  `json:"cached"`
	Thoughts int  `json:"thoughts"`
	Tool     int  `json:"tool"`
	Total    int  `json:"total"`
	Present  bool `json:"-"`
}

type bobToolCallJSON struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Args          jsontext.Value  `json:"args"`
	Status        string          `json:"status"`
	Timestamp     string          `json:"timestamp"`
	ResultDisplay string          `json:"resultDisplay"`
	Result        []bobResultJSON `json:"result"`
}

func parseBobSession(ctx context.Context, path, machine string) (*ParsedSession, []ParsedMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
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
	if err := json.UnmarshalRead(ctxio.Reader{Context: ctx, Reader: file}, &raw); err != nil {
		return nil, nil, fmt.Errorf("decode Bob session %s: %w", path, err)
	}
	if len(raw.Messages) == 0 {
		return nil, nil, nil
	}

	messages := make([]ParsedMessage, 0, len(raw.Messages))
	firstMessage := ""
	startedAt := parseTimestamp(raw.StartTime)
	endedAt := parseTimestamp(raw.LastUpdated)
	for _, entry := range raw.Messages {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
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
				Ordinal: len(messages), Role: RoleUser, Content: content,
				SourceUUID:   entry.ID,
				DialogueText: new(content),
				Timestamp:    ts, ContentLength: len(content),
			})
		case "bob-shell":
			content, thinking, dialogue := bobLegacyMessageProjection(entry)
			toolCalls := bobToolCalls(entry.ToolCalls)
			if content == "" && len(toolCalls) == 0 {
				continue
			}
			model := entry.Model
			if model == "" {
				model = "premium"
			}
			message := ParsedMessage{
				Ordinal: len(messages), Role: RoleAssistant, Content: content,
				SourceUUID:   entry.ID,
				ThinkingText: thinking, Timestamp: ts, Model: model,
				DialogueText: new(dialogue),
				HasThinking:  thinking != "", HasToolUse: len(toolCalls) > 0,
				ContentLength: len(content), ToolCalls: toolCalls,
			}
			applyBobTokenUsage(&message, entry.Tokens)
			messages = append(messages, message)
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
	session := &ParsedSession{
		ID: "bob:" + stem, Project: project, Machine: machine, Agent: AgentBob,
		SourceSessionID: raw.SessionID, FirstMessage: firstMessage,
		StartedAt: startedAt, EndedAt: endedAt, MessageCount: len(messages),
		File: FileInfo{Path: path, Size: info.Size(), Mtime: info.ModTime().UnixNano()},
	}
	for _, message := range messages {
		if message.Role == RoleUser {
			session.UserMessageCount++
		}
	}
	accumulateMessageTokenUsage(session, messages)
	return session, messages, nil
}

func bobVisibleContent(content string) (string, string, string) {
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
		content = rest[end+len("</thinking"):]
		content = strings.TrimPrefix(content, ">")
	}
	think := strings.Join(thinking, "\n\n")
	text := strings.TrimSpace(visible.String())
	dialogue := text
	if think != "" {
		block := "[Thinking]\n" + think + "\n[/Thinking]"
		if text != "" {
			text = block + "\n\n" + text
		} else {
			text = block
		}
	}
	return text, think, dialogue
}

func bobToolCalls(raw []bobToolCallJSON) []ParsedToolCall {
	return bobLegacyToolCalls(raw)
}

func applyBobTokenUsage(message *ParsedMessage, tokens bobTokensJSON) {
	if !tokens.Present {
		message.tokenPresenceKnown = true
		return
	}
	raw, err := json.Marshal(struct {
		Input     int `json:"input_tokens"`
		Output    int `json:"output_tokens"`
		Cached    int `json:"cache_read_input_tokens"`
		Reasoning int `json:"reasoning_tokens"`
		Tool      int `json:"tool_tokens"`
		Total     int `json:"total_tokens"`
	}{tokens.Input, tokens.Output, tokens.Cached, tokens.Thoughts, tokens.Tool, tokens.Total})
	if err != nil {
		return
	}
	message.TokenUsage = raw
	message.ContextTokens = tokens.Input + tokens.Cached
	message.OutputTokens = tokens.Output
	message.HasContextTokens = true
	message.HasOutputTokens = true
	message.tokenPresenceKnown = true
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
		strings.HasPrefix(parts[len(parts)-1], "session-") && strings.HasSuffix(parts[len(parts)-1], ".json")
}

// bobDBSourceSet exposes one virtual source per task in Bob's shared database.
type bobDBSourceSet struct{ multiSessionContainerSourceSet }

func newBobDBSourceSet(roots []string) bobDBSourceSet {
	return bobDBSourceSet{multiSessionContainerSourceSet: NewMultiSessionContainerSourceSet(
		AgentBob, roots,
		WithStreamingSourceDiscovery(bobDBDiscoverEach),
		WithWatchRoots(bobDBWatchRoots),
		WithChangedPathClassifier(bobDBClassifyPath),
		WithMemberLookup(bobDBFindMember),
		WithContextFingerprint(bobDBFingerprint),
		WithContextContainerParse(bobDBParseContainer),
		WithContextMemberParse(bobDBParseMember),
		WithMemberPresence(bobDBMemberPresent),
	)}
}

// The upstream collecting entry point uses the legacy callbacks directly.
// Collect through streaming discovery instead so both Bob entry points retain
// the caller's context and database errors.
func (s bobDBSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	return collectDiscoveredSources(ctx, s.DiscoverEach)
}

func bobDBPath(ctx context.Context, root string) string {
	if ctx.Err() != nil {
		return ""
	}
	path := filepath.Join(root, "db", bobDBName)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

func bobDBVirtualPath(dbPath, taskID string) string { return VirtualSourcePath(dbPath, taskID) }
func bobDBParseVirtualPath(path string) (string, string, bool) {
	return ParseVirtualSourcePathForBase(path, bobDBName)
}

func bobDBDiscoverEach(ctx context.Context, root string, yield func(multiSessionMatch) error) error {
	dbPath := bobDBPath(ctx, root)
	if dbPath == "" {
		return ctx.Err()
	}
	conn, err := openBobDB(ctx, dbPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	taskExprs, err := bobDBTaskLocationExpressions(ctx, conn)
	if err != nil {
		return fmt.Errorf("discover Bob tasks: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `SELECT id, project_id, `+taskExprs.directory+`, `+taskExprs.env+`, created_at, updated_at FROM tasks ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("discover Bob tasks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, projectID, directory, env string
		var created, updated int64
		if err := rows.Scan(&id, &projectID, &directory, &env, &created, &updated); err != nil {
			return err
		}
		if id == "" {
			continue
		}
		cwd := bobDBTaskWorkspace(directory, projectID, env)
		project := ExtractProjectFromCwd(cwd)
		if project == "" {
			project = projectID
		}
		if err := yield(multiSessionMatch{
			Path: bobDBVirtualPath(dbPath, id), Container: dbPath, MemberID: id,
			ProjectHint: project, DiscoveryMTimeNS: max(created, updated) * int64(time.Millisecond),
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func bobDBWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path: root, Recursive: false,
			IncludeGlobs: []string{"db/" + bobDBName, "db/" + bobDBName + "-*"},
			DebounceKey:  string(AgentBob) + ":db:" + root,
		})
	}
	return out
}

func bobDBClassifyPath(root, path string, allowMissing bool) (multiSessionMatch, bool) {
	return classifySQLiteContainerPath(root, path, filepath.Join("db", bobDBName), allowMissing, false, bobDBParseVirtualPath)
}

func bobDBFindMember(ctx context.Context, root, taskID string) (multiSessionMatch, bool) {
	dbPath := bobDBPath(ctx, root)
	if dbPath == "" || taskID == "" {
		return multiSessionMatch{}, false
	}
	conn, err := openBobDB(ctx, dbPath)
	if err != nil {
		return multiSessionMatch{}, false
	}
	defer conn.Close()
	var found string
	if err := conn.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id = ?`, taskID).Scan(&found); err != nil {
		return multiSessionMatch{}, false
	}
	return multiSessionMatch{Path: bobDBVirtualPath(dbPath, found), Container: dbPath, MemberID: found}, true
}

func bobDBFingerprint(ctx context.Context, src multiSessionSource) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	info, err := os.Stat(src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	mtime, err := sqliteDBCompositeMtime(src.Container, sqliteDBJournalSuffixes)
	if err != nil {
		mtime = info.ModTime().UnixNano()
	}
	return SourceFingerprint{Size: info.Size(), MTimeNS: mtime, Hash: fmt.Sprintf("%d:%d", info.Size(), mtime)}, nil
}

func openBobDB(ctx context.Context, path string) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+sqliteURIPath(path)+"?mode=ro&_busy_timeout=3000")
	if err != nil {
		return nil, fmt.Errorf("open Bob database %s: %w", path, err)
	}
	return db, nil
}

type bobDBTask struct {
	ID, Project, Directory, Title, Env, Costs string
	CreatedAt, UpdatedAt                      int64
}

func bobDBTasks(ctx context.Context, conn *sql.DB) ([]bobDBTask, error) {
	taskExprs, err := bobDBTaskLocationExpressions(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("list Bob tasks: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `SELECT id, project_id, `+taskExprs.directory+`, title, `+taskExprs.env+`, COALESCE(costs, ''), created_at, updated_at FROM tasks ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list Bob tasks: %w", err)
	}
	defer rows.Close()
	var tasks []bobDBTask
	for rows.Next() {
		var task bobDBTask
		if err := rows.Scan(&task.ID, &task.Project, &task.Directory, &task.Title, &task.Env, &task.Costs, &task.CreatedAt, &task.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan Bob task: %w", err)
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func bobDBParseContainer(ctx context.Context, src multiSessionSource, req ParseRequest) ([]ParseResult, error) {
	conn, err := openBobDB(ctx, src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	tasks, err := bobDBTasks(ctx, conn)
	if err != nil {
		return nil, err
	}
	results := make([]ParseResult, 0, len(tasks))
	for _, task := range tasks {
		result, err := bobDBParseTaskWithDB(ctx, conn, src.Container, task, req.Machine)
		if err != nil {
			return nil, err
		}
		if result != nil {
			results = append(results, *result)
		}
	}
	return results, nil
}

func bobDBParseMember(ctx context.Context, src multiSessionSource, req ParseRequest) (*ParseResult, error) {
	conn, err := openBobDB(ctx, src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var task bobDBTask
	taskExprs, err := bobDBTaskLocationExpressions(ctx, conn)
	if err != nil {
		return nil, err
	}
	err = conn.QueryRowContext(ctx, `SELECT id, project_id, `+taskExprs.directory+`, title, `+taskExprs.env+`, COALESCE(costs, ''), created_at, updated_at FROM tasks WHERE id = ?`, src.MemberID).
		Scan(&task.ID, &task.Project, &task.Directory, &task.Title, &task.Env, &task.Costs, &task.CreatedAt, &task.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return bobDBParseTaskWithDB(ctx, conn, src.Container, task, req.Machine)
}

func bobDBMemberPresent(ctx context.Context, src multiSessionSource) bool {
	_, ok := bobDBFindMember(ctx, src.Root, src.MemberID)
	return ok
}

func bobDBParseTaskWithDB(ctx context.Context, conn *sql.DB, path string, task bobDBTask, machine string) (*ParseResult, error) {
	rows, err := conn.QueryContext(ctx, `SELECT id, role, data, created_at FROM messages WHERE task_id = ? ORDER BY created_at, id`, task.ID)
	if err != nil {
		return nil, fmt.Errorf("list Bob messages: %w", err)
	}
	defer rows.Close()
	var messages []ParsedMessage
	first := ""
	for rows.Next() {
		var id, role, raw string
		var createdAt int64
		if err := rows.Scan(&id, &role, &raw, &createdAt); err != nil {
			return nil, err
		}
		message, ok, err := parseBobDBMessage(role, raw, createdAt, len(messages))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		message.SourceUUID = id
		if first == "" && message.Role == RoleUser && message.Content != "" {
			first = truncate(strings.ReplaceAll(message.Content, "\n", " "), 300)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, nil
	}
	cwd := bobDBTaskWorkspace(task.Directory, task.Project, task.Env)
	project := ExtractProjectFromCwd(cwd)
	if project == "" {
		project = task.Project
	}
	session := ParsedSession{
		ID: "bob:" + task.ID, Project: project, Cwd: cwd, Machine: machine, Agent: AgentBob,
		SourceSessionID: task.ID, FirstMessage: first, SessionName: task.Title,
		StartedAt: time.UnixMilli(task.CreatedAt).UTC(), EndedAt: time.UnixMilli(task.UpdatedAt).UTC(),
		MessageCount: len(messages), File: FileInfo{Path: bobDBVirtualPath(path, task.ID)},
	}
	for _, message := range messages {
		if message.Role == RoleUser {
			session.UserMessageCount++
		}
	}
	if info, err := os.Stat(path); err == nil {
		session.File.Size, session.File.Mtime = info.Size(), info.ModTime().UnixNano()
	}
	accumulateMessageTokenUsage(&session, messages)
	if event := bobDBTaskUsageEvent(task, session.ID, session.EndedAt, messages); event != nil {
		session.UsageEvents = []ParsedUsageEvent{*event}
		for i := range messages {
			messages[i].TokenUsage = nil
		}
	}
	return &ParseResult{Session: session, Messages: messages, UsageEvents: session.UsageEvents}, nil
}

type bobDBTaskLocationColumns struct {
	directory string
	env       string
}

func bobDBTaskLocationExpressions(ctx context.Context, conn *sql.DB) (bobDBTaskLocationColumns, error) {
	hasDirectory, err := openCodeTableHasColumn(ctx, conn, "tasks", "directory")
	if err != nil {
		return bobDBTaskLocationColumns{}, err
	}
	hasEnv, err := openCodeTableHasColumn(ctx, conn, "tasks", "env")
	if err != nil {
		return bobDBTaskLocationColumns{}, err
	}
	expressions := bobDBTaskLocationColumns{directory: `''`, env: `''`}
	if hasDirectory {
		expressions.directory = `COALESCE(directory, '')`
	}
	if hasEnv {
		expressions.env = `COALESCE(env, '')`
	}
	return expressions, nil
}

func bobDBTaskWorkspace(directory, projectID, rawEnv string) string {
	if directory != "" {
		return directory
	}
	var env struct {
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal([]byte(rawEnv), &env) == nil && env.Workspace != "" {
		return env.Workspace
	}
	if strings.HasPrefix(projectID, "file:") {
		return strings.TrimPrefix(projectID, "file:")
	}
	return ""
}

func bobDBTaskUsageEvent(task bobDBTask, sessionID string, occurredAt time.Time, messages []ParsedMessage) *ParsedUsageEvent {
	var costs struct {
		Cost jsontext.Value `json:"cost"`
	}
	if strings.TrimSpace(task.Costs) == "" || json.Unmarshal([]byte(task.Costs), &costs) != nil || len(costs.Cost) == 0 || string(costs.Cost) == "null" {
		return nil
	}
	cost, err := money.ParseDollars(string(costs.Cost))
	if err != nil {
		return nil
	}
	model := ""
	var input, output, cacheRead, cacheWrite, reasoning int
	for _, message := range messages {
		if model == "" && message.Role == RoleAssistant && message.Model != "" {
			model = message.Model
		}
		usage := usagefacts.ParseTokenUsage(string(message.TokenUsage))
		input += int(usage.InputTokens)
		output += int(usage.OutputTokens)
		cacheRead += int(usage.CacheReadTokens)
		cacheWrite += int(usage.CacheCreationTokens)
		reasoning += int(usage.ReasoningTokens)
	}
	if model == "" {
		model = "premium"
	}
	return &ParsedUsageEvent{
		SessionID: sessionID, Source: "bob-task", Model: model,
		InputTokens: input, OutputTokens: output, ReasoningTokens: reasoning,
		CacheReadInputTokens: cacheRead, CacheCreationInputTokens: cacheWrite,
		Cost: &cost, CostStatus: "exact", CostSource: "bob-reported",
		OccurredAt: occurredAt.Format(time.RFC3339Nano), DedupKey: "task:" + sessionID,
	}
}

type bobDBMessagePayload struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Reasoning string           `json:"reasoning"`
	Model     string           `json:"model"`
	ToolCalls []bobDBToolCall  `json:"toolCalls"`
	Meta      bobDBMessageMeta `json:"_meta"`
}
type bobDBMessageMeta struct {
	Timestamp int64      `json:"timestamp"`
	Spend     bobDBSpend `json:"spend"`
}
type bobDBSpend struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Reasoning  int `json:"reasoningTokens"`
	Context    int `json:"contextTokens"`
}
type bobDBToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments jsontext.Value `json:"arguments"`
	Args      jsontext.Value `json:"args"`
}

func parseBobDBMessage(role, raw string, createdAt int64, ordinal int) (ParsedMessage, bool, error) {
	var payload bobDBMessagePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ParsedMessage{}, false, fmt.Errorf("decode Bob message: %w", err)
	}
	if payload.Role != "" {
		role = payload.Role
	}
	if role == "system" {
		return ParsedMessage{}, false, nil
	}
	content := strings.TrimSpace(payload.Content)
	if role == "user" {
		if start := strings.Index(content, "<user_query>"); start >= 0 {
			content = content[start+len("<user_query>"):]
			if end := strings.Index(content, "</user_query>"); end >= 0 {
				content = strings.TrimSpace(content[:end])
			}
		}
	}
	parsedRole := RoleType(role)
	if parsedRole != RoleUser && parsedRole != RoleAssistant && parsedRole != RoleTool {
		return ParsedMessage{}, false, nil
	}
	message := ParsedMessage{
		Ordinal: ordinal, Role: parsedRole, Content: content,
		Timestamp: time.UnixMilli(createdAt).UTC(), Model: payload.Model,
		ContentLength: len(content),
		DialogueText:  new(content),
	}
	if payload.Meta.Timestamp != 0 {
		message.Timestamp = time.UnixMilli(payload.Meta.Timestamp).UTC()
	}
	if parsedRole == RoleAssistant {
		if reasoning := strings.TrimSpace(payload.Reasoning); reasoning != "" {
			message.ThinkingText = reasoning
			message.HasThinking = true
			message.Content = "[Thinking]\n" + reasoning + "\n[/Thinking]\n" + content
			message.ContentLength = len(message.Content)
		}
		message.ToolCalls = bobDBToolCalls(payload.ToolCalls)
		message.HasToolUse = len(message.ToolCalls) > 0
		applyBobDBSpend(&message, payload.Meta.Spend)
	}
	if message.Content == "" && len(message.ToolCalls) == 0 {
		return ParsedMessage{}, false, nil
	}
	return message, true, nil
}

func bobDBToolCalls(raw []bobDBToolCall) []ParsedToolCall {
	calls := make([]ParsedToolCall, 0, len(raw))
	for _, call := range raw {
		if call.Name == "" {
			continue
		}
		input := call.Arguments
		if len(input) == 0 {
			input = call.Args
		}
		calls = append(calls, ParsedToolCall{
			ToolUseID: call.ID, ToolName: call.Name,
			Category: NormalizeToolCategory(call.Name), InputJSON: string(input),
		})
	}
	return calls
}

func applyBobDBSpend(message *ParsedMessage, spend bobDBSpend) {
	if spend.Input == 0 && spend.Output == 0 && spend.CacheRead == 0 && spend.CacheWrite == 0 && spend.Reasoning == 0 {
		message.tokenPresenceKnown = true
		if spend.Context > 0 {
			message.ContextTokens = spend.Context
			message.HasContextTokens = true
		}
		return
	}
	raw, err := json.Marshal(struct {
		Input      int `json:"input_tokens"`
		Output     int `json:"output_tokens"`
		CacheRead  int `json:"cache_read_input_tokens"`
		CacheWrite int `json:"cache_creation_input_tokens"`
		Reasoning  int `json:"reasoning_tokens"`
	}{spend.Input, spend.Output, spend.CacheRead, spend.CacheWrite, spend.Reasoning})
	if err != nil {
		return
	}
	message.TokenUsage = raw
	message.ContextTokens = spend.Input + spend.CacheRead + spend.CacheWrite
	message.OutputTokens = spend.Output
	message.HasContextTokens = true
	message.HasOutputTokens = true
	message.tokenPresenceKnown = true
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
		jsonSources:  newBobSourceSet(cfg.Roots), dbSources: newBobDBSourceSet(cfg.Roots),
		ideSources: newBobIDESourceSet(cfg.Roots),
	}
}

type bobProvider struct {
	ProviderBase
	jsonSources JSONLSourceSet
	dbSources   bobDBSourceSet
	ideSources  singleFileSourceSet
}

func (p *bobProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	jsonSources, err := p.jsonSources.Discover(ctx)
	if err != nil {
		return nil, err
	}
	dbSources, err := p.dbSources.Discover(ctx)
	if err != nil {
		return nil, err
	}
	ideSources, err := p.ideSources.Discover(ctx)
	if err != nil {
		return nil, err
	}
	return append(append(jsonSources, dbSources...), ideSources...), nil
}
func (p *bobProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	if err := p.jsonSources.DiscoverEach(ctx, yield); err != nil {
		return err
	}
	if err := p.dbSources.DiscoverEach(ctx, yield); err != nil {
		return err
	}
	return p.ideSources.DiscoverEach(ctx, yield)
}
func (p *bobProvider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	if source, ok, err := p.dbSources.SourceForReconciliation(ctx, path, project); err != nil || ok {
		return source, ok, err
	}
	if source, ok, err := p.ideSources.FindSource(ctx, FindSourceRequest{StoredFilePath: path}); err != nil || ok {
		source.ProjectHint = project
		return source, ok, err
	}
	return p.jsonSources.SourceForReconciliation(ctx, path, project)
}
func (p *bobProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	jsonPlan, err := p.jsonSources.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	dbPlan, err := p.dbSources.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	idePlan, err := p.ideSources.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	return WatchPlan{Roots: append(append(jsonPlan.Roots, dbPlan.Roots...), idePlan.Roots...)}, nil
}
func (p *bobProvider) SourcesForChangedPath(ctx context.Context, req ChangedPathRequest) ([]SourceRef, error) {
	if sources, err := p.dbSources.SourcesForChangedPath(ctx, req); err != nil || len(sources) > 0 {
		return sources, err
	}
	if sources, err := p.ideSources.SourcesForChangedPath(ctx, req); err != nil || len(sources) > 0 {
		return sources, err
	}
	return p.jsonSources.SourcesForChangedPath(ctx, req)
}
func (p *bobProvider) FindSource(ctx context.Context, req FindSourceRequest) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	if source, ok, err := p.dbSources.FindSource(ctx, req); err != nil || ok {
		return source, ok, err
	}
	if source, ok, err := p.ideSources.FindSource(ctx, req); err != nil || ok {
		return source, ok, err
	}
	return p.jsonSources.FindSource(ctx, req)
}
func (p *bobProvider) Fingerprint(ctx context.Context, source SourceRef) (SourceFingerprint, error) {
	if _, ok := source.Opaque.(multiSessionSource); ok {
		return p.dbSources.Fingerprint(ctx, source)
	}
	if _, ok := source.Opaque.(singleFileSource); ok {
		return p.ideSources.Fingerprint(ctx, source)
	}
	return p.jsonSources.Fingerprint(ctx, source)
}
func (p *bobProvider) Parse(ctx context.Context, req ParseRequest) (ParseOutcome, error) {
	req.Machine = firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	if _, ok := req.Source.Opaque.(multiSessionSource); ok {
		return p.dbSources.Parse(ctx, req)
	}
	if _, ok := req.Source.Opaque.(singleFileSource); ok {
		return p.ideSources.Parse(ctx, req)
	}
	return p.jsonSources.Parse(ctx, req)
}

func newBobSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentBob, roots,
		WithRecursive(), WithExtensions(".json"), WithContentHashing(), WithFollowSymlinkFiles(),
		WithDescendPath(func(root, path string) bool {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return false
			}
			rel = filepath.ToSlash(rel)
			return rel == "." || rel == "tmp" || strings.HasPrefix(rel, "tmp/")
		}),
		WithIncludePath(isBobSourcePath), WithSessionIDFromPath(bobSessionIDFromPath),
		WithParseFile(func(ctx context.Context, path string, req ParseRequest) ([]ParseResult, []string, error) {
			session, messages, err := parseBobSession(ctx, path, req.Machine)
			if err != nil || session == nil {
				return nil, nil, err
			}
			if req.Fingerprint.Hash != "" {
				session.File.Hash = req.Fingerprint.Hash
			}
			return []ParseResult{{Session: *session, Messages: messages}}, nil, nil
		}),
	)
}

func bobProviderCapabilities() Capabilities {
	return Capabilities{
		Source: func() SourceCapabilities {
			caps := jsonlFileProviderSourceCapabilities()
			caps.StreamingDiscovery = CapabilitySupported
			caps.MultiSessionSource = CapabilitySupported
			caps.SharedContainerSource = CapabilitySupported
			caps.ForceReplaceOnParse = CapabilitySupported
			return caps
		}(),
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported, SessionName: CapabilitySupported,
			Model: CapabilitySupported, Thinking: CapabilitySupported,
			ToolCalls: CapabilitySupported, PerMessageTokenUsage: CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
		},
	}
}

var _ Provider = (*bobProvider)(nil)
var _ ProviderFactory = bobProviderFactory{}
