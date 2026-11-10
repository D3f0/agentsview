package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBobLegacyStructuredThoughtStreamsAndResults(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tmp", "project", "chats", "session-stream.json")
	writeSourceFile(t, path, `{"sessionId":"shared-id","projectHash":"project","startTime":"2026-01-01T00:00:00Z","lastUpdated":"2026-01-01T00:00:03Z","messages":[
 {"id":"u","type":"user","content":"Read the file","timestamp":"2026-01-01T00:00:01Z"},
 {"id":"a","type":"bob-shell","content":"**Inspect file**\nRead it.</thinking>","timestamp":"2026-01-01T00:00:03Z","model":"model-a","thoughts":[{"subject":"","description":"Answer","timestamp":"2026-01-01T00:00:01Z"},{"subject":"Inspect file","description":"Read","timestamp":"2026-01-01T00:00:02Z"},{"subject":"Inspect file","description":"Read it.","timestamp":"2026-01-01T00:00:02.500Z"}],"tokens":{"input":0,"output":0,"cached":0,"thoughts":0,"tool":5,"total":5},"toolCalls":[{"id":"call-1","name":"read_file","args":{"file_path":"fixture.txt"},"status":"success","timestamp":"2026-01-01T00:00:03Z","result":[{"functionResponse":{"id":"call-1","name":"read_file","response":{"output":"retained output"}}}]}]}
 ]}`)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	result := out.Results[0].Result
	require.Len(t, result.Messages, 2)
	message := result.Messages[1]
	assert.Equal(t, "Inspect file\nRead it.", message.ThinkingText)
	assert.True(t, message.HasThinking)
	assert.Equal(t, "[Thinking]\nInspect file\nRead it.\n[/Thinking]\n\nAnswer", message.Content)
	assert.True(t, message.HasContextTokens, "observed zero is not missing usage")
	assert.True(t, message.HasOutputTokens, "observed zero is not missing usage")
	require.Len(t, message.ToolCalls, 1)
	require.Len(t, message.ToolCalls[0].ResultEvents, 1)
	event := message.ToolCalls[0].ResultEvents[0]
	assert.Equal(t, "retained output", event.Content)
	assert.Equal(t, "call-1", event.ToolUseID)
	assert.Equal(t, int64(1767225603000), event.Timestamp.UnixMilli())
}

func TestBobLegacyToolErrorsAndDisplayPrecedence(t *testing.T) {
	for _, tt := range []struct{ name, display, result, want string }{
		{"error without display", "", `[{"functionResponse":{"id":"c","name":"read_file","response":{"error":"permission denied"}}}]`, "permission denied"},
		{"display and raw output", "display output", `[{"functionResponse":{"id":"c","name":"read_file","response":{"output":"raw output"}}}]`, "display output"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "tmp", "p", "chats", "session-error.json")
			writeSourceFile(t, path, `{"sessionId":"s","messages":[{"type":"bob-shell","content":"Tool response","toolCalls":[{"id":"c","name":"read_file","status":"error","resultDisplay":"`+tt.display+`","result":`+tt.result+`}]}]}`)
			_, messages, err := parseBobSession(path, "local")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Len(t, messages[0].ToolCalls, 1)
			require.Len(t, messages[0].ToolCalls[0].ResultEvents, 1)
			assert.Equal(t, tt.want, messages[0].ToolCalls[0].ResultEvents[0].Content)
		})
	}
}

func TestBobLegacyNamedThoughtsDoNotTreatAnswerStreamAsReasoning(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tmp", "p", "chats", "session-answer.json")
	writeSourceFile(t, path, `{"sessionId":"s","messages":[{"type":"bob-shell","content":"The answer.","thoughts":[{"subject":"","description":"The","timestamp":"2026-01-01T00:00:00Z"},{"subject":"","description":"The answer.","timestamp":"2026-01-01T00:00:01Z"}]}]}`)
	_, messages, err := parseBobSession(path, "local")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.False(t, messages[0].HasThinking)
	assert.Empty(t, messages[0].ThinkingText)
	assert.Equal(t, "The answer.", messages[0].Content)
}
