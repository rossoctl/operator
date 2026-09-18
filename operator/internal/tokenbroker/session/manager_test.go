package session

import (
	"testing"
	"time"

	"log/slog"
	"os"

	"github.com/google/uuid"
	"github.com/rossoctl/operator/internal/tokenbroker/core"
)

// FakeClock for testing
type FakeClock struct {
	now time.Time
}

func (c *FakeClock) Now() time.Time {
	return c.now
}

func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.now.Add(d)
	return ch
}

func (c *FakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestSessionManager_CreateSession(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	sm := NewSessionManager(60*time.Second, 5, clock, logger)
	defer sm.Shutdown()

	userID := "user1"
	sessionKey := uuid.New().String()
	err := sm.CreateSession(sessionKey, userID, "")
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Verify session exists
	session, err := sm.GetSession(sessionKey)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if session.UserID != userID {
		t.Errorf("Expected user ID %s, got %s", userID, session.UserID)
	}
}

func TestSessionManager_ValidateSession(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	sm := NewSessionManager(60*time.Second, 5, clock, logger)
	defer sm.Shutdown()

	userID := "user1"
	sessionKey := uuid.New().String()
	_ = sm.CreateSession(sessionKey, userID, "")

	// Valid user
	err := sm.ValidateSession(sessionKey, userID)
	if err != nil {
		t.Errorf("ValidateSession should succeed for correct user: %v", err)
	}

	// Wrong user
	err = sm.ValidateSession(sessionKey, "user2")
	if err == nil {
		t.Error("ValidateSession should fail for wrong user")
	}

	// Non-existent session
	err = sm.ValidateSession("non-existent", userID)
	if err == nil {
		t.Error("ValidateSession should fail for non-existent session")
	}
}

func TestSessionManager_MaxSessionsPerUser(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	maxSessions := 3
	sm := NewSessionManager(60*time.Second, maxSessions, clock, logger)
	defer sm.Shutdown()

	userID := "user1"

	// Create max sessions
	for i := 0; i < maxSessions; i++ {
		sessionKey := uuid.New().String()
		err := sm.CreateSession(sessionKey, userID, "")
		if err != nil {
			t.Fatalf("CreateSession %d failed: %v", i, err)
		}
	}

	// Next session should fail
	sessionKey := uuid.New().String()
	err := sm.CreateSession(sessionKey, userID, "")
	if err == nil {
		t.Error("CreateSession should fail when max sessions exceeded")
	}
}

func TestSessionManager_EndSession(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	sm := NewSessionManager(60*time.Second, 5, clock, logger)
	defer sm.Shutdown()

	userID := "user1"
	sessionKey := uuid.New().String()
	_ = sm.CreateSession(sessionKey, userID, "")

	// Capture Done before ending so we can assert waiters are unblocked.
	session, err := sm.GetSession(sessionKey)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	// End session
	err = sm.EndSession(sessionKey)
	if err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	// Done should be closed so blocked waiters (e.g. AcquireToken) return.
	select {
	case <-session.Done:
	default:
		t.Error("EndSession should close session.Done")
	}

	// Session should no longer exist
	_, err = sm.GetSession(sessionKey)
	if err == nil {
		t.Error("GetSession should fail for ended session")
	}
}

func TestSessionManager_ExpireSession(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	sm := NewSessionManager(60*time.Second, 5, clock, logger)
	defer sm.Shutdown()

	userID := "user1"
	sessionKey := uuid.New().String()
	_ = sm.CreateSession(sessionKey, userID, "")

	// Capture Done before expiring so we can assert waiters are unblocked.
	session, err := sm.GetSession(sessionKey)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	// Expire session
	sm.ExpireSession(sessionKey)

	// Done should be closed so blocked waiters (e.g. AcquireToken) return.
	select {
	case <-session.Done:
	default:
		t.Error("ExpireSession should close session.Done")
	}

	// Session should no longer exist
	_, err = sm.GetSession(sessionKey)
	if err == nil {
		t.Error("GetSession should fail for expired session")
	}
}

func TestSessionManager_SessionTimer(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	timeout := 100 * time.Millisecond
	sm := NewSessionManager(timeout, 5, clock, logger)
	defer sm.Shutdown()

	userID := "user1"
	sessionKey := uuid.New().String()
	_ = sm.CreateSession(sessionKey, userID, "")

	// Start timer
	sm.StartSessionTimer(sessionKey)

	// Wait for timeout
	time.Sleep(timeout + 50*time.Millisecond)

	// Session should be expired
	_, err := sm.GetSession(sessionKey)
	if err == nil {
		t.Error("Session should be expired after timeout")
	}
}

func TestSessionManager_ResetSessionTimer(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	timeout := 150 * time.Millisecond
	sm := NewSessionManager(timeout, 5, clock, logger)
	defer sm.Shutdown()

	userID := "user1"
	sessionKey := uuid.New().String()
	_ = sm.CreateSession(sessionKey, userID, "")

	// Start timer
	sm.StartSessionTimer(sessionKey)

	// Wait half the timeout
	time.Sleep(timeout / 2)

	// Reset timer
	sm.ResetSessionTimer(sessionKey)

	// Wait another half timeout (total time > original timeout)
	time.Sleep(timeout / 2)

	// Session should still exist (timer was reset)
	_, err := sm.GetSession(sessionKey)
	if err != nil {
		t.Error("Session should still exist after timer reset")
	}

	// Start a new timer after reset
	sm.StartSessionTimer(sessionKey)

	// Wait for full timeout
	time.Sleep(timeout + 100*time.Millisecond)

	// Now session should be expired
	_, err = sm.GetSession(sessionKey)
	if err == nil {
		t.Error("Session should be expired after full timeout")
	}
}

func TestSessionManager_GetSessionByState(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	sm := NewSessionManager(60*time.Second, 5, clock, logger)
	defer sm.Shutdown()

	userID := "user1"
	sessionKey := "test-session-key"
	backendRedirectURL := "https://backend.example.com/oauth/complete"

	// Create session
	err := sm.CreateSession(sessionKey, userID, backendRedirectURL)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Set up OAuth transaction with state
	session, _ := sm.GetSession(sessionKey)
	session.ActiveOAuthTx = &core.OAuthTransaction{
		State: sessionKey + ".abc123def456",
	}

	// Test valid state
	retrievedSession, err := sm.GetSessionByState(sessionKey + ".abc123def456")
	if err != nil {
		t.Fatalf("GetSessionByState failed: %v", err)
	}
	if retrievedSession.SessionKey != sessionKey {
		t.Errorf("Expected session key %s, got %s", sessionKey, retrievedSession.SessionKey)
	}

	// Test state with dots in session key
	sessionKeyWithDots := "session.with.dots"
	err = sm.CreateSession(sessionKeyWithDots, userID, backendRedirectURL)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	session2, _ := sm.GetSession(sessionKeyWithDots)
	session2.ActiveOAuthTx = &core.OAuthTransaction{
		State: sessionKeyWithDots + ".xyz789",
	}

	retrievedSession2, err := sm.GetSessionByState(sessionKeyWithDots + ".xyz789")
	if err != nil {
		t.Fatalf("GetSessionByState failed for session key with dots: %v", err)
	}
	if retrievedSession2.SessionKey != sessionKeyWithDots {
		t.Errorf("Expected session key %s, got %s", sessionKeyWithDots, retrievedSession2.SessionKey)
	}

	// Test invalid state (no dot)
	_, err = sm.GetSessionByState("invalid-state-no-dot")
	if err == nil {
		t.Error("GetSessionByState should fail for state without dot")
	}

	// Test non-existent session
	_, err = sm.GetSessionByState("nonexistent.nonce")
	if err == nil {
		t.Error("GetSessionByState should fail for non-existent session")
	}
}

func TestSessionManager_GetSessionCount(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	sm := NewSessionManager(60*time.Second, 5, clock, logger)
	defer sm.Shutdown()

	if count := sm.GetSessionCount(); count != 0 {
		t.Errorf("Expected session count 0, got %d", count)
	}

	// Create sessions
	_ = sm.CreateSession(uuid.New().String(), "user1", "")
	_ = sm.CreateSession(uuid.New().String(), "user1", "")
	_ = sm.CreateSession(uuid.New().String(), "user2", "")

	if count := sm.GetSessionCount(); count != 3 {
		t.Errorf("Expected session count 3, got %d", count)
	}
}

func TestSessionManager_GetUserSessionCount(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	sm := NewSessionManager(60*time.Second, 5, clock, logger)
	defer sm.Shutdown()

	user1 := "user1"
	user2 := "user2"

	// Create sessions
	_ = sm.CreateSession(uuid.New().String(), user1, "")
	_ = sm.CreateSession(uuid.New().String(), user1, "")
	_ = sm.CreateSession(uuid.New().String(), user2, "")

	if count := sm.GetUserSessionCount(user1); count != 2 {
		t.Errorf("Expected user1 session count 2, got %d", count)
	}

	if count := sm.GetUserSessionCount(user2); count != 1 {
		t.Errorf("Expected user2 session count 1, got %d", count)
	}

	if count := sm.GetUserSessionCount("user3"); count != 0 {
		t.Errorf("Expected user3 session count 0, got %d", count)
	}
}

func TestSessionManager_Shutdown(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	sm := NewSessionManager(60*time.Second, 5, clock, logger)

	// Create sessions
	_ = sm.CreateSession(uuid.New().String(), "user1", "")
	_ = sm.CreateSession(uuid.New().String(), "user2", "")

	if count := sm.GetSessionCount(); count != 2 {
		t.Errorf("Expected session count 2, got %d", count)
	}

	// Shutdown
	sm.Shutdown()

	// All sessions should be cleaned up
	if count := sm.GetSessionCount(); count != 0 {
		t.Errorf("Expected session count 0 after shutdown, got %d", count)
	}
}

func TestSessionManager_TokenWaiters(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	sm := NewSessionManager(60*time.Second, 5, clock, logger)
	defer sm.Shutdown()

	userID := "user1"
	sessionKey := uuid.New().String()
	_ = sm.CreateSession(sessionKey, userID, "")

	session, _ := sm.GetSession(sessionKey)

	// Add token waiters
	resource := "http://mcp.example.com"
	waiter1 := make(chan core.TokenResult, 1)
	waiter2 := make(chan core.TokenResult, 1)

	session.TokenWaiters[resource] = []chan core.TokenResult{waiter1, waiter2}

	// End session (should notify waiters)
	_ = sm.EndSession(sessionKey)

	// Verify waiters were notified
	select {
	case result := <-waiter1:
		if result.Error == nil {
			t.Error("Waiter should receive error on session end")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Waiter1 was not notified")
	}

	select {
	case result := <-waiter2:
		if result.Error == nil {
			t.Error("Waiter should receive error on session end")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Waiter2 was not notified")
	}
}

func TestSessionManager_EventWaiters(t *testing.T) {
	clock := &FakeClock{now: time.Now()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	sm := NewSessionManager(60*time.Second, 5, clock, logger)
	defer sm.Shutdown()

	userID := "user1"
	sessionKey := uuid.New().String()
	_ = sm.CreateSession(sessionKey, userID, "")

	session, _ := sm.GetSession(sessionKey)

	// Start goroutine waiting for event
	eventReceived := make(chan bool, 1)
	go func() {
		select {
		case event := <-session.EventWaiters:
			if event.Type == "error" && event.Code == "session_expired" {
				eventReceived <- true
			}
		case <-time.After(200 * time.Millisecond):
			eventReceived <- false
		}
	}()

	// Give goroutine time to start waiting
	time.Sleep(50 * time.Millisecond)

	// End session (should send error event)
	_ = sm.EndSession(sessionKey)

	// Verify event was received
	select {
	case received := <-eventReceived:
		if !received {
			t.Error("Event waiter should receive error event on session end")
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("Timeout waiting for event")
	}
}

// Verify FakeClock implements core.Clock
var _ core.Clock = (*FakeClock)(nil)
