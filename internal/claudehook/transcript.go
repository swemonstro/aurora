package claudehook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// ScanQuestionTranscript looks for a tool_result after offset that matches
// toolUseID with is_error=true. Content strings are never inspected or logged;
// only structural fields (type, tool_use_id, is_error) participate.
func ScanQuestionTranscript(path, toolUseID string, offset int64) (bool, int64, error) {
	path = strings.TrimSpace(path)
	toolUseID = strings.TrimSpace(toolUseID)
	if path == "" || toolUseID == "" {
		return false, offset, fmt.Errorf("transcript path and tool use ID must not be empty")
	}

	file, err := os.Open(path)
	if err != nil {
		return false, offset, fmt.Errorf("open transcript: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return false, offset, fmt.Errorf("stat transcript: %w", err)
	}
	if offset < 0 {
		offset = 0
	}
	if info.Size() < offset {
		// Truncated file: resume at new end so pre-watch records cannot match.
		return false, info.Size(), nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return false, offset, fmt.Errorf("seek transcript: %w", err)
	}

	reader := bufio.NewReader(file)
	nextOffset := offset
	for {
		line, readErr := reader.ReadBytes('\n')
		complete := readErr == nil || len(line) > 0 && line[len(line)-1] == '\n'
		if complete {
			nextOffset += int64(len(line))
			line = bytes.TrimSpace(line)
			if len(line) > 0 && lineHasErrorToolResult(line, toolUseID) {
				return true, nextOffset, nil
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return false, nextOffset, nil
			}
			return false, nextOffset, fmt.Errorf("read transcript: %w", readErr)
		}
	}
}

// lineHasErrorToolResult reports a matching errored tool_result without decoding
// free-form content into logs or return values.
func lineHasErrorToolResult(line []byte, toolUseID string) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(line, &root) != nil {
		return false
	}
	// Prefer message.content when present (Claude Code JSONL shape).
	if messageRaw, ok := root["message"]; ok {
		var message map[string]json.RawMessage
		if json.Unmarshal(messageRaw, &message) == nil {
			if contentRaw, ok := message["content"]; ok && contentHasErrorToolResult(contentRaw, toolUseID) {
				return true
			}
		}
	}
	if contentRaw, ok := root["content"]; ok && contentHasErrorToolResult(contentRaw, toolUseID) {
		return true
	}
	// Flat tool_result records.
	return blockIsErrorToolResult(root, toolUseID)
}

func contentHasErrorToolResult(contentRaw json.RawMessage, toolUseID string) bool {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(contentRaw, &blocks) != nil {
		return false
	}
	for _, block := range blocks {
		if blockIsErrorToolResult(block, toolUseID) {
			return true
		}
	}
	return false
}

func blockIsErrorToolResult(block map[string]json.RawMessage, toolUseID string) bool {
	typeRaw, ok := block["type"]
	if !ok {
		return false
	}
	var blockType string
	if json.Unmarshal(typeRaw, &blockType) != nil || blockType != "tool_result" {
		return false
	}
	idRaw, ok := block["tool_use_id"]
	if !ok {
		return false
	}
	var id string
	if json.Unmarshal(idRaw, &id) != nil || strings.TrimSpace(id) != toolUseID {
		return false
	}
	errorRaw, ok := block["is_error"]
	if !ok {
		return false
	}
	var isError bool
	if json.Unmarshal(errorRaw, &isError) != nil || !isError {
		return false
	}
	return true
}
