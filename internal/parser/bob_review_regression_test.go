package parser

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBobThoughtStreamRetainsResponseAndRepeatedPhrase(t *testing.T) {
	entry := bobMessageJSON{Content: "**Inspect**\nRead it.</thinking>\nAnswer repeats Read it.", Thoughts: []bobThoughtJSON{
		{Subject: "Inspect", Description: "Read it."},
		{Description: "Final answer only in stream."},
	}}
	content, thinking := bobMessageContent(entry)
	assert.Equal(t, "Inspect\nRead it.", thinking)
	assert.Contains(t, content, "Answer repeats Read it.")
	assert.Contains(t, content, "Final answer only in stream.")
}

func TestBobThoughtStreamRetainsMissingSubjectHeading(t *testing.T) {
	content, thinking := bobMessageContent(bobMessageJSON{Content: "<thinking>Read it.</thinking>\nAnswer", Thoughts: []bobThoughtJSON{{Subject: "Inspect", Description: "Read it."}}})
	assert.Contains(t, thinking, "Inspect")
	assert.Contains(t, thinking, "Read it.")
	assert.Contains(t, content, "Answer")
}

func TestBobResultPreservesEachResponseIdentity(t *testing.T) {
	var call bobToolCallJSON
	require.NoError(t, json.Unmarshal([]byte(`{"id":"outer","name":"read_file","result":[{"functionResponse":{"id":"inner-1","response":{"output":"one"}}},{"functionResponse":{"id":"inner-2","response":{"output":"two"}}}]}`), &call))
	calls := bobToolCalls([]bobToolCallJSON{call})
	require.Len(t, calls, 1)
	require.Len(t, calls[0].ResultEvents, 2)
	assert.Equal(t, "inner-1", calls[0].ResultEvents[0].ToolUseID)
	assert.Equal(t, "inner-2", calls[0].ResultEvents[1].ToolUseID)
}
