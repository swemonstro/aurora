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
	DefaultSessionTTL = 12 * time.Hour
	stateLockTimeout  = time.Second
	stateLockRetry    = 10 * time.Millisecond
)

type StateConfig struct {
	Path string
	TTL  time.Duration
}

type Session struct {
	State     status.State `json:"state"`
	UpdatedAt time.Time    `json:"updated_at"`
}

type sessionState struct {
	Sessions map[string]Session `json:"sessions"`
}

type SessionStore struct {
	path string
	ttl  time.Duration
	now  func() time.Time
}

type LifecycleUpdate struct {
	State  status.State
	Active bool
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
	applyEvent(state.Sessions, event.SessionID, action, now)
	aggregate := Aggregate(state.Sessions)
	if err := s.writeAtomic(directory, state); err != nil {
		return LifecycleUpdate{}, true, err
	}
	return LifecycleUpdate{State: aggregate, Active: len(state.Sessions) > 0}, true, nil
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

func applyEvent(sessions map[string]Session, rawSessionID string, action EventAction, now time.Time) {
	sessionID := strings.TrimSpace(rawSessionID)
	if sessionID == "" {
		return
	}
	if action.Remove {
		delete(sessions, sessionID)
		return
	}
	sessions[sessionID] = Session{State: action.State, UpdatedAt: now}
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
