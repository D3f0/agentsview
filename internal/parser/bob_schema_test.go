package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBobSQLiteWithoutDirectoryColumnUsesEnv(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "db", bobDBName)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, writer.Close()) })
	_, err = writer.ExecContext(t.Context(), `
 CREATE TABLE tasks (id TEXT, project_id TEXT, title TEXT, env TEXT, costs TEXT, created_at INTEGER, updated_at INTEGER);
 CREATE TABLE messages (id TEXT, task_id TEXT, role TEXT, data TEXT, created_at INTEGER);
 INSERT INTO tasks VALUES ('env-task', 'file:/fallback', 'Task', '{"workspace":"/workspace/example"}', '', 1000, 2000);
 INSERT INTO messages VALUES ('msg', 'env-task', 'user', '{"content":"Hello"}', 1000);
 `)
	require.NoError(t, err)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, "example", sources[0].ProjectHint)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	assert.Equal(t, "/workspace/example", out.Results[0].Result.Session.Cwd)
	assert.Equal(t, "example", out.Results[0].Result.Session.Project)
}

func TestBobIDEExplicitZeroCostAndEmptyAPIHistory(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "tasks", "zero", bobIDEHistory), `[]`)
	writeSourceFile(t, filepath.Join(root, "tasks", "zero", "ui_messages.json"), `[
 {"ts":1000,"type":"say","say":"text","text":"Hello"},
 {"ts":2000,"type":"say","say":"api_req_started","text":"{\"tokensIn\":40,\"tokensOut\":5,\"cost\":0}"},
 {"ts":3000,"type":"say","say":"text","text":"World"}
 ]`)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	r := out.Results[0].Result
	require.Len(t, r.Messages, 2)
	assert.Equal(t, "Hello", r.Messages[0].Content)
	assert.Equal(t, "World", r.Messages[1].Content)
	require.Len(t, r.UsageEvents, 1)
	require.NotNil(t, r.UsageEvents[0].Cost)
	assert.Zero(t, r.UsageEvents[0].Cost.Microdollars)
	assert.Equal(t, "exact", r.UsageEvents[0].CostStatus)
}
