package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// Removing the 169 Bob scope must leave the unchanged CLI snapshots with their
// version-168 reasoning/result loss, rather than silently stamping them current.
func TestBobRelease169UpgradeRepairsUnchangedSnapshotsInPlace(t *testing.T) {
	const (
		previousVersion = 168
		sourceID        = "producer-task"
		nativeID        = "bob:producer-task"
		otherID         = "gptme:unaffected"
		orphanBobID     = "bob:missing-snapshot"
		orphanOtherID   = "gptme:missing-source"
	)
	bobRoot := t.TempDir()
	cliBody := []byte(`{"sessionId":"producer-task","projectHash":"demo","startTime":"2026-01-01T00:00:00Z","lastUpdated":"2026-01-01T00:00:03Z","messages":[
 {"id":"user-1","type":"user","content":"Read the file","timestamp":"2026-01-01T00:00:01Z"},
 {"id":"assistant-1","type":"bob-shell","content":"**Inspect file**\nRead it.</thinking>","timestamp":"2026-01-01T00:00:03Z","model":"model-a","thoughts":[{"subject":"","description":"Answer","timestamp":"2026-01-01T00:00:01Z"},{"subject":"Inspect file","description":"Read","timestamp":"2026-01-01T00:00:02Z"},{"subject":"Inspect file","description":"Read it.","timestamp":"2026-01-01T00:00:02.500Z"}],"tokens":{"input":100,"output":20,"cached":30,"thoughts":5},"toolCalls":[{"id":"call-1","name":"read_file","args":{"file_path":"fixture.txt"},"status":"success","timestamp":"2026-01-01T00:00:03Z","result":[{"functionResponse":{"id":"call-1","name":"read_file","response":{"output":"retained output"}}}]}]}
]}`)
	cliIDs := []string{"bob:session-release-snapshot-a", "bob:session-release-snapshot-b"}
	cliPaths := []string{
		filepath.Join(bobRoot, "tmp", "demo", "chats", "session-release-snapshot-a.json"),
		filepath.Join(bobRoot, "tmp", "demo", "chats", "session-release-snapshot-b.json"),
	}
	for _, path := range cliPaths {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, cliBody, 0o600))
	}
	infoBefore, err := os.Stat(cliPaths[0])
	require.NoError(t, err)

	// A native SQLite task sharing the CLI source identity remains a separate
	// session. The release's Bob 2.x reasoning support must survive this repair.
	producerPath := filepath.Join(bobRoot, "db", "bob.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(producerPath), 0o700))
	producer, err := sql.Open("sqlite3", producerPath)
	require.NoError(t, err)
	_, err = producer.ExecContext(t.Context(), `
		CREATE TABLE tasks (id TEXT PRIMARY KEY, project_id TEXT, directory TEXT,
			title TEXT, env TEXT, costs TEXT, created_at INTEGER, updated_at INTEGER);
		CREATE TABLE messages (id TEXT PRIMARY KEY, task_id TEXT, role TEXT,
			data TEXT, created_at INTEGER);
		INSERT INTO tasks VALUES ('producer-task','demo','/workspace/demo','Native task',
			'{}','',1767225600000,1767225603000);
		INSERT INTO messages VALUES ('user','producer-task','user',
			'{"role":"user","content":"Native question"}',1767225601000);
		INSERT INTO messages VALUES ('assistant','producer-task','assistant',
			'{"role":"assistant","content":"Native answer","reasoning":"Native reasoning"}',1767225603000);
	`)
	require.NoError(t, err)
	require.NoError(t, producer.Close())

	otherRoot := t.TempDir()
	otherPath := filepath.Join(otherRoot, "unaffected", "conversation.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(otherPath), 0o700))
	require.NoError(t, os.WriteFile(otherPath, []byte(`{"role":"user","content":"Unchanged question","timestamp":"2026-01-01T00:00:01"}
{"role":"assistant","content":"Unchanged answer","timestamp":"2026-01-01T00:00:03"}
`), 0o600))
	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	cfg := EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentBob:   {bobRoot},
			parser.AgentGptme: {otherRoot},
		},
		Machine: "local", DisableFilesystemProjectDiscovery: true,
	}
	engine := NewEngine(t.Context(), database, cfg)
	require.Equal(t, 4, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()

	// Write the output the actual 168 CLI parser produced through normal archive
	// APIs; keep its source fingerprints and paths from the initial real sync.
	before := make(map[string]*db.Session)
	for _, id := range cliIDs {
		stored, err := database.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, stored)
		require.NotNil(t, stored.FileHash)
		require.NotEmpty(t, *stored.FileHash)
		before[id] = stored
		oldMessages := []db.Message{
			{SessionID: id, Ordinal: 0, Role: "user", Content: "Read the file", DialogueText: new("Read the file"), Timestamp: "2026-01-01T00:00:01Z", ContentLength: 13},
			{SessionID: id, Ordinal: 1, Role: "assistant", Content: "**Inspect file**\nRead it.</thinking>", DialogueText: new("**Inspect file**\nRead it.</thinking>"), Timestamp: "2026-01-01T00:00:03Z", ContentLength: len("**Inspect file**\nRead it.</thinking>"), Model: "model-a", HasToolUse: true, ContextTokens: 130, OutputTokens: 20, HasContextTokens: true, HasOutputTokens: true,
				ToolCalls: []db.ToolCall{{ToolName: "read_file", Category: "Read", ToolUseID: "call-1", InputJSON: `{"file_path":"fixture.txt"}`}}},
		}
		require.NoError(t, database.ReplaceSessionMessages(t.Context(), id, oldMessages))
		messages, err := database.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		require.Len(t, messages, 2)
		pinID, err := database.PinMessage(t.Context(), id, messages[0].ID, new("Keep this question"))
		require.NoError(t, err)
		require.NotZero(t, pinID)
		require.NoError(t, database.RenameSession(t.Context(), id, new("Saved snapshot")))
		_, err = database.StarSession(t.Context(), id)
		require.NoError(t, err)
		_, err = database.SetSessionLabels(t.Context(), id, []string{"retained"})
		require.NoError(t, err)
	}
	orphanPaths := make(map[string]string)
	orphanMessages := make(map[string][]db.Message)
	for _, orphan := range []struct{ id, agent, text string }{
		{orphanBobID, "bob", "Archived Bob history"},
		{orphanOtherID, "gptme", "Archived other history"},
	} {
		missingPath := filepath.Join(t.TempDir(), "missing.json")
		orphanPaths[orphan.id] = missingPath
		require.NoError(t, database.UpsertSession(t.Context(), db.Session{
			ID: orphan.id, Agent: orphan.agent, Project: "demo", Machine: "local",
			FilePath: new(missingPath), MessageCount: 1, UserMessageCount: 1,
		}))
		require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{
			SessionID: orphan.id, Ordinal: 0, Role: "user", Content: orphan.text,
			DialogueText: new(orphan.text), ContentLength: len(orphan.text),
		}}))
		messages, err := database.GetAllMessages(t.Context(), orphan.id)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		orphanMessages[orphan.id] = messages
		require.NoError(t, database.RenameSession(t.Context(), orphan.id, new("Saved orphan")))
		_, err = database.StarSession(t.Context(), orphan.id)
		require.NoError(t, err)
	}
	otherBefore, err := database.GetAllMessages(t.Context(), otherID)
	require.NoError(t, err)
	nativeBefore, err := database.GetSessionFull(t.Context(), nativeID)
	require.NoError(t, err)
	require.NotNil(t, nativeBefore)
	for _, id := range append(append([]string{}, cliIDs...), nativeID, otherID, orphanBobID, orphanOtherID) {
		require.NoError(t, database.SetSessionDataVersion(t.Context(), id, previousVersion))
	}
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "PRAGMA user_version = 168")
		return err
	}))
	require.NoError(t, database.Close())

	// Establish an on-disk 168 archive, not a metadata-only scope-map assertion.
	oldArchive, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(archivePath)+"?mode=ro")
	require.NoError(t, err)
	var onDiskVersion int
	require.NoError(t, oldArchive.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&onDiskVersion))
	require.Equal(t, previousVersion, onDiskVersion)
	require.NoError(t, oldArchive.Close())
	assert.Equal(t, []parser.AgentType{parser.AgentBob}, db.ScopedDataVersionProviders(168, 169))
	reopened, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	assert.False(t, reopened.NeedsResync(), "this upgrade must not request a full rebuild")
	stale, err := reopened.GetAllMessages(t.Context(), cliIDs[0])
	require.NoError(t, err)
	require.Len(t, stale, 2)
	assert.Empty(t, stale[1].ThinkingText)
	require.Len(t, stale[1].ToolCalls, 1)
	assert.Empty(t, stale[1].ToolCalls[0].ResultEvents)
	upgraded := NewEngine(t.Context(), reopened, cfg)
	defer upgraded.Close()
	stats := upgraded.SyncAll(t.Context(), nil)
	assert.Equal(t, 3, stats.Synced, "only two CLI snapshots and the native Bob task need reparsing")
	assert.Zero(t, stats.Failed)

	for i, id := range cliIDs {
		after, err := reopened.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, after)
		assert.Equal(t, id, after.ID)
		assert.Equal(t, sourceID, after.SourceSessionID, "source task identity must not replace the persisted snapshot ID")
		assert.Equal(t, new(cliPaths[i]), after.FilePath)
		assert.Equal(t, before[id].FileHash, after.FileHash)
		assert.Equal(t, before[id].FileSize, after.FileSize)
		assert.Equal(t, before[id].FileMtime, after.FileMtime)
		assert.Equal(t, new("Saved snapshot"), after.DisplayName)
		assert.Equal(t, []string{"retained"}, after.Labels)
		assert.Equal(t, 169, after.DataVersion)
		messages, err := reopened.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		require.Len(t, messages, 2)
		assert.Equal(t, "user", messages[0].Role)
		assert.Equal(t, "Read the file", messages[0].Content)
		assert.Equal(t, "user-1", messages[0].SourceUUID)
		assert.Equal(t, "assistant", messages[1].Role)
		assert.Equal(t, "assistant-1", messages[1].SourceUUID)
		assert.Equal(t, "[Thinking]\nInspect file\nRead it.\n[/Thinking]\n\nAnswer", messages[1].Content)
		assert.Equal(t, "Inspect file\nRead it.", messages[1].ThinkingText)
		assert.True(t, messages[1].HasThinking)
		assert.Equal(t, new("Answer"), messages[1].DialogueText)
		assert.Equal(t, "2026-01-01T00:00:03Z", messages[1].Timestamp)
		assert.Equal(t, 130, messages[1].ContextTokens)
		assert.Equal(t, 20, messages[1].OutputTokens)
		require.Len(t, messages[1].ToolCalls, 1)
		call := messages[1].ToolCalls[0]
		assert.Equal(t, "read_file", call.ToolName)
		assert.Equal(t, "Read", call.Category)
		assert.Equal(t, "call-1", call.ToolUseID)
		assert.JSONEq(t, `{"file_path":"fixture.txt"}`, call.InputJSON)
		require.Len(t, call.ResultEvents, 1)
		assert.Equal(t, "retained output", call.ResultEvents[0].Content)
		assert.Equal(t, "call-1", call.ResultEvents[0].ToolUseID)
		assert.Equal(t, "2026-01-01T00:00:03Z", call.ResultEvents[0].Timestamp)
		pins, err := reopened.ListPinnedMessages(t.Context(), id, "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		assert.Equal(t, messages[0].ID, pins[0].MessageID)
		assert.Equal(t, new("Keep this question"), pins[0].Note)
	}
	nativeAfter, err := reopened.GetSessionFull(t.Context(), nativeID)
	require.NoError(t, err)
	require.NotNil(t, nativeAfter)
	assert.Equal(t, nativeBefore.ID, nativeAfter.ID)
	assert.Equal(t, nativeBefore.FilePath, nativeAfter.FilePath)
	assert.Equal(t, sourceID, nativeAfter.SourceSessionID)
	nativeMessages, err := reopened.GetAllMessages(t.Context(), nativeID)
	require.NoError(t, err)
	require.Len(t, nativeMessages, 2)
	assert.Equal(t, "[Thinking]\nNative reasoning\n[/Thinking]\nNative answer", nativeMessages[1].Content)
	assert.Equal(t, "Native reasoning", nativeMessages[1].ThinkingText)
	assert.Equal(t, new("Native answer"), nativeMessages[1].DialogueText)
	otherAfter, err := reopened.GetAllMessages(t.Context(), otherID)
	require.NoError(t, err)
	assert.Equal(t, otherBefore, otherAfter, "an unaffected source must not be rewritten")
	assert.Equal(t, 169, reopened.GetSessionDataVersion(t.Context(), otherID))
	for _, orphan := range []struct {
		id, text string
		version  int
	}{
		{orphanBobID, "Archived Bob history", 168},
		{orphanOtherID, "Archived other history", 169},
	} {
		session, err := reopened.GetSessionFull(t.Context(), orphan.id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, orphan.id, session.ID)
		assert.Equal(t, orphan.version, session.DataVersion)
		assert.Equal(t, new("Saved orphan"), session.DisplayName)
		assert.Equal(t, new(orphanPaths[orphan.id]), session.FilePath)
		messages, err := reopened.GetAllMessages(t.Context(), orphan.id)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		assert.Equal(t, orphan.text, messages[0].Content)
		assert.Equal(t, orphanMessages[orphan.id], messages, "missing-source history must stay in its original rows")
	}
	starred, err := reopened.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.ElementsMatch(t, append(append([]string{}, cliIDs...), orphanBobID, orphanOtherID), starred)
	page, err := reopened.ListSessions(t.Context(), db.SessionFilter{Limit: 20})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 6, "snapshot/source collisions must not lose or duplicate sessions")
	infoAfter, err := os.Stat(cliPaths[0])
	require.NoError(t, err)
	assert.Equal(t, infoBefore.ModTime(), infoAfter.ModTime())
	for _, path := range cliPaths {
		unchanged, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, cliBody, unchanged)
	}
}
