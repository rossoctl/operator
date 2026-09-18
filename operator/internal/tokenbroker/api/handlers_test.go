package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/rossoctl/operator/internal/tokenbroker/core"
)

// mockBroker implements a mock TokenBroker for testing
type mockBroker struct {
	acquireTokenFunc func(ctx context.Context, sessionKey, userID, resourceURL string) (string, error)
}

func (m *mockBroker) AcquireToken(ctx context.Context, sessionKey, userID, resourceURL string) (string, error) {
	if m.acquireTokenFunc != nil {
		return m.acquireTokenFunc(ctx, sessionKey, userID, resourceURL)
	}
	return "mock-token", nil
}

func (m *mockBroker) CompleteOAuth(sessionKey, userID, code, state string) error {
	return nil
}

func (m *mockBroker) GetSessionStore() core.SessionStore {
	return &mockSessionStore{}
}

// mockSessionStore implements a mock SessionStore for testing
type mockSessionStore struct {
	validateSessionFunc   func(sessionKey, userID string) error
	getSessionFunc        func(sessionKey string) (*core.Session, error)
	getSessionByStateFunc func(state string) (*core.Session, error)
}

func (m *mockSessionStore) CreateSession(sessionKey, userID, backendRedirectURL string) error {
	return nil
}

func (m *mockSessionStore) GetSession(sessionKey string) (*core.Session, error) {
	if m.getSessionFunc != nil {
		return m.getSessionFunc(sessionKey)
	}

	return &core.Session{
		SessionKey: sessionKey,
		UserID:     "user123",
		CreatedAt:  time.Now(),
	}, nil
}

func (m *mockSessionStore) GetSessionByState(state string) (*core.Session, error) {
	if m.getSessionByStateFunc != nil {
		return m.getSessionByStateFunc(state)
	}
	return &core.Session{
		SessionKey: "mock-session",
		UserID:     "user123",
		CreatedAt:  time.Now(),
	}, nil
}

func (m *mockSessionStore) ValidateSession(sessionKey, userID string) error {
	if m.validateSessionFunc != nil {
		return m.validateSessionFunc(sessionKey, userID)
	}
	return nil
}

func (m *mockSessionStore) EndSession(sessionKey string) error {
	return nil
}

func (m *mockSessionStore) ExpireSession(sessionKey string) {}

func (m *mockSessionStore) ResetSessionTimer(sessionKey string) {}

func (m *mockSessionStore) StartSessionTimer(sessionKey string) {}

