package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBobIDEUIResultsPairBeforeAPICompletion(t *testing.T) {
	root := t.TempDir()
	writeBobIDETask(t, root, "delayed", `[{"role":"user","ts":1700000000000,"content":[{"type":"text","text":"Run command"}]},{"role":"assistant","ts":1700000001000,"content":[{"type":"text","text":"<execute_command><command>first</command></execute_command><execute_command><command>second</command></execute_command>"}]}]`, `[{"type":"say","say":"text","ts":1700000000000,"text":"Run command"},{"type":"ask","ask":"command","ts":1700000000100,"text":"first"},{"type":"say","say":"command_output","ts":1700000000200,"text":"first result"},{"type":"ask","ask":"command","ts":1700000000300,"text":"second"},{"type":"say","say":"command_output","ts":1700000000400,"text":"Process exited with code 1\nsecond failed"}]`, "")
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	refs, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, refs, 1)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: refs[0]})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	var calls []ParsedToolCall
	for _, m := range out.Results[0].Result.Messages {
		calls = append(calls, m.ToolCalls...)
	}
	require.Len(t, calls, 2)
	require.Len(t, calls[0].ResultEvents, 1)
	assert.Equal(t, "first result", calls[0].ResultEvents[0].Content)
	require.Len(t, calls[1].ResultEvents, 1)
	assert.Contains(t, calls[1].ResultEvents[0].Content, "second failed")
	assert.Equal(t, "errored", calls[1].ResultEvents[0].Status)
}

func TestBobIDEFailureClosesPendingCall(t *testing.T) {
	root := t.TempDir()
	writeBobIDETask(t, root, "failure", `[{"role":"user","ts":1700000000000,"content":[{"type":"text","text":"Run command"}]},{"role":"assistant","ts":1700000001000,"content":[{"type":"text","text":"<execute_command><command>first</command></execute_command>"}]}]`, `[{"type":"say","say":"text","ts":1700000000000,"text":"Run command"},{"type":"ask","ask":"command","ts":1700000000100,"text":"first"},{"type":"ask","ask":"api_req_failed","ts":1700000000200,"text":"request failed"}]`, "")
	_, msgs, err := parseBobIDESession(t.Context(), root+"/tasks/failure", "", "local")
	require.NoError(t, err)
	var calls []ParsedToolCall
	for _, m := range msgs {
		calls = append(calls, m.ToolCalls...)
	}
	require.Len(t, calls, 1)
	require.Len(t, calls[0].ResultEvents, 1)
	assert.Equal(t, "errored", calls[0].ResultEvents[0].Status)
}

func TestBobIDEUnrelatedFailureDoesNotPoisonCompletedTool(t *testing.T) {
	root := t.TempDir()
	writeBobIDETask(t, root, "finished", `[{"role":"user","ts":1700000000000,"content":[{"type":"text","text":"Run command"}]},{"role":"assistant","ts":1700000001000,"content":[{"type":"text","text":"<execute_command><command>first</command></execute_command>"}]}]`, `[{"type":"say","say":"text","ts":1700000000000,"text":"Run command"},{"type":"ask","ask":"command","ts":1700000000100,"text":"first"},{"type":"say","say":"command_output","ts":1700000000200,"text":"success"},{"type":"say","say":"text","ts":1700000000300,"text":"New unrelated conversation"},{"type":"ask","ask":"api_req_failed","ts":1700000000400,"text":"new request failed"}]`, "")
	_, msgs, err := parseBobIDESession(t.Context(), root+"/tasks/finished", "", "local")
	require.NoError(t, err)
	var calls []ParsedToolCall
	var failureFound bool
	for _, m := range msgs {
		calls = append(calls, m.ToolCalls...)
		if m.Role == RoleSystem && m.Content == "new request failed" {
			failureFound = true
		}
	}
	require.Len(t, calls, 1)
	require.Len(t, calls[0].ResultEvents, 1)
	assert.Equal(t, "completed", calls[0].ResultEvents[0].Status)
	assert.True(t, failureFound)
}

func TestBobIDEUIFindsUnusedToolInLaterAPITurn(t *testing.T) {
	root := t.TempDir()
	writeBobIDETask(t, root, "turns", `[{"role":"user","ts":1700000000000,"content":[{"type":"text","text":"Task"}]},{"role":"assistant","ts":1700000001000,"content":[{"type":"text","text":"<read_file><args><file><path>x</path></file></args></read_file>"}]},{"role":"assistant","ts":1700000002000,"content":[{"type":"text","text":"<execute_command><command>run</command></execute_command>"}]}]`, `[{"type":"say","say":"text","ts":1700000000000,"text":"Task"},{"type":"ask","ask":"tool","ts":1700000000100,"text":"{\"tool\":\"readFile\",\"path\":\"x\",\"content\":\"read result\"}"},{"type":"ask","ask":"command","ts":1700000000300,"text":"run"},{"type":"say","say":"command_output","ts":1700000000400,"text":"run result"}]`, "")
	_, msgs, err := parseBobIDESession(t.Context(), root+"/tasks/turns", "", "local")
	require.NoError(t, err)
	var calls []ParsedToolCall
	for _, m := range msgs {
		calls = append(calls, m.ToolCalls...)
	}
	require.Len(t, calls, 2)
	require.Len(t, calls[1].ResultEvents, 1)
	assert.Equal(t, "run result", calls[1].ResultEvents[0].Content)
}

func TestBobIDEUICompletesDelegationAndMCPResource(t *testing.T) {
	root := t.TempDir()
	writeBobIDETask(t, root, "special", `[{"role":"user","ts":1700000000000,"content":[{"type":"text","text":"Task"}]},{"role":"assistant","ts":1700000001000,"content":[{"type":"text","text":"<new_task><message>Delegate</message></new_task><access_mcp_resource><uri>resource://fixture</uri></access_mcp_resource>"}]}]`, `[{"type":"say","say":"text","ts":1700000000000,"text":"Task"},{"type":"ask","ask":"tool","ts":1700000000100,"text":"{\"tool\":\"newTask\",\"message\":\"Delegate\"}"},{"type":"say","say":"subtask_result","ts":1700000000200,"text":"delegated result"},{"type":"ask","ask":"use_mcp_server","ts":1700000000300,"text":"{\"type\":\"access_mcp_resource\",\"serverName\":\"fixture\",\"uri\":\"resource://fixture\"}"},{"type":"say","say":"mcp_server_response","ts":1700000000400,"text":"resource result"}]`, "")
	_, msgs, err := parseBobIDESession(t.Context(), root+"/tasks/special", "", "local")
	require.NoError(t, err)
	var calls []ParsedToolCall
	for _, m := range msgs {
		calls = append(calls, m.ToolCalls...)
	}
	require.Len(t, calls, 2)
	require.Len(t, calls[0].ResultEvents, 1)
	require.Len(t, calls[1].ResultEvents, 1)
	assert.Equal(t, "delegated result", calls[0].ResultEvents[0].Content)
	assert.Equal(t, "resource result", calls[1].ResultEvents[0].Content)
}
