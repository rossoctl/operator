package session

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rossoctl/operator/internal/tokenbroker/core"
)

// SessionManager manages OAuth sessions and their lifecycle.
type SessionManager struct {
	sessions           map[string]*core.Session
	userSessions       map[string][]string // userID -> session keys
	mu                 sync.RWMutex
	sessionTimeout     time.Duration
	maxSessionsPerUser int
	clock              core.Clock
	logger             *slog.Logger
	timers             map[string]*time.Timer // sessionKey -> timer
	shutdownChan       chan struct{}
}

// NewSessionManager creates a new session manager.
func NewSessionManager(sessionTimeout time.Duration, maxSessionsPerUser int, clock core.Clock, logger *slog.Logger) *SessionManager {
	return &SessionManager{
		sessions:           make(map[string]*core.Session),
		userSessions:       make(map[string][]string),
		sessionTimeout:     sessionTimeout,
		maxSessionsPerUser: maxSessionsPerUser,
		clock:              clock,
		logger:             logger,
		timers:             make(map[string]*time.Timer),
		shutdownChan:       make(chan struct{}),
	}
}

// CreateSession creates a new session with a provided session key and backend redirect URL.
func (sm *SessionManager) CreateSession(sessionKey, userID, backendRedirectURL string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Check max sessions per user
	if existingSessions, ok := sm.userSessions[userID]; ok {
		if len(existingSessions) >= sm.maxSessionsPerUser {
			sm.logger.Warn("Max sessions per user exceeded",
				"user_id", userID,
				"current_sessions", len(existingSessions),
				"max_sessions", sm.maxSessionsPerUser)
			return fmt.Errorf("max sessions per user exceeded")
		}
	}

	// Create session with provided key
	now := sm.clock.Now()
	session := &core.Session{
		SessionKey:                sessionKey,
		UserID:                    userID,
		BackendSessionRedirectURL: backendRedirectURL,
		CreatedAt:                 now,
		LastPollAt:                now,
		ExpirationDeadline:        now.Add(sm.sessionTimeout),
		AcquisitionSemaphore:      NewSemaphore(1),
		EventWaiters:              make(chan core.Event, 1),
		TokenWaiters:              make(map[string][]chan core.TokenResult),
		Done:                      make(chan struct{}),
		ActiveOAuthTx:             nil,
	}

	sm.sessions[sessionKey] = session

	// Track user sessions
	if sm.userSessions[userID] == nil {
		sm.userSessions[userID] = []string{}
	}
	sm.userSessions[userID] = append(sm.userSessions[userID], sessionKey)

	sm.logger.Info("Session created",
		"session_key", sessionKey,
		"user_id", userID,
		"backend_redirect_url", backendRedirectURL)

	return nil
}

// GetSession retrieves a session by its key.
func (sm *SessionManager) GetSession(sessionKey string) (*core.Session, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[sessionKey]
	if !ok {
		return nil, fmt.Errorf("session not found")
	}

	return session, nil
}

// GetSessionByState retrieves a session by its OAuth state parameter.
// The state format is: sessionKey.nonce (where nonce is base64url encoded, so no '.' in it)
// We find the LAST '.' to split sessionKey from nonce.
func (sm *SessionManager) GetSessionByState(state string) (*core.Session, error) {
	// Find the last '.' to split sessionKey from nonce
	lastDot := -1
	for i := len(state) - 1; i >= 0; i-- {
		if state[i] == '.' {
			lastDot = i
			break
		}
	}

	if lastDot == -1 {
		return nil, fmt.Errorf("invalid state format: no separator found")
	}

	sessionKey := state[:lastDot]

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[sessionKey]
	if !ok {
		return nil, fmt.Errorf("session not found for state")
	}

	// Verify the state matches (security check)
	if session.ActiveOAuthTx == nil || session.ActiveOAuthTx.State != state {
		return nil, fmt.Errorf("state mismatch")
	}

	return session, nil
}

// ValidateSession checks if a session exists and belongs to the specified user.
func (sm *SessionManager) ValidateSession(sessionKey, userID string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[sessionKey]
	if !ok {
		return fmt.Errorf("session not found")
	}

	if session.UserID != userID {
		sm.logger.Warn("Session user mismatch",
			"session_key", sessionKey,
			"expected_user", session.UserID,
			"provided_user", userID)
		return fmt.Errorf("session user mismatch")
	}

	return nil
}

// EndSession terminates a session and releases all resources.
func (sm *SessionManager) EndSession(sessionKey string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionKey]
	if !ok {
		return fmt.Errorf("session not found")
	}

	sm.logger.Info("Ending session",
		"session_key", sessionKey,
		"user_id", session.UserID)

	sm.cleanupSessionLocked(sessionKey, session, "session ended")

	return nil
}