func TestHandleGetToken_NewAPI(t *testing.T) {
	tests := []struct {
		name                string
		authHeader          string
		serverURLHeader     string
		authEndpointHeader  string
		tokenEndpointHeader string
		wantStatus          int
		wantToken           string
		wantErrorCode       string
	}{
		{
			name:            "valid request with JWT",
			authHeader:      "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIiwianRpIjoic2Vzc2lvbjQ1NiJ9.signature", // #notsecret
			serverURLHeader: "https://mcp.example.com",
			wantStatus:      http.StatusOK,
			wantToken:       "mock-token",
		},
		{
			name:                "valid request with OAuth endpoints",
			authHeader:          "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIiwianRpIjoic2Vzc2lvbjQ1NiJ9.signature", // #notsecret
			serverURLHeader:     "https://mcp.example.com",
			authEndpointHeader:  "https://auth.example.com/authorize",
			tokenEndpointHeader: "https://auth.example.com/token",
			wantStatus:          http.StatusOK,
			wantToken:           "mock-token",
		},
		{
			name:            "missing Authorization header",
			serverURLHeader: "https://mcp.example.com",
			wantStatus:      http.StatusUnauthorized,
			wantErrorCode:   "invalid_token",
		},
		{
			name:            "invalid Authorization header format",
			authHeader:      "Basic dXNlcjpwYXNz",
			serverURLHeader: "https://mcp.example.com",
			wantStatus:      http.StatusUnauthorized,
			wantErrorCode:   "invalid_token",
		},
		{
			name:          "missing X-Server-Url header",
			authHeader:    "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIiwianRpIjoic2Vzc2lvbjQ1NiJ9.signature", // #notsecret
			wantStatus:    http.StatusBadRequest,
			wantErrorCode: "invalid_request",
		},
		{
			name:            "invalid JWT - missing sub claim",
			authHeader:      "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJqdGkiOiJzZXNzaW9uNDU2In0.signature", // #notsecret
			serverURLHeader: "https://mcp.example.com",
			wantStatus:      http.StatusUnauthorized,
			wantErrorCode:   "invalid_token",
		},
		{
			name:            "invalid JWT - missing jti claim",
			authHeader:      "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIn0.signature", // #notsecret
			serverURLHeader: "https://mcp.example.com",
			wantStatus:      http.StatusUnauthorized,
			wantErrorCode:   "invalid_token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock broker
			broker := &mockBroker{
				acquireTokenFunc: func(ctx context.Context, sessionKey, userID, resourceURL string) (string, error) {
					// Verify parameters
					if userID != "user123" {
						t.Errorf("Expected userID 'user123', got '%s'", userID)
					}
					if sessionKey != "session456" {
						t.Errorf("Expected sessionKey 'session456', got '%s'", sessionKey)
					}
					if resourceURL != "https://mcp.example.com" {
						t.Errorf("Expected resourceURL 'https://mcp.example.com', got '%s'", resourceURL)
					}
					// Note: authEndpoint and tokenEndpoint are no longer passed to AcquireToken
					// They are discovered internally by the broker
					return "mock-token", nil
				},
			}

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			handler := NewHandler(broker, &mockSessionStore{}, logger, nil)

			// Create request
			req := httptest.NewRequest(http.MethodPost, "/sessions/token", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			if tt.serverURLHeader != "" {
				req.Header.Set("X-Server-Url", tt.serverURLHeader)
			}
			if tt.authEndpointHeader != "" {
				req.Header.Set("X-Authorization-Endpoint", tt.authEndpointHeader)
			}
			if tt.tokenEndpointHeader != "" {
				req.Header.Set("X-Token-Endpoint", tt.tokenEndpointHeader)
			}

			// Create response recorder
			w := httptest.NewRecorder()

			// Call handler
			handler.HandleGetToken(w, req)

			// Check status code
			if w.Code != tt.wantStatus {
				t.Errorf("Expected status %d, got %d", tt.wantStatus, w.Code)
			}

			// Check response body
			if tt.wantStatus == http.StatusOK {
				var response map[string]string
				if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
					t.Fatalf("Failed to decode response: %v", err)
				}
				if response["token"] != tt.wantToken {
					t.Errorf("Expected token '%s', got '%s'", tt.wantToken, response["token"])
				}
			} else {
				var errorResponse ErrorResponse
				if err := json.NewDecoder(w.Body).Decode(&errorResponse); err != nil {
					t.Fatalf("Failed to decode error response: %v", err)
				}
				if errorResponse.Code != tt.wantErrorCode {
					t.Errorf("Expected error code '%s', got '%s'", tt.wantErrorCode, errorResponse.Code)
				}
			}
		})
	}
}

