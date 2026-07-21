package codexhook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type transcriptEvent struct {
	Type    string `json:"type"`
	Payload struct {
		Type   string `json:"type"`
		TurnID string `json:"turn_id"`
	} `json:"payload"`
}

// ScanTranscript reads complete JSONL records added at or after offset. Invalid
// complete records are ignored and an incomplete final record is retried later.
func ScanTranscript(path, turnID string, offset int64) (bool, int64, error) {
	path = strings.TrimSpace(path)
	turnID = strings.TrimSpace(turnID)
	if path == "" || turnID == "" {
		return false, offset, fmt.Errorf("transcript path and turn ID must not be empty")
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
		// The file was truncated. Resume only after its new end so records that
		// predate this permission request cannot be mistaken for a new abort.
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
			if len(line) > 0 {
				var event transcriptEvent
				if json.Unmarshal(line, &event) == nil &&
					event.Type == "event_msg" &&
					event.Payload.Type == "turn_aborted" &&
					strings.TrimSpace(event.Payload.TurnID) == turnID {
					return true, nextOffset, nil
				}
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
