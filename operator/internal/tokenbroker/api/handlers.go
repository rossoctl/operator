package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/jwtvalidation/validation"
	"github.com/rossoctl/operator/internal/tokenbroker/auth"
	"github.com/rossoctl/operator/internal/tokenbroker/core"
)

// TokenBroker defines the interface for token acquisition operations.
type TokenBroker interface {
	AcquireToken(ctx context.Context, sessionKey, userID, resourceURL string) (string, error)
	CompleteOAuth(sessionKey, userID, code, state string) error
	GetSessionStore() core.SessionStore
}

// Handler handles HTTP requests for the Token Broker API.
type Handler struct {
	broker               TokenBroker
	sessionStore         core.SessionStore
	logger               *slog.Logger
	allowedRedirectHosts []string
	jwtVerifier          validation.Verifier // nil = fallback to no-validation mode
	jwtAudiences         []string
}

// NewHandler creates a new API handler.
func NewHandler(broker TokenBroker, sessionStore core.SessionStore, logger *slog.Logger, allowedRedirectHosts []string) *Handler {
	return &Handler{
		broker:               broker,
		sessionStore:         sessionStore,
		logger:               logger,
		allowedRedirectHosts: allowedRedirectHosts,
	}
}

// WithJWTVerifier sets the JWT verifier and expected audiences on the handler.
func (h *Handler) WithJWTVerifier(verifier validation.Verifier, audiences []string) *Handler {
	h.jwtVerifier = verifier
	h.jwtAudiences = audiences
	return h
}

// RegisterRoutes registers all API routes.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Post("/sessions", h.HandleCreateSession)
	r.Post("/sessions/token", h.HandleGetToken)       // AuthBridge API endpoint
	r.Post("/sessions/broker-events", h.HandleEvents) // Backend API endpoint (JWT auth)
	r.Post("/sessions/end", h.HandleEndSession)       // Backend API endpoint (JWT auth)
	r.Get("/oauth/callback", h.HandleOAuthCallback)   // OAuth provider callback
}

// parseAndValidateJWT extracts and validates the JWT from the Authorization header.
// When a JWKS verifier is configured it performs full signature/expiry/issuer/audience
// validation. When no verifier is configured it falls back to claim-only parsing (dev mode).
// Returns (userID, sessionKey) on success, or writes an error response and returns ("", "").
func (h *Handler) parseAndValidateJWT(w http.ResponseWriter, r *http.Request, errWriter func(http.ResponseWriter, int, string, string)) (userID, sessionKey string) {
	authHeader := r.Header.Get("Authorization")
	tokenStr, err := auth.ExtractBearerToken(authHeader)
	if err != nil {
		h.logger.Warn("Failed to extract bearer token", "error", err)
		errWriter(w, http.StatusUnauthorized, "invalid_token", "Missing or invalid Authorization header")
		return "", ""
	}

	if h.jwtVerifier != nil {
		claims, err := h.jwtVerifier.Verify(r.Context(), tokenStr, h.jwtAudiences)
		if err != nil {
			h.logger.Warn("JWT validation failed", "error", err)
			errWriter(w, http.StatusUnauthorized, "invalid_token", fmt.Sprintf("JWT validation failed: %v", err))
			return "", ""
		}
		// Extract session key from Extra map (session_uid preferred, jti as fallback).
		sk, _ := claims.Extra["session_uid"].(string)
		if sk == "" {
			sk, _ = claims.Extra["jti"].(string)
		}
		if claims.Subject == "" || sk == "" {
			h.logger.Warn("JWT missing required claims", "has_sub", claims.Subject != "", "has_session_key", sk != "")
			errWriter(w, http.StatusUnauthorized, "invalid_token", "JWT missing required sub or session_uid/jti claims")
			return "", ""
		}
		return claims.Subject, sk
	}

	// Fallback: no-validation mode (dev/test only — WARNING logged at startup).
	legacyClaims, err := auth.ParseJWTWithoutValidation(tokenStr)
	if err != nil {
		h.logger.Warn("Failed to parse JWT", "error", err)
		errWriter(w, http.StatusUnauthorized, "invalid_token", fmt.Sprintf("Invalid JWT: %v", err))
		return "", ""
	}
	return legacyClaims.Sub, legacyClaims.GetSessionKey()
}

