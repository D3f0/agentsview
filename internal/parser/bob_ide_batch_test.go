package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBobIDEBatchedAPIResultsKeepIndependentStatus(t *testing.T) {
	messages := []ParsedMessage{
		{Role: RoleAssistant, ToolCalls: []ParsedToolCall{{ToolUseID: "one", ToolName: "read_file"}, {ToolUseID: "two", ToolName: "read_file"}}},
		{Role: RoleUser, Content: "[read_file] Result:\nfirst success\n[read_file] Result:\nexit code 1\nsecond failed"},
	}
	bobIDEPairAPIResults(messages)
	require.Len(t, messages[0].ToolCalls[0].ResultEvents, 1)
	require.Len(t, messages[0].ToolCalls[1].ResultEvents, 1)
	assert.Equal(t, "completed", messages[0].ToolCalls[0].ResultEvents[0].Status)
	assert.NotContains(t, messages[0].ToolCalls[0].ResultEvents[0].Content, "second failed")
	assert.Equal(t, "errored", messages[0].ToolCalls[1].ResultEvents[0].Status)
}

func TestBobIDEIgnoresOpaqueOptionalMetadata(t *testing.T) {
	root := t.TempDir()
	writeBobIDETask(t, root, "opaque", `[{"role":"user","ts":1700000000000,"content":[{"type":"text","text":"Task"}]}]`, `[{"type":"say","say":"text","ts":1700000000000,"text":"Task"}]`, `{`)
	_, messages, err := parseBobIDESession(t.Context(), root+"/tasks/opaque", "", "local")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "Task", messages[0].Content)
}
