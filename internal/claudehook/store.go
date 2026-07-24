package claudehook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/swemonstro/aurora/internal/status"
)

const (
	StateFileEnv      = "AURORA_CLAUDE_STATE_FILE"
	SessionTTLEnv     = "AURORA_CLAUDE_SESSION_TTL"
	WatcherFileEnv    = "AURORA_CLAUDE_WATCHER_FILE"
	DefaultSessionTTL = 12 * time.Hour
	stateLockTimeout  = time.Second
	stateLockRetry    = 10 * time.Millisecond
)

type StateConfig struct {
	Path string
	TTL  time.Duration
}

type Session struct {
	State            status.State `json:"state"`
	UpdatedAt        time.Time    `json:"updated_at"`
	ToolUseID        string       `json:"tool_use_id,omitempty"`
	TranscriptPath   string       `json:"transcript_path,omitempty"`
	TranscriptOffset int64        `json:"transcript_offset,omitempty"`
	Revision         uint64       `json:"revision,omitempty"`
}

type sessionState struct {
	Sessions     map[string]Session `json:"sessions"`
	NextRevision uint64             `json:"next_revision,omitempty"`
}

type SessionStore struct {
	path string
	ttl  time.Duration
	now  func() time.Time
}

// QuestionWatch identifies one AskUserQuestion pending Esc recovery.
type QuestionWatch struct {
	SessionID        string `json:"session_id"`
	ToolUseID        string `json:"tool_use_id"`
	TranscriptPath   string `json:"transcript_path"`
	TranscriptOffset int64  `json:"transcript_offset"`
	Revision         uint64 `json:"revision"`
}

type LifecycleUpdate struct {
	State  status.State
	Active bool
	Watch  *QuestionWatch
}

func StateConfigFromEnv(getenv func(string) string, userHomeDir func() (string, error)) (StateConfig, error) {
	home, err := userHomeDir()
	if err != nil {
		return StateConfig{}, fmt.Errorf("resolve user home directory: %w", err)
	}

	path := strings.TrimSpace(getenv(StateFileEnv))
	if path == "" {
		path = filepath.Join(home, ".local", "state", "aurora", "claude-sessions.json")
	} else {
		path, err = expandHome(path, home)
		if err != nil {
			return StateConfig{}, err
		}
	}

	ttl := DefaultSessionTTL
	if configured := strings.TrimSpace(getenv(SessionTTLEnv)); configured != "" {
		if parsed, parseErr := time.ParseDuration(configured); parseErr == nil && parsed > 0 {
			ttl = parsed
		}
	}
	return StateConfig{Path: path, TTL: ttl}, nil
}

func NewSessionStore(path string, ttl time.Duration) (*SessionStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("session state path must not be empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("session TTL must be positive")
	}
	return &SessionStore{path: path, ttl: ttl, now: time.Now}, nil
}

func (s *SessionStore) Update(event Event) (status.State, bool, error) {
	update, supported, err := s.UpdateLifecycle(event)
	return update.State, supported, err
}

func (s *SessionStore) UpdateLifecycle(event Event) (LifecycleUpdate, bool, error) {
	action, supported := MapEvent(event)
	if !supported {
		return LifecycleUpdate{}, false, nil
	}

	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return LifecycleUpdate{}, true, fmt.Errorf("create session state directory: %w", err)
	}

	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return LifecycleUpdate{}, true, fmt.Errorf("open session state lock: %w", err)
	}
	defer lock.Close()
	if err := lock.Chmod(0o600); err != nil {
		return LifecycleUpdate{}, true, fmt.Errorf("set session state lock permissions: %w", err)
	}
	if err := lockFile(lock, stateLockTimeout); err != nil {
		return LifecycleUpdate{}, true, fmt.Errorf("lock session state: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	state, err := s.read()
	if err != nil {
		return LifecycleUpdate{}, true, err
	}
	now := s.now().UTC()
	pruneStale(state.Sessions, now, s.ttl)
	watch := applyEvent(&state, event, action, now)
	aggregate := Aggregate(state.Sessions)
	if err := s.writeAtomic(directory, state); err != nil {
		return LifecycleUpdate{}, true, err
	}
	return LifecycleUpdate{
		State:  aggregate,
		Active: len(state.Sessions) > 0,
		Watch:  watch,
	}, true, nil
}

// QuestionPending reports whether the watched AskUserQuestion is still the
// active pending question for that session (same tool_use_id and revision).
func (s *SessionStore) QuestionPending(watch QuestionWatch) (bool, error) {
	state, err := s.read()
	if err != nil {
		return false, err
	}
	return questionMatches(state.Sessions[watch.SessionID], watch), nil
}

// RecoverCancelledQuestion marks a cancelled AskUserQuestion as idle when the
// watch still matches. recovered is false when a newer lifecycle event already
// superseded this watch (no duplicate transition).
func (s *SessionStore) RecoverCancelledQuestion(watch QuestionWatch) (LifecycleUpdate, bool, error) {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return LifecycleUpdate{}, false, fmt.Errorf("create session state directory: %w", err)
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return LifecycleUpdate{}, false, fmt.Errorf("open session state lock: %w", err)
	}
	defer lock.Close()
	if err := lock.Chmod(0o600); err != nil {
		return LifecycleUpdate{}, false, fmt.Errorf("set session state lock permissions: %w", err)
	}
	if err := lockFile(lock, stateLockTimeout); err != nil {
		return LifecycleUpdate{}, false, fmt.Errorf("lock session state: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	state, err := s.read()
	if err != nil {
		return LifecycleUpdate{}, false, err
	}
	now := s.now().UTC()
	pruneStale(state.Sessions, now, s.ttl)
	session, exists := state.Sessions[watch.SessionID]
	if !exists || !questionMatches(session, watch) {
		return LifecycleUpdate{}, false, nil
	}
	state.NextRevision++
	state.Sessions[watch.SessionID] = Session{
		State:     status.Idle,
		UpdatedAt: now,
		Revision:  state.NextRevision,
	}
	if err := s.writeAtomic(directory, state); err != nil {
		return LifecycleUpdate{}, false, err
	}
	return LifecycleUpdate{
		State:  Aggregate(state.Sessions),
		Active: true,
	}, true, nil
}

func questionMatches(session Session, watch QuestionWatch) bool {
	return session.State == status.Attention &&
		session.ToolUseID == watch.ToolUseID &&
		session.TranscriptPath == watch.TranscriptPath &&
		session.Revision == watch.Revision &&
		watch.ToolUseID != "" &&
		watch.TranscriptPath != ""
}

func (s *SessionStore) read() (sessionState, error) {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return sessionState{Sessions: make(map[string]Session)}, nil
	}
	if err != nil {
		return sessionState{}, fmt.Errorf("open session state: %w", err)
	}
	defer file.Close()

	var state sessionState
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&state); err != nil {
		return sessionState{}, fmt.Errorf("decode session state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return sessionState{}, fmt.Errorf("session state contains more than one JSON value")
		}
		return sessionState{}, fmt.Errorf("decode trailing session state: %w", err)
	}
	if state.Sessions == nil {
		state.Sessions = make(map[string]Session)
	}
	return state, nil
}