// ErrorResponse represents a standardized error response (flat format).
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BackendErrorResponse represents the nested error format used by Backend endpoints.
type BackendErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains error details (for Backend error format).
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Default().Error("failed to encode JSON response", "error", err)
	}
}

// writeError writes a standardized error response (flat format for AuthBridge API).
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{
		Code:    code,
		Message: message,
	})
}

// writeBackendError writes a standardized error response (nested format for Backend API).
func writeBackendError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, BackendErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

// CreateSessionRequest represents the request body for POST /sessions
type CreateSessionRequest struct {
	BackendSessionRedirectURL string `json:"backend_session_redirect_url"`
}

// HandleCreateSession handles POST /sessions
func (h *Handler) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	userID, sessionKey := h.parseAndValidateJWT(w, r, writeBackendError)
	if userID == "" {
		return
	}

	// Parse request body for backend redirect URL
	var req CreateSessionRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.logger.Warn("Failed to parse request body", "error", err)
			writeBackendError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
			return
		}
	}

	// Validate backend redirect URL against allowed hosts.
	if req.BackendSessionRedirectURL != "" && len(h.allowedRedirectHosts) > 0 {
		parsed, err := url.Parse(req.BackendSessionRedirectURL)
		if err != nil || !h.isAllowedRedirectHost(parsed.Hostname()) {
			h.logger.Warn("Rejected backend_session_redirect_url: host not in allowlist",
				"host", func() string {
					if err != nil {
						return "<invalid>"
					}
					return parsed.Hostname()
				}())
			writeBackendError(w, http.StatusBadRequest, "invalid_request", "backend_session_redirect_url host is not permitted")
			return
		}
	}

	h.logger.Info("Creating session",
		"user_id", userID,
		"session_key", sessionKey,
		"backend_redirect_url", req.BackendSessionRedirectURL)

	// Create session with provided session key and redirect URL
	err := h.sessionStore.CreateSession(sessionKey, userID, req.BackendSessionRedirectURL)
	if err != nil {
		if err.Error() == "max sessions per user exceeded" {
			writeBackendError(w, http.StatusTooManyRequests, "too_many_sessions", err.Error())
			return
		}
		h.logger.Error("Failed to create session", "error", err, "user_id", userID, "session_key", sessionKey)
		writeBackendError(w, http.StatusInternalServerError, "internal_error", "Failed to create session")
		return
	}

	h.logger.Info("Session created", "session_key", sessionKey, "user_id", userID)

	// Return success (client already knows the session key from JWT jti)
	w.WriteHeader(http.StatusCreated)
}

// HandleGetToken handles POST /sessions/token (new API with JWT authentication).
func (h *Handler) HandleGetToken(w http.ResponseWriter, r *http.Request) {
	userID, sessionKey := h.parseAndValidateJWT(w, r, writeError)
	if userID == "" {
		return
	}

	// Extract resource URL from header.
	// This is set by the authbridge sidecar from operator-configured routes — trusted infrastructure.
	resourceURL := r.Header.Get("X-Server-Url")
	if resourceURL == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Missing X-Server-Url header")
		return
	}

	h.logger.Info("Token request (new API)",
		"session_key", sessionKey,
		"user_id", userID,
		"resource_url", resourceURL)

	// Validate session
	if err := h.sessionStore.ValidateSession(sessionKey, userID); err != nil {
		h.logger.Warn("Session validation failed",
			"session_key", sessionKey,
			"user_id", userID,
			"error", err)
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid session or user mismatch")
		return
	}

	// Acquire token (blocks until available or timeout)
	ctx := r.Context()
	accessToken, err := h.broker.AcquireToken(ctx, sessionKey, userID, resourceURL)
	if err != nil {
		h.logger.Error("Token acquisition failed",
			"session_key", sessionKey,
			"user_id", userID,
			"resource_url", resourceURL,
			"error", err)

		// Classify the error with errors.Is so that wrapping (AcquireToken wraps
		// every performOAuthFlow error) does not collapse a specific, recoverable
		// condition into a generic 5xx. A 5xx makes agents/sidecars mark this route
		// dead and stop retrying, so it is reserved for genuinely retryable
		// server/upstream failures — never session/client state.
		switch {
		case errors.Is(err, core.ErrOAuthTimeout):
			// User did not finish the OAuth flow in time — transient, retryable.
			writeError(w, http.StatusRequestTimeout, "timeout", "OAuth flow did not complete in time")
		case errors.Is(err, core.ErrSessionEnded), errors.Is(err, core.ErrSessionExpired):
			// Session gone — permanent for this session; client must re-establish.
			writeError(w, http.StatusUnauthorized, "session_expired", "Session ended or expired")
		case errors.Is(err, core.ErrOAuthUpstream):
			// Discovery / token exchange failed upstream — transient, retryable.
			writeError(w, http.StatusBadGateway, "oauth_upstream", "Upstream OAuth provider error")
		default:
			// Unknown: do not signal a permanently-dead route. Retryable server error.
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to obtain token")
		}
		return
	}

	h.logger.Info("Token acquired (new API)",
		"session_key", sessionKey,
		"user_id", userID,
		"resource_url", resourceURL)

	// Return token
	writeJSON(w, http.StatusOK, map[string]string{
		"token": accessToken,
	})
}