// TestHandleGetTokenErrorClassification verifies that HandleGetToken classifies
// broker errors by sentinel (via errors.Is) even when the broker wraps them, and
// never returns 503 — a 5xx makes agents mark the route dead and stop retrying.
func TestHandleGetTokenErrorClassification(t *testing.T) {
	// A valid JWT for user123 / session456 so we reach the broker call.
	const validJWT = "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIiwianRpIjoic2Vzc2lvbjQ1NiJ9.signature" // #notsecret

	tests := []struct {
		name          string
		brokerErr     error
		wantStatus    int
		wantErrorCode string
	}{
		{
			// The reviewer's regression: session ended, but wrapped by AcquireToken.
			name:          "wrapped session ended -> 401",
			brokerErr:     fmt.Errorf("OAuth flow failed: %w", core.ErrSessionEnded),
			wantStatus:    http.StatusUnauthorized,
			wantErrorCode: "session_expired",
		},
		{
			name:          "wrapped session expired -> 401",
			brokerErr:     fmt.Errorf("OAuth flow failed: %w", core.ErrSessionExpired),
			wantStatus:    http.StatusUnauthorized,
			wantErrorCode: "session_expired",
		},
		{
			name:          "wrapped oauth timeout -> 408",
			brokerErr:     fmt.Errorf("OAuth flow failed: %w", core.ErrOAuthTimeout),
			wantStatus:    http.StatusRequestTimeout,
			wantErrorCode: "timeout",
		},
		{
			name:          "wrapped upstream failure -> 502",
			brokerErr:     fmt.Errorf("OAuth flow failed: %w", core.ErrOAuthUpstream),
			wantStatus:    http.StatusBadGateway,
			wantErrorCode: "oauth_upstream",
		},
		{
			name:          "unknown error -> 500 (not 503)",
			brokerErr:     errors.New("something unexpected"),
			wantStatus:    http.StatusInternalServerError,
			wantErrorCode: "internal_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker := &mockBroker{
				acquireTokenFunc: func(ctx context.Context, sessionKey, userID, resourceURL string) (string, error) {
					return "", tt.brokerErr
				},
			}
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			handler := NewHandler(broker, &mockSessionStore{}, logger, nil)

			req := httptest.NewRequest(http.MethodPost, "/sessions/token", nil)
			req.Header.Set("Authorization", validJWT)
			req.Header.Set("X-Server-Url", "https://mcp.example.com")

			w := httptest.NewRecorder()
			handler.HandleGetToken(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("Expected status %d, got %d", tt.wantStatus, w.Code)
			}
			if w.Code == http.StatusServiceUnavailable {
				t.Errorf("Handler returned 503; agents treat 5xx as a dead route and stop retrying")
			}

			var errorResponse ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&errorResponse); err != nil {
				t.Fatalf("Failed to decode error response: %v", err)
			}
			if errorResponse.Code != tt.wantErrorCode {
				t.Errorf("Expected error code '%s', got '%s'", tt.wantErrorCode, errorResponse.Code)
			}
		})
	}
}

func TestErrorResponseFormats(t *testing.T) {
	t.Run("AuthBridge API uses flat error format", func(t *testing.T) {
		w := httptest.NewRecorder()
		writeError(w, http.StatusBadRequest, "test_code", "test message")

		var response ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if response.Code != "test_code" {
			t.Errorf("Expected code 'test_code', got '%s'", response.Code)
		}
		if response.Message != "test message" {
			t.Errorf("Expected message 'test message', got '%s'", response.Message)
		}
	})

	t.Run("Backend API uses nested error format", func(t *testing.T) {
		w := httptest.NewRecorder()
		writeBackendError(w, http.StatusBadRequest, "test_code", "test message")

		var response BackendErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}

		if response.Error.Code != "test_code" {
			t.Errorf("Expected code 'test_code', got '%s'", response.Error.Code)
		}
		if response.Error.Message != "test message" {
			t.Errorf("Expected message 'test message', got '%s'", response.Error.Message)
		}
	})
}

func TestHandleOAuthCallback(t *testing.T) {
	tests := []struct {
		name         string
		code         string
		state        string
		wantStatus   int
		wantLocation string
	}{
		{
			name:         "valid OAuth callback",
			code:         "auth-code-123",
			state:        "session-key-456.nonce789",
			wantStatus:   http.StatusFound,
			wantLocation: "https://backend.example.com/oauth/complete?oauth_status=success",
		},
		{
			name:       "missing code parameter",
			state:      "session-key-456.nonce789",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing state parameter",
			code:       "auth-code-123",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid state format (no dot)",
			code:       "auth-code-123",
			state:      "invalid-state-no-dot",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock broker
			broker := &mockBroker{}

			// Create mock session store with custom GetSessionByState
			sessionStore := &mockSessionStore{
				getSessionByStateFunc: func(state string) (*core.Session, error) {
					if state == "" || state == "invalid-state-no-dot" {
						return nil, errors.New("session not found")
					}
					return &core.Session{
						SessionKey:                "session-key-456",
						UserID:                    "user123",
						BackendSessionRedirectURL: "https://backend.example.com/oauth/complete",
						ActiveOAuthTx: &core.OAuthTransaction{
							State: state,
						},
					}, nil
				},
			}

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			handler := NewHandler(broker, sessionStore, logger, nil)

			// Create request with query parameters
			url := "/oauth/callback"
			if tt.code != "" || tt.state != "" {
				url += "?"
				if tt.code != "" {
					url += "code=" + tt.code
				}
				if tt.state != "" {
					if tt.code != "" {
						url += "&"
					}
					url += "state=" + tt.state
				}
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)

			// Create response recorder
			w := httptest.NewRecorder()

			// Call handler
			handler.HandleOAuthCallback(w, req)

			// Check status code
			if w.Code != tt.wantStatus {
				t.Errorf("Expected status %d, got %d", tt.wantStatus, w.Code)
			}

			// Check redirect location for successful callback
			if tt.wantStatus == http.StatusFound {
				location := w.Header().Get("Location")
				if location != tt.wantLocation {
					t.Errorf("Expected Location header '%s', got '%s'", tt.wantLocation, location)
				}
			}
		})
	}
}

