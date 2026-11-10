package parser

import (
	"encoding/json"
	"strings"
)

type bobThoughtJSON struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Timestamp   string `json:"timestamp"`
}

func (tokens *bobTokensJSON) UnmarshalJSON(data []byte) error {
	type plain bobTokensJSON
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*tokens = bobTokensJSON(value)
	tokens.Present = strings.TrimSpace(string(data)) != "null"
	return nil
}

type bobResultJSON struct {
	FunctionResponse *struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Response struct {
			Output string `json:"output"`
			Error  string `json:"error"`
		} `json:"response"`
	} `json:"functionResponse"`
	InlineData *struct {
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	} `json:"inlineData"`
}

func bobMessageContent(entry bobMessageJSON) (string, string) {
	content, thinking, _ := bobLegacyMessageProjection(entry)
	return content, thinking
}

func bobLegacyMessageProjection(entry bobMessageJSON) (string, string, string) {
	_, thinking, dialogue := bobVisibleContent(entry.Content)
	var streams []bobThoughtJSON
	for _, thought := range entry.Thoughts {
		if len(streams) > 0 && streams[len(streams)-1].Subject == thought.Subject &&
			strings.HasPrefix(thought.Description, streams[len(streams)-1].Description) {
			streams[len(streams)-1] = thought
		} else {
			streams = append(streams, thought)
		}
	}
	var additional, answers []string
	for _, thought := range streams {
		subject, description := strings.TrimSpace(thought.Subject), strings.TrimSpace(thought.Description)
		if subject == "" {
			if description != "" {
				answers = append(answers, description)
			}
			continue
		}
		if description != "" && strings.Contains(thinking, description) {
			if !strings.Contains(thinking, subject) {
				thinking = subject + "\n" + thinking
				// The subject is reasoning metadata, not dialogue.
			}
			continue
		}
		additional = append(additional, strings.TrimSpace(subject+"\n"+description))
		if thinking == "" {
			trimmed := strings.TrimSpace(dialogue)
			if strings.HasPrefix(trimmed, "**"+subject+"**") {
				rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "**"+subject+"**"))
				if strings.HasPrefix(rest, description) {
					dialogue = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(rest, description)), "</thinking>"))
				}
			}
		}
	}
	if len(additional) > 0 {
		extra := strings.Join(additional, "\n\n")
		if thinking != "" {
			thinking += "\n\n" + extra
		} else {
			thinking = extra
		}
	}
	for _, answer := range answers {
		if !strings.Contains(dialogue, answer) && !(thinking != "" && strings.Contains(thinking, answer) && strings.TrimSpace(entry.Content) != "") {
			if dialogue != "" {
				dialogue += "\n\n"
			}
			dialogue += answer
		}
	}
	content := dialogue
	if thinking != "" {
		content = "[Thinking]\n" + thinking + "\n[/Thinking]"
		if dialogue != "" {
			content += "\n\n" + dialogue
		}
	}
	return content, thinking, dialogue
}

func bobLegacyToolCalls(raw []bobToolCallJSON) []ParsedToolCall {
	calls := make([]ParsedToolCall, 0, len(raw))
	for _, call := range raw {
		if call.Name == "" {
			continue
		}
		input := ""
		if len(call.Args) > 0 && string(call.Args) != "null" {
			input = string(call.Args)
		}
		parsed := ParsedToolCall{ToolUseID: call.ID, ToolName: call.Name, Category: NormalizeToolCategory(call.Name), InputJSON: input}
		display := strings.TrimSpace(call.ResultDisplay)
		for _, result := range call.Result {
			if response := result.FunctionResponse; response != nil {
				text := response.Response.Output
				if response.Response.Error != "" {
					text = response.Response.Error
				}
				if display != "" {
					text = display
					display = ""
				}
				parsed.ResultEvents = append(parsed.ResultEvents, ParsedToolResultEvent{ToolUseID: firstNonEmptyJSONLString(response.ID, call.ID), Source: "bob", Status: call.Status, Content: text, Timestamp: parseTimestamp(call.Timestamp)})
			}
			if image := result.InlineData; image != nil && image.Data != "" {
				text := "![Tool result](data:" + image.MimeType + ";base64," + image.Data + ")"
				if len(parsed.ResultEvents) > 0 {
					parsed.ResultEvents[len(parsed.ResultEvents)-1].Content += "\n\n" + text
				} else {
					parsed.ResultEvents = append(parsed.ResultEvents, ParsedToolResultEvent{ToolUseID: call.ID, Source: "bob", Status: call.Status, Content: text, Timestamp: parseTimestamp(call.Timestamp)})
				}
			}
		}
		if display != "" {
			parsed.ResultEvents = append(parsed.ResultEvents, ParsedToolResultEvent{ToolUseID: call.ID, Source: "bob", Status: call.Status, Content: display, Timestamp: parseTimestamp(call.Timestamp)})
		}
		calls = append(calls, parsed)
	}
	return calls
}
