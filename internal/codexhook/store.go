package codexhook

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
	DefaultSessionTTL = 12 * time.Hour
	stateLockTimeout  = time.Second
	stateLockRetry    = 10 * time.Millisecond
)

type Session struct {
	State            status.State `json:"state"`
	UpdatedAt        time.Time    `json:"updated_at"`
	TurnID           string       `json:"turn_id,omitempty"`
	TranscriptPath   string       `json:"transcript_path,omitempty"`
	TranscriptOffset int64        `json:"transcript_offset,omitempty"`
	Revision         uint64       `json:"revision,omitempty"`
}

type sessionState struct {
	Sessions     map[string]Session `json:"sessions"`
	NextRevision uint64             `json:"next_revision,omitempty"`
}

type PermissionWatch struct {
	SessionID        string `json:"session_id"`
	TurnID           string `json:"turn_id"`
	TranscriptPath   string `json:"transcript_path"`
	TranscriptOffset int64  `json:"transcript_offset"`
	Revision         uint64 `json:"revision"`
}

type LifecycleUpdate struct {
	State  status.State
	Active bool
	Watch  *PermissionWatch
}

type SessionStore struct {
	path string
	ttl  time.Duration
	now  func() time.Time
}

func NewSessionStore(path string, ttl time.Duration) (*SessionStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("session state path must not be empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("session TTL must be positive")
	}

	return &SessionStore{
		path: path,
		ttl:  ttl,
		now:  time.Now,
	}, nil
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

func (s *SessionStore) PermissionPending(watch PermissionWatch) (bool, error) {
	state, err := s.read()
	if err != nil {
		return false, err
	}
	return permissionMatches(state.Sessions[watch.SessionID], watch), nil
}

func (s *SessionStore) RecoverCancelled(watch PermissionWatch) (LifecycleUpdate, bool, error) {
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
	if !exists || !permissionMatches(session, watch) {
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

func permissionMatches(session Session, watch PermissionWatch) bool {
	return session.State == status.Attention &&
		session.TurnID == watch.TurnID &&
		session.TranscriptPath == watch.TranscriptPath &&
		session.Revision == watch.Revision
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
		if !errors.Is(err, syscall.EWOULDBLOCK) &&
			!errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}

		time.Sleep(stateLockRetry)
	}
}

func (s *SessionStore) writeAtomic(directory string, state sessionState) error {
	file, err := os.CreateTemp(directory, ".codex-sessions-*.tmp")
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
	if err := json.NewEncoder(file).Encode(state); err != nil {
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

func applyEvent(
	state *sessionState,
	event Event,
	action EventAction,
	now time.Time,
) *PermissionWatch {
	sessionID := strings.TrimSpace(event.SessionID)
	if sessionID == "" {
		return nil
	}

	if action.Remove {
		delete(state.Sessions, sessionID)
		return nil
	}

	state.NextRevision++
	session := Session{
		State:     action.State,
		UpdatedAt: now,
		Revision:  state.NextRevision,
	}
	if event.HookEventName == "PermissionRequest" {
		session.TurnID = strings.TrimSpace(event.TurnID)
		session.TranscriptPath = strings.TrimSpace(event.TranscriptPath)
		session.TranscriptOffset = transcriptEnd(session.TranscriptPath)
	}
	state.Sessions[sessionID] = session

	if session.State != status.Attention || session.TurnID == "" || session.TranscriptPath == "" {
		return nil
	}
	return &PermissionWatch{
		SessionID:        sessionID,
		TurnID:           session.TurnID,
		TranscriptPath:   session.TranscriptPath,
		TranscriptOffset: session.TranscriptOffset,
		Revision:         session.Revision,
	}
}

func transcriptEnd(path string) int64 {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}

func pruneStale(
	sessions map[string]Session,
	now time.Time,
	ttl time.Duration,
) {
	for id, session := range sessions {
		if session.UpdatedAt.IsZero() ||
			now.Sub(session.UpdatedAt) > ttl {
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
			aggregate = status.Attention
		case status.Working:
			if aggregate == status.Idle {
				aggregate = status.Working
			}
		}
	}

	return aggregate
}