func TestHandleEvents_LongPollReturnsEvent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	eventCh := make(chan core.Event, 1)
	doneCh := make(chan struct{})

	sessionStore := &mockSessionStore{
		validateSessionFunc: func(sessionKey, userID string) error {
			if sessionKey != "session456" || userID != "user123" {
				return errors.New("invalid session")
			}
			return nil
		},
		getSessionFunc: func(sessionKey string) (*core.Session, error) {
			return &core.Session{
				SessionKey:   sessionKey,
				UserID:       "user123",
				EventWaiters: eventCh,
				Done:         doneCh,
			}, nil
		},
	}

	broker := &mockBroker{}
	handler := NewHandler(broker, sessionStore, logger, nil)

	req := httptest.NewRequest(http.MethodPost, "/sessions/broker-events", nil)
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIiwianRpIjoic2Vzc2lvbjQ1NiJ9.signature") // #notsecret
	w := httptest.NewRecorder()

	go func() {
		time.Sleep(10 * time.Millisecond)
		eventCh <- core.Event{Type: "oauth_url_ready", AuthURL: "https://example.com/auth"}
	}()

	handler.HandleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var event core.Event
	if err := json.NewDecoder(w.Body).Decode(&event); err != nil {
		t.Fatalf("Failed to decode event: %v", err)
	}

	if event.Type != "oauth_url_ready" {
		t.Fatalf("Expected event type oauth_url_ready, got %s", event.Type)
	}
	if event.AuthURL != "https://example.com/auth" {
		t.Fatalf("Expected auth_url https://example.com/auth, got %s", event.AuthURL)
	}
}

func TestHandleEvents_LongPollReturnsSessionClosedEvent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	eventCh := make(chan core.Event, 1)
	doneCh := make(chan struct{})

	sessionStore := &mockSessionStore{
		validateSessionFunc: func(sessionKey, userID string) error {
			if sessionKey != "session456" || userID != "user123" {
				return errors.New("invalid session")
			}
			return nil
		},
		getSessionFunc: func(sessionKey string) (*core.Session, error) {
			return &core.Session{
				SessionKey:   sessionKey,
				UserID:       "user123",
				EventWaiters: eventCh,
				Done:         doneCh,
			}, nil
		},
	}

	broker := &mockBroker{}
	handler := NewHandler(broker, sessionStore, logger, nil)

	req := httptest.NewRequest(http.MethodPost, "/sessions/broker-events", nil)
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIiwianRpIjoic2Vzc2lvbjQ1NiJ9.signature") // #notsecret
	w := httptest.NewRecorder()

	go func() {
		time.Sleep(10 * time.Millisecond)
		close(doneCh)
	}()

	handler.HandleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var event core.Event
	if err := json.NewDecoder(w.Body).Decode(&event); err != nil {
		t.Fatalf("Failed to decode event: %v", err)
	}

	if event.Type != "error" {
		t.Fatalf("Expected event type error, got %s", event.Type)
	}
	if event.Code != "session_expired" {
		t.Fatalf("Expected event code session_expired, got %s", event.Code)
	}
}
