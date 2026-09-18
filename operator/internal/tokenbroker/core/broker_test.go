package core_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/rossoctl/operator/internal/tokenbroker/cache"
	"github.com/rossoctl/operator/internal/tokenbroker/core"
	"github.com/rossoctl/operator/internal/tokenbroker/oauth"
	"github.com/rossoctl/operator/internal/tokenbroker/session"
)

// TestAcquireToken_UnblocksOnSessionEnd verifies that ending a session while a
// token request is blocked waiting for OAuth completion returns promptly with a
// "session ended" error, rather than hanging until TokenWaitTimeout.
func TestAcquireToken_UnblocksOnSessionEnd(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	clock := &core.RealClock{}

	tokenCache := cache.NewTokenCache(clock)
	// Long timeout: the test must succeed because EndSession unblocks the wait,
	// not because the wait times out.
	sessionManager := session.NewSessionManager(5*time.Minute, 5, clock, logger)
	defer sessionManager.Shutdown()

	oauthClient := oauth.NewClient(&oauth.Config{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
	}, logger)

	// Configured metadata so DiscoverEndpoints does not make an HTTP request.
	discoverer := oauth.NewDiscovererWithConfig(&oauth.DiscovererConfig{
		AuthorizationEndpoint: "https://example.com/oauth/authorize",
		TokenEndpoint:         "https://example.com/oauth/token",
		ScopesSupported:       []string{"read"},
	}, logger)

	broker := core.NewTokenBroker(
		sessionManager,
		tokenCache,
		discoverer,
		oauthClient,
		"http://broker.example.com/oauth/callback",
		5*time.Minute,
		logger,
	)

	userID := "user1"
	sessionKey := "session-1"
	resourceURL := "https://mcp.example.com"

	if err := sessionManager.CreateSession(sessionKey, userID, ""); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// AcquireToken blocks waiting for OAuth completion (no callback will arrive).
	type result struct {
		token string
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		token, err := broker.AcquireToken(context.Background(), sessionKey, userID, resourceURL)
		resultCh <- result{token: token, err: err}
	}()

	// Give the goroutine time to reach the OAuth-completion wait, then end the
	// session out from under it.
	time.Sleep(100 * time.Millisecond)
	if err := sessionManager.EndSession(sessionKey); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	select {
	case res := <-resultCh:
		if res.err == nil {
			t.Fatalf("expected error after session ended, got token %q", res.token)
		}
		// Must be classifiable via errors.Is despite AcquireToken's wrapping —
		// this is what lets the API layer return 401 instead of a spurious 503.
		if !errors.Is(res.err, core.ErrSessionEnded) {
			t.Errorf("expected error to wrap ErrSessionEnded, got: %v", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AcquireToken did not return after EndSession; it is still blocked")
	}
}