// ExpireSession marks a session as expired and fails all pending requests.
func (sm *SessionManager) ExpireSession(sessionKey string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionKey]
	if !ok {
		return
	}

	sm.logger.Info("Session expired",
		"session_key", sessionKey,
		"user_id", session.UserID)

	sm.cleanupSessionLocked(sessionKey, session, "session expired")
}

// cleanupSessionLocked cleans up a session and notifies all waiters.
// Must be called with sm.mu held.
func (sm *SessionManager) cleanupSessionLocked(sessionKey string, session *core.Session, reason string) {
	// Stop timer if exists
	if timer, ok := sm.timers[sessionKey]; ok {
		timer.Stop()
		delete(sm.timers, sessionKey)
	}

	// Fail all token waiters
	for resourceURL, waiters := range session.TokenWaiters {
		for _, waiter := range waiters {
			select {
			case waiter <- core.TokenResult{
				Token: "",
				Error: fmt.Errorf("%s", reason),
			}:
			default:
			}
			close(waiter)
		}
		sm.logger.Debug("Failed token waiters",
			"session_key", sessionKey,
			"resource_url", resourceURL,
			"waiter_count", len(waiters))
	}

	// Close Done to unblock any waiters (e.g. AcquireToken blocked on the
	// OAuth completion channel). cleanupSessionLocked runs at most once per
	// key, so this close is safe.
	close(session.Done)

	// Send a final error event so a connected Backend long-poll learns the
	// session ended. EventWaiters is buffered (cap 1); the send is best-effort.
	//
	// We intentionally do NOT close(EventWaiters): performOAuthFlow may still be
	// sending to it concurrently (it does not hold sm.mu), and closing a channel
	// during a concurrent send is a data race that panics. The channel is
	// garbage-collected with the session; Done is the broadcast that unblocks
	// waiters.
	select {
	case session.EventWaiters <- core.Event{
		Type:    "error",
		Message: reason,
		Code:    "session_expired",
	}:
	default:
	}

	// Remove from user sessions
	if userSessions, ok := sm.userSessions[session.UserID]; ok {
		newSessions := []string{}
		for _, key := range userSessions {
			if key != sessionKey {
				newSessions = append(newSessions, key)
			}
		}
		if len(newSessions) > 0 {
			sm.userSessions[session.UserID] = newSessions
		} else {
			delete(sm.userSessions, session.UserID)
		}
	}

	// Remove session
	delete(sm.sessions, sessionKey)

	sm.logger.Debug("Session cleaned up",
		"session_key", sessionKey,
		"reason", reason)
}

// ResetSessionTimer resets the idle timeout timer for a session.
func (sm *SessionManager) ResetSessionTimer(sessionKey string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionKey]
	if !ok {
		return
	}

	// Stop existing timer
	if timer, ok := sm.timers[sessionKey]; ok {
		timer.Stop()
		delete(sm.timers, sessionKey)
	}

	// Update last poll time
	session.LastPollAt = sm.clock.Now()
	session.ExpirationDeadline = session.LastPollAt.Add(sm.sessionTimeout)

	sm.logger.Debug("Session timer reset",
		"session_key", sessionKey,
		"user_id", session.UserID)
}

// StartSessionTimer starts the idle timeout timer for a session.
func (sm *SessionManager) StartSessionTimer(sessionKey string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionKey]
	if !ok {
		return
	}

	// Stop existing timer
	if timer, ok := sm.timers[sessionKey]; ok {
		timer.Stop()
	}

	// Start new timer
	timer := time.AfterFunc(sm.sessionTimeout, func() {
		sm.ExpireSession(sessionKey)
	})
	sm.timers[sessionKey] = timer

	sm.logger.Debug("Session timer started",
		"session_key", sessionKey,
		"user_id", session.UserID,
		"timeout", sm.sessionTimeout)
}

// Shutdown stops all timers and cleans up all sessions.
func (sm *SessionManager) Shutdown() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	close(sm.shutdownChan)

	sm.logger.Info("Shutting down session manager",
		"active_sessions", len(sm.sessions))

	// Stop all timers
	for _, timer := range sm.timers {
		timer.Stop()
	}
	sm.timers = make(map[string]*time.Timer)

	// Clean up all sessions
	for sessionKey, session := range sm.sessions {
		sm.cleanupSessionLocked(sessionKey, session, "shutdown")
	}

	sm.logger.Info("Session manager shutdown complete")
}

// GetSessionCount returns the number of active sessions.
func (sm *SessionManager) GetSessionCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

// GetUserSessionCount returns the number of active sessions for a user.
func (sm *SessionManager) GetUserSessionCount(userID string) int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if sessions, ok := sm.userSessions[userID]; ok {
		return len(sessions)
	}
	return 0
}