// HandleEvents handles POST /sessions/events
func (h *Handler) HandleEvents(w http.ResponseWriter, r *http.Request) {
	userID, sessionKey := h.parseAndValidateJWT(w, r, writeBackendError)
	if userID == "" {
		return
	}

	// Long-poll for events
	h.handleEventLongPoll(w, r, sessionKey, userID)
}

// handleEventLongPoll handles long-polling for events.
func (h *Handler) handleEventLongPoll(w http.ResponseWriter, r *http.Request, sessionKey, userID string) {
	h.logger.Debug("Event long-poll started",
		"session_key", sessionKey,
		"user_id", userID)

	// Validate session
	if err := h.sessionStore.ValidateSession(sessionKey, userID); err != nil {
		h.logger.Warn("Session validation failed for event poll",
			"session_key", sessionKey,
			"user_id", userID,
			"error", err)
		writeBackendError(w, http.StatusUnauthorized, "unauthorized", "Invalid session or user mismatch")
		return
	}

	// Reset session timer (Backend is connected)
	h.sessionStore.ResetSessionTimer(sessionKey)

	// Get session
	session, err := h.sessionStore.GetSession(sessionKey)
	if err != nil {
		writeBackendError(w, http.StatusUnauthorized, "session_not_found", "Session not found")
		return
	}

	// When we return, start the session timer
	defer h.sessionStore.StartSessionTimer(sessionKey)

	select {
	case event := <-session.EventWaiters:
		h.logger.Info("Event sent to Backend",
			"session_key", sessionKey,
			"event_type", event.Type)
		writeJSON(w, http.StatusOK, event)

	case <-session.Done:
		h.logger.Info("Session closed while waiting for event",
			"session_key", sessionKey,
			"user_id", userID)
		writeJSON(w, http.StatusOK, core.Event{
			Type:    "error",
			Message: "Session expired",
			Code:    "session_expired",
		})

	case <-r.Context().Done():
		h.logger.Debug("Event long-poll canceled by client",
			"session_key", sessionKey,
			"reason", r.Context().Err())
		return
	}
}

