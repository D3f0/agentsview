package parser

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBobSQLiteProducerOrderAndToolResults(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "db", bobDBName)
	require.NoError(t, writeBobSQLiteFixture(path))
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`DELETE FROM messages;
 INSERT INTO messages VALUES ('a','task-1','assistant','{"role":"assistant","content":"Running","_meta":{"timestamp":2000},"toolCalls":[{"id":"call-1","name":"execute_command","arguments":{"command":"false"}}]}',5000);
 INSERT INTO messages VALUES ('z','task-1','user','{"role":"user","content":"Run the command","_meta":{"timestamp":1000}}',5000);
 INSERT INTO messages VALUES ('b','task-1','tool','{"role":"tool","content":"Command failed","toolUsage":{"signature":{"id":"call-1","name":"execute_command","isError":true}},"_meta":{"timestamp":3000,"exitCode":1}}',5000);`)
	require.NoError(t, err)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	r := out.Results[0].Result
	require.Len(t, r.Messages, 3)
	assert.Equal(t, "Run the command", r.Messages[0].Content)
	assert.Equal(t, "Running", r.Messages[1].Content)
	assert.Equal(t, "Command failed", r.Messages[2].Content)
	assert.Equal(t, 2, r.Messages[2].Ordinal)
	require.Len(t, r.Messages[2].ToolResults, 1)
	assert.Equal(t, "call-1", r.Messages[2].ToolResults[0].ToolUseID)
	assert.Equal(t, "errored", r.Messages[2].ToolResults[0].Status)
	assert.Equal(t, `"Command failed"`, r.Messages[2].ToolResults[0].ContentRaw)
	assert.True(t, r.Messages[2].IsSystem)
	assert.Equal(t, SourceSubtypeToolResult, r.Messages[2].SourceSubtype)
}

func TestBobIDEToolCarriersDoNotCountAsHumanPrompts(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "tasks", "carrier", bobIDEHistory), `[
 {"role":"user","ts":1000,"content":[{"type":"text","text":"[read_file for 'main.go'] Result:\npackage main\n<environment_details>generated context</environment_details>"}]},
 {"role":"user","ts":2000,"content":[{"type":"text","text":"<task>Fix the code</task>\n<environment_details>generated context</environment_details>"}]},
 {"role":"assistant","ts":3000,"content":[{"type":"text","text":"Fixed"}]}
 ]`)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	r := out.Results[0].Result
	assert.Equal(t, 1, r.Session.UserMessageCount)
	assert.Equal(t, "Fix the code", r.Session.FirstMessage)
	assert.True(t, r.Messages[0].IsSystem)
	assert.Equal(t, RoleTool, r.Messages[0].Role)
	assert.NotContains(t, r.Messages[1].Content, "environment_details")
}
