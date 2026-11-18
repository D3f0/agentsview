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

func TestBobVersionUpgradeReparsesUnchangedTask(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "db", "bob.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	producer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = producer.Exec(`
		CREATE TABLE tasks (id TEXT PRIMARY KEY, project_id TEXT, directory TEXT,
			title TEXT, env TEXT, costs TEXT, created_at INTEGER, updated_at INTEGER);
		CREATE TABLE messages (id TEXT PRIMARY KEY, task_id TEXT, role TEXT,
			data TEXT, created_at INTEGER);
		INSERT INTO tasks VALUES ('upgrade-task','demo','/workspace/demo','Review',
			'{}','{"cost":0.25,"contextTokens":1234}',1794262558000,1794262560000);
		INSERT INTO messages VALUES ('user','upgrade-task','user',
			'{"role":"user","content":"Explain the parser."}',1794262558000);
		INSERT INTO messages VALUES ('assistant','upgrade-task','assistant',
			'{"role":"assistant","content":"It reads tasks.","reasoning":"Inspect the schema.","_meta":{"spend":{"cost":0.25,"contextTokens":1234}}}',1794262560000);
	`)
	require.NoError(t, err)
	require.NoError(t, producer.Close())
	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.OpenIsolated(t.Context(), archivePath)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentBob: {root}},
		Machine:   "local", DisableFilesystemProjectDiscovery: true,
	})
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	const sessionID = "bob:upgrade-task"
	before, err := database.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE messages SET thinking_text = '',
			has_thinking = 0, context_tokens = 0, has_context_tokens = 0,
			dialogue_text = NULL WHERE session_id = ?`, sessionID)
		return err
	}))
	require.NoError(t, database.SetSessionDataVersion(t.Context(), sessionID, 167))
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "PRAGMA user_version = 167")
		return err
	}))
	require.NoError(t, database.Close())
	reopened, err := db.OpenIsolated(t.Context(), archivePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	engine = NewEngine(t.Context(), reopened, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentBob: {root}},
		Machine:   "local", DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	messages, err := reopened.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "Inspect the schema.", messages[1].ThinkingText)
	assert.Equal(t, 1234, messages[1].ContextTokens)
	after, err := reopened.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, before.FilePath, after.FilePath)
	assert.Equal(t, db.CurrentDataVersion(), after.DataVersion)
}
