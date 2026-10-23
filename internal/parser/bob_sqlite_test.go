package parser

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/mattn/go-sqlite3"
)

func TestBobProviderDiscoversAndParsesSQLiteTasks(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "db", bobDBName)
	require.NoError(t, writeBobSQLiteFixture(dbPath))

	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, bobDBVirtualPath(dbPath, "task-1"), sources[0].DisplayPath)
	assert.Equal(t, "project", sources[0].ProjectHint)

	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.NotEmpty(t, fingerprint.Hash)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0], Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0].Result
	assert.Equal(t, "bob:task-1", result.Session.ID)
	assert.Equal(t, "project", result.Session.Project)
	assert.Equal(t, "/workspace/project", result.Session.Cwd)
	assert.Equal(t, "How are the UI components built?", result.Session.FirstMessage)
	assert.Equal(t, "UI architecture", result.Session.SessionName)
	assert.Equal(t, 2, result.Session.MessageCount)
	assert.Equal(t, 1, result.Session.UserMessageCount)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
	assert.Equal(t, "premium", result.Messages[1].Model)
	assert.Equal(t, 120, result.Messages[1].ContextTokens)
	assert.Equal(t, 40, result.Messages[1].OutputTokens)
	assert.Equal(t, fingerprint.Hash, result.Session.File.Hash)
	require.Len(t, result.Session.UsageEvents, 1)
	require.NotNil(t, result.Session.UsageEvents[0].Cost)
	assert.Equal(t, int64(1_364_024), result.Session.UsageEvents[0].Cost.Microdollars)
	assert.Equal(t, "exact", result.Session.UsageEvents[0].CostStatus)
	assert.Equal(t, "bob-reported", result.Session.UsageEvents[0].CostSource)
}

func TestBobProviderKeepsLegacyTaskDirectorySupport(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "db", bobDBName)
	require.NoError(t, writeBobLegacySQLiteFixture(dbPath))
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "/legacy/workspace", outcome.Results[0].Result.Session.Cwd)
}

func TestBobProviderClassifiesSQLiteSidecarChange(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "db", bobDBName)
	require.NoError(t, writeBobSQLiteFixture(dbPath))
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	writer.SetMaxOpenConns(1)
	defer writer.Close()
	var journalMode string
	require.NoError(t, writer.QueryRowContext(
		t.Context(), "PRAGMA journal_mode=WAL",
	).Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)
	_, err = writer.ExecContext(t.Context(),
		"UPDATE tasks SET updated_at = updated_at + 1 WHERE id = ?", "task-1")
	require.NoError(t, err)
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: dbPath + "-wal", EventKind: "write", WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, dbPath, changed[0].DisplayPath)
}

func TestBobProviderResolvesSQLiteTaskWorkspace(t *testing.T) {
	tests := []struct {
		name, directory, projectID, rawEnv, want string
	}{
		{"directory wins", "/dir", "file:/project", `{"workspace":"/env"}`, "/dir"},
		{"env fallback", "", "file:/project", `{"workspace":"/env"}`, "/env"},
		{"file project fallback", "", "file:/project", "{}", "/project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dbPath := filepath.Join(root, "db", bobDBName)
			require.NoError(t, writeBobSQLiteFixture(dbPath))
			conn, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `
				UPDATE tasks SET directory = ?, project_id = ?, env = ?
				WHERE id = ?`, tt.directory, tt.projectID, tt.rawEnv, "task-1")
			require.NoError(t, err)
			require.NoError(t, conn.Close())

			provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Equal(t, tt.want, outcome.Results[0].Result.Session.Cwd)
		})
	}
}

func writeBobSQLiteFixture(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE tasks (
			id TEXT PRIMARY KEY, project_id TEXT, directory TEXT, title TEXT,
			env TEXT, costs TEXT, created_at INTEGER, updated_at INTEGER
		);
		CREATE TABLE messages (
			id TEXT PRIMARY KEY, task_id TEXT, role TEXT, data TEXT, created_at INTEGER
		);
	`); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO tasks
		(id, project_id, directory, title, env, costs, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"task-1", "file:/workspace/project", "", "UI architecture",
		`{"workspace":"/workspace/project"}`,
		`{"input":100,"output":40,"cacheRead":20,"cost":1.364024}`,
		1785767435000, 1785767513000); err != nil {
		return err
	}
	userData, err := json.Marshal(map[string]any{
		"role":    "user",
		"content": "<user_query>How are the UI components built?</user_query>",
	})
	if err != nil {
		return err
	}
	assistantData, err := json.Marshal(map[string]any{
		"role": "assistant", "content": "They use React components.", "model": "premium",
		"_meta": map[string]any{
			"timestamp": 1785767512923,
			"spend":     map[string]int{"input": 100, "output": 40, "cacheRead": 20},
		},
	})
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO messages (id, task_id, role, data, created_at) VALUES
		(?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
		"m-1", "task-1", "user", string(userData), 1785767512922,
		"m-2", "task-1", "assistant", string(assistantData), 1785767512923)
	return err
}

func writeBobLegacySQLiteFixture(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE tasks (
			id TEXT PRIMARY KEY, project_id TEXT, directory TEXT, title TEXT,
			costs TEXT, created_at INTEGER, updated_at INTEGER
		);
		CREATE TABLE messages (
			id TEXT PRIMARY KEY, task_id TEXT, role TEXT, data TEXT, created_at INTEGER
		);
		INSERT INTO tasks VALUES
			('legacy-task', 'legacy', '/legacy/workspace', 'Legacy task', '', 1, 2);
		INSERT INTO messages VALUES
			('legacy-message', 'legacy-task', 'user', '{"content":"Legacy prompt"}', 1);
	`); err != nil {
		return err
	}
	return nil
}
