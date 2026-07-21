package codexhook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func RecordSessionID(path string, event Event) error {
	path = strings.TrimSpace(path)
	if path == "" || event.HookEventName != "SessionStart" {
		return nil
	}

	sessionID := strings.TrimSpace(event.SessionID)
	if sessionID == "" {
		return nil
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create session ID directory: %w", err)
	}

	file, err := os.OpenFile(
		path,
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("open session ID file: %w", err)
	}
	defer file.Close()

	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("set session ID file permissions: %w", err)
	}
	if _, err := fmt.Fprintln(file, sessionID); err != nil {
		return fmt.Errorf("write session ID file: %w", err)
	}

	return nil
}
