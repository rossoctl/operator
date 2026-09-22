package core

import "errors"

// Sentinel errors returned by the token broker. They are classified in the API
// layer with errors.Is so that wrapping (e.g. fmt.Errorf("...: %w", err)) does
// not collapse a specific, recoverable condition into a generic 5xx response.
//
// Why this matters: agents/sidecars treat a 5xx from the token service as
// "this route is dead" and stop retrying it. So a 5xx must be reserved for
// failures that are permanent or genuinely server-side, never for session/client
// state (401) or a transient upstream hiccup (502).
var (
	// ErrSessionEnded indicates the session was ended (e.g. by the backend or
	// shutdown) while a token acquisition was in flight. Permanent for this
	// session; the client must establish a new one. Maps to 401.
	ErrSessionEnded = errors.New("session ended")

	// ErrSessionExpired indicates the session's idle timeout elapsed. Permanent
	// for this session; the client must establish a new one. Maps to 401.
	ErrSessionExpired = errors.New("session expired")

	// ErrOAuthTimeout indicates the user did not complete the OAuth flow within
	// the wait timeout. Transient — the flow can be retried. Maps to 408.
	ErrOAuthTimeout = errors.New("timeout waiting for OAuth completion")

	// ErrOAuthUpstream indicates a failure talking to the OAuth provider or the
	// resource-server discovery endpoint. Transient upstream error, retryable.
	// Maps to 502.
	ErrOAuthUpstream = errors.New("oauth upstream failure")
)