func lockFile(file *os.File, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		time.Sleep(stateLockRetry)
	}
}

func (s *SessionStore) writeAtomic(directory string, state sessionState) error {
	file, err := os.CreateTemp(directory, ".claude-sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary session state: %w", err)
	}
	temporaryPath := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("set session state permissions: %w", err)
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(state); err != nil {
		return fmt.Errorf("encode session state: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync session state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close session state: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace session state: %w", err)
	}
	keep = true

	directoryFile, err := os.Open(directory)
	if err == nil {
		_ = directoryFile.Sync()
		_ = directoryFile.Close()
	}
	return nil
}

func applyEvent(state *sessionState, event Event, action EventAction, now time.Time) *QuestionWatch {
	sessionID := strings.TrimSpace(event.SessionID)
	if sessionID == "" {
		return nil
	}
	if action.Remove {
		delete(state.Sessions, sessionID)
		return nil
	}

	// New AskUserQuestion with transcript identity starts (or replaces) a watch.
	if event.HookEventName == "PreToolUse" &&
		event.ToolName == "AskUserQuestion" &&
		event.ToolUseID != "" &&
		event.TranscriptPath != "" {
		state.NextRevision++
		session := Session{
			State:            action.State,
			UpdatedAt:        now,
			ToolUseID:        event.ToolUseID,
			TranscriptPath:   event.TranscriptPath,
			TranscriptOffset: transcriptEnd(event.TranscriptPath),
			Revision:         state.NextRevision,
		}
		state.Sessions[sessionID] = session
		return &QuestionWatch{
			SessionID:        sessionID,
			ToolUseID:        session.ToolUseID,
			TranscriptPath:   session.TranscriptPath,
			TranscriptOffset: session.TranscriptOffset,
			Revision:         session.Revision,
		}
	}

	existing := state.Sessions[sessionID]
	if preservesQuestionWatch(event, existing) {
		// Keep the pending question identity so Esc recovery stays valid across
		// the verified permission_prompt follow-up for the same ask.
		state.Sessions[sessionID] = Session{
			State:            action.State,
			UpdatedAt:        now,
			ToolUseID:        existing.ToolUseID,
			TranscriptPath:   existing.TranscriptPath,
			TranscriptOffset: existing.TranscriptOffset,
			Revision:         existing.Revision,
		}
		return nil
	}

	// Any other lifecycle progress invalidates an older question watcher and
	// clears tool_use_id / transcript_path / prior revision identity.
	state.NextRevision++
	state.Sessions[sessionID] = Session{
		State:     action.State,
		UpdatedAt: now,
		Revision:  state.NextRevision,
	}
	return nil
}

// preservesQuestionWatch is true only for the verified follow-up after
// PreToolUse AskUserQuestion: Notification permission_prompt while the session
// is still attention with an active question identity. idle_prompt, unknown
// notification types, and all other lifecycle events must clear that identity
// so a later permission_prompt cannot revive a stale watch.
func preservesQuestionWatch(event Event, existing Session) bool {
	if event.HookEventName != "Notification" {
		return false
	}
	if event.NotificationType != "permission_prompt" {
		return false
	}
	if existing.State != status.Attention {
		return false
	}
	if existing.ToolUseID == "" || existing.TranscriptPath == "" {
		return false
	}
	return true
}

func transcriptEnd(path string) int64 {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func pruneStale(sessions map[string]Session, now time.Time, ttl time.Duration) {
	for id, session := range sessions {
		if session.UpdatedAt.IsZero() || now.Sub(session.UpdatedAt) > ttl {
			delete(sessions, id)
		}
	}
}

func Aggregate(sessions map[string]Session) status.State {
	aggregate := status.Idle
	for _, session := range sessions {
		switch session.State {
		case status.Error:
			return status.Error
		case status.Attention:
			if aggregate != status.Error {
				aggregate = status.Attention
			}
		case status.Working:
			if aggregate == status.Idle {
				aggregate = status.Working
			}
		}
	}
	return aggregate
}

func expandHome(path, home string) (string, error) {
	if path == "~" {
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("unsupported home-relative state path %q", path)
	}
	return path, nil
}