// HandleOAuthCallback handles GET /oauth/callback - receives OAuth callbacks from OAuth provider
func (h *Handler) HandleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	// Log the full callback request for debugging
	h.logger.Info("OAuth callback received from provider",
		"method", r.Method,
		"url", r.URL.String(),
		"remote_addr", r.RemoteAddr,
		"user_agent", r.Header.Get("User-Agent"),
		"referer", r.Header.Get("Referer"))

	// Extract code and state from query parameters
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	errorParam := r.URL.Query().Get("error")
	errorDescription := r.URL.Query().Get("error_description")

	h.logger.Info("OAuth callback parameters",
		"has_code", code != "",
		"code_length", len(code),
		"has_state", state != "",
		"state_length", len(state),
		"has_error", errorParam != "",
		"error", errorParam,
		"error_description", errorDescription)

	// Check for OAuth provider errors
	if errorParam != "" {
		h.logger.Warn("OAuth provider returned error",
			"error", errorParam,
			"description", errorDescription)

		// Look up session by state to get redirect URL
		if state != "" {
			if session, err := h.sessionStore.GetSessionByState(state); err == nil && session.BackendSessionRedirectURL != "" {
				// Redirect to backend with error parameters
				redirectURL := h.addQueryParams(session.BackendSessionRedirectURL, map[string]string{
					"oauth_status":      "error",
					"error":             errorParam,
					"error_description": errorDescription,
				})
				http.Redirect(w, r, redirectURL, http.StatusFound)
				return
			}
		}
		http.Error(w, fmt.Sprintf("OAuth error: %s - %s", errorParam, errorDescription), http.StatusBadRequest)
		return
	}

	if code == "" || state == "" {
		h.logger.Warn("OAuth callback missing required parameters",
			"has_code", code != "",
			"has_state", state != "")
		http.Error(w, "Missing code or state parameter", http.StatusBadRequest)
		return
	}

	h.logger.Info("OAuth callback received",
		"code_length", len(code),
		"state_length", len(state))

	// Look up session by state parameter
	session, err := h.sessionStore.GetSessionByState(state)
	if err != nil {
		h.logger.Error("Failed to find session for OAuth callback",
			"state_length", len(state),
			"error", err)
		http.Error(w, "Invalid or expired OAuth session", http.StatusBadRequest)
		return
	}

	h.logger.Info("Session found for OAuth callback",
		"session_key", session.SessionKey,
		"user_id", session.UserID)

	// Complete OAuth flow
	if err := h.broker.CompleteOAuth(session.SessionKey, session.UserID, code, state); err != nil {
		h.logger.Error("Failed to complete OAuth",
			"session_key", session.SessionKey,
			"user_id", session.UserID,
			"error", err)

		// Redirect to backend with error
		if session.BackendSessionRedirectURL != "" {
			redirectURL := h.addQueryParams(session.BackendSessionRedirectURL, map[string]string{
				"oauth_status":      "error",
				"error":             "token_exchange_failed",
				"error_description": "Failed to exchange authorization code for access token",
			})
			http.Redirect(w, r, redirectURL, http.StatusFound)
			return
		}

		http.Error(w, "Failed to complete OAuth flow", http.StatusInternalServerError)
		return
	}

	h.logger.Info("OAuth completion processed",
		"session_key", session.SessionKey)

	// Redirect user to Backend's session redirect URL with success status
	if session.BackendSessionRedirectURL == "" {
		h.logger.Warn("No backend redirect URL configured for session",
			"session_key", session.SessionKey)
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OAuth authorization successful. You may close this window.")); err != nil {
			h.logger.Warn("Failed to write OAuth success response", "error", err)
		}
		return
	}

	// Add success status to redirect URL
	redirectURL := h.addQueryParams(session.BackendSessionRedirectURL, map[string]string{
		"oauth_status": "success",
	})

	h.logger.Info("Redirecting user to backend",
		"session_key", session.SessionKey,
		"redirect_url", redirectURL)

	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// addQueryParams adds query parameters to a URL, preserving existing parameters
func (h *Handler) addQueryParams(baseURL string, params map[string]string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		h.logger.Warn("Failed to parse redirect URL", "url", baseURL, "error", err)
		return baseURL
	}

	q := u.Query()
	for key, value := range params {
		q.Set(key, value)
	}
	u.RawQuery = q.Encode()

	return u.String()
}

// HandleEndSession handles POST /sessions/end
func (h *Handler) HandleEndSession(w http.ResponseWriter, r *http.Request) {
	userID, sessionKey := h.parseAndValidateJWT(w, r, writeBackendError)
	if userID == "" {
		return
	}

	h.logger.Info("Ending session",
		"session_key", sessionKey,
		"user_id", userID)

	// Validate session
	if err := h.sessionStore.ValidateSession(sessionKey, userID); err != nil {
		h.logger.Warn("Session validation failed for end session",
			"session_key", sessionKey,
			"user_id", userID,
			"error", err)
		writeBackendError(w, http.StatusUnauthorized, "unauthorized", "Invalid session or user mismatch")
		return
	}

	// End session
	if err := h.sessionStore.EndSession(sessionKey); err != nil {
		h.logger.Error("Failed to end session",
			"session_key", sessionKey,
			"error", err)
		writeBackendError(w, http.StatusInternalServerError, "internal_error", "Failed to end session")
		return
	}

	h.logger.Info("Session ended",
		"session_key", sessionKey)

	w.WriteHeader(http.StatusOK)
}

// isAllowedRedirectHost reports whether host is in the configured allowlist.
func (h *Handler) isAllowedRedirectHost(host string) bool {
	for _, allowed := range h.allowedRedirectHosts {
		if allowed == host {
			return true
		}
	}
	return false
}
