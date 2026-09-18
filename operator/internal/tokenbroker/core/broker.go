package core

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rossoctl/operator/internal/tokenbroker/oauth"
	pkgauth "github.com/rossoctl/operator/pkg/oauth"
)

// TokenBroker orchestrates token acquisition for OAuth sessions.
type TokenBroker struct {
	sessionStore SessionStore
	tokenCache   TokenCache
	discoverer   *oauth.Discoverer
	oauthClient  *oauth.Client
	callbackURL  string // Token Broker's own callback URL
	waitTimeout  time.Duration
	logger       *slog.Logger
}

// NewTokenBroker creates a new token broker.
func NewTokenBroker(
	sessionStore SessionStore,
	tokenCache TokenCache,
	discoverer *oauth.Discoverer,
	oauthClient *oauth.Client,
	callbackURL string,
	waitTimeout time.Duration,
	logger *slog.Logger,
) *TokenBroker {
	return &TokenBroker{
		sessionStore: sessionStore,
		tokenCache:   tokenCache,
		discoverer:   discoverer,
		oauthClient:  oauthClient,
		callbackURL:  callbackURL,
		waitTimeout:  waitTimeout,
		logger:       logger,
	}
}

// AcquireToken acquires a token for a user and resource server.
// This implements the double-checked locking pattern with session semaphore.
func (tb *TokenBroker) AcquireToken(ctx context.Context, sessionKey, userID, resourceURL string) (string, error) {
	// Step 1: Validate session and user FIRST (security-critical)
	// This prevents session hijacking where user A tries to use user B's session
	if err := tb.sessionStore.ValidateSession(sessionKey, userID); err != nil {
		return "", fmt.Errorf("session validation failed: %w", err)
	}

	// Step 2: Check cache (fast path after validation)
	// Cache is now keyed by sessionKey to ensure session isolation
	if token, found := tb.tokenCache.GetToken(sessionKey, resourceURL); found {
		tb.logger.Info("Token found in cache (after session validation)",
			"session_key", sessionKey,
			"user_id", userID,
			"resource_url", resourceURL)
		return token, nil
	}

	// Get session
	session, err := tb.sessionStore.GetSession(sessionKey)
	if err != nil {
		return "", fmt.Errorf("failed to get session: %w", err)
	}

	// Step 3: Acquire per-session semaphore
	tb.logger.Debug("Acquiring session semaphore",
		"session_key", sessionKey,
		"user_id", userID,
		"resource_url", resourceURL)

	if err := session.AcquisitionSemaphore.Acquire(ctx); err != nil {
		return "", fmt.Errorf("failed to acquire semaphore: %w", err)
	}
	defer session.AcquisitionSemaphore.Release()

	tb.logger.Debug("Session semaphore acquired",
		"session_key", sessionKey)

	// Step 4: Check cache again (double-check after semaphore)
	// Cache is keyed by sessionKey to ensure session isolation
	if token, found := tb.tokenCache.GetToken(sessionKey, resourceURL); found {
		tb.logger.Info("Token found in cache after semaphore acquisition",
			"session_key", sessionKey,
			"user_id", userID,
			"resource_url", resourceURL)
		return token, nil
	}

	// Step 5: Token not in cache, initiate OAuth flow
	tb.logger.Info("Token not in cache, initiating OAuth flow",
		"session_key", sessionKey,
		"user_id", userID,
		"resource_url", resourceURL)

	token, err := tb.performOAuthFlow(ctx, session, userID, resourceURL)
	if err != nil {
		return "", fmt.Errorf("OAuth flow failed: %w", err)
	}

	return token, nil
}

// performOAuthFlow performs the complete OAuth flow to obtain a token.
// The Token Broker now acts as the OAuth client, generating PKCE and exchanging tokens.
func (tb *TokenBroker) performOAuthFlow(ctx context.Context, session *Session, userID, resourceURL string) (string, error) {
	// Step 1: Discover OAuth endpoints and scopes from resource server
	tb.logger.Debug("Discovering OAuth endpoints from resource server",
		"resource_url", resourceURL)

	authEndpoint, tokenEndpoint, scopes, err := tb.discoverer.DiscoverEndpoints(ctx, resourceURL)
	if err != nil {
		return "", fmt.Errorf("%w: failed to discover OAuth endpoints: %w", ErrOAuthUpstream, err)
	}

	tb.logger.Info("OAuth endpoints discovered",
		"resource_url", resourceURL,
		"auth_endpoint", authEndpoint,
		"token_endpoint", tokenEndpoint,
		"scopes", scopes)

	// Step 2: Generate PKCE challenge at Token Broker
	tb.logger.Debug("Generating PKCE challenge",
		"resource_url", resourceURL)

	pkce, err := pkgauth.GeneratePKCEChallenge()
	if err != nil {
		return "", fmt.Errorf("failed to generate PKCE challenge: %w", err)
	}

	tb.logger.Debug("PKCE challenge generated",
		"resource_url", resourceURL,
		"challenge_method", pkce.Method)

	// Step 3: Store OAuth transaction in session
	session.ActiveOAuthTx = &OAuthTransaction{
		ResourceURL:    resourceURL,
		AuthEndpoint:   authEndpoint,
		TokenEndpoint:  tokenEndpoint,
		Scopes:         scopes,
		PKCE:           pkce,
		CodeVerifier:   pkce.Verifier,
		Status:         OAuthTxStatusWaitingCode,
		CompletionChan: make(chan OAuthCompletion, 1),
	}

	// Step 4: Build authorization URL
	callbackURL := tb.callbackURL

	authURL, state, err := tb.oauthClient.BuildAuthorizationURL(authEndpoint, callbackURL, session.SessionKey, scopes, pkce)
	if err != nil {
		session.ActiveOAuthTx.Status = OAuthTxStatusFailed
		return "", fmt.Errorf("failed to build authorization URL: %w", err)
	}

	session.ActiveOAuthTx.AuthURL = authURL
	session.ActiveOAuthTx.CallbackURL = callbackURL
	session.ActiveOAuthTx.State = state

	tb.logger.Info("Authorization URL built",
		"resource_url", resourceURL,
		"auth_url_length", len(authURL))

	// Step 5: Publish oauth_url_ready event to Backend
	tb.logger.Info("Publishing oauth_url_ready event to Backend",
		"session_key", session.SessionKey,
		"resource_url", resourceURL,
		"auth_url_length", len(authURL))
	tb.logger.Debug("oauth_url_ready auth URL", "auth_url", authURL)

	urlReadyEvent := Event{
		Type:    "oauth_url_ready",
		AuthURL: authURL,
	}

	select {
	case session.EventWaiters <- urlReadyEvent:
		tb.logger.Info("oauth_url_ready event successfully sent to Backend",
			"session_key", session.SessionKey,
			"resource_url", resourceURL)
	case <-ctx.Done():
		return "", fmt.Errorf("context cancelled while publishing event: %w", ctx.Err())
	default:
		return "", fmt.Errorf("no event waiter available")
	}

	// Step 6: Wait for OAuth completion (authorization code from callback)
	tb.logger.Info("Waiting for OAuth completion",
		"session_key", session.SessionKey,
		"timeout", tb.waitTimeout)

	waitCtx2, cancel2 := context.WithTimeout(ctx, tb.waitTimeout)
	defer cancel2()

	var completion OAuthCompletion
	select {
	case completion = <-session.ActiveOAuthTx.CompletionChan:
		if completion.Error != nil {
			return "", fmt.Errorf("OAuth completion failed: %w", completion.Error)
		}
		tb.logger.Info("OAuth completion received",
			"session_key", session.SessionKey,
			"code_length", len(completion.Code))

	case <-waitCtx2.Done():
		session.ActiveOAuthTx.Status = OAuthTxStatusFailed
		return "", fmt.Errorf("%w", ErrOAuthTimeout)

	case <-session.Done:
		session.ActiveOAuthTx.Status = OAuthTxStatusFailed
		return "", fmt.Errorf("%w", ErrSessionEnded)

	case <-ctx.Done():
		session.ActiveOAuthTx.Status = OAuthTxStatusFailed
		return "", fmt.Errorf("context cancelled: %w", ctx.Err())
	}

	// Step 7: Exchange authorization code for token directly with OAuth provider
	tb.logger.Debug("Exchanging authorization code for token",
		"resource_url", resourceURL,
		"token_endpoint", tokenEndpoint)

	token, expiresIn, err := tb.oauthClient.ExchangeToken(ctx, tokenEndpoint, completion.Code, pkce.Verifier, callbackURL)
	if err != nil {
		session.ActiveOAuthTx.Status = OAuthTxStatusFailed
		return "", fmt.Errorf("%w: token exchange failed: %w", ErrOAuthUpstream, err)
	}

	session.ActiveOAuthTx.Status = OAuthTxStatusCompleted

	tb.logger.Info("Token obtained successfully",
		"session_key", session.SessionKey,
		"resource_url", resourceURL,
		"token_length", len(token),
		"expires_in", expiresIn)

	// Step 8: Cache token (keyed by sessionKey for session isolation)
	if err := tb.tokenCache.SetToken(session.SessionKey, resourceURL, token); err != nil {
		tb.logger.Error("Failed to cache token",
			"error", err,
			"session_key", session.SessionKey,
			"user_id", userID,
			"resource_url", resourceURL)
		// Don't fail the request, we have the token
	}

	// Step 9: Unblock any other waiters for this resource server
	if waiters, ok := session.TokenWaiters[resourceURL]; ok {
		for _, waiter := range waiters {
			select {
			case waiter <- TokenResult{Token: token, Error: nil}:
			default:
			}
			close(waiter)
		}
		delete(session.TokenWaiters, resourceURL)
		tb.logger.Debug("Unblocked token waiters",
			"session_key", session.SessionKey,
			"resource_url", resourceURL,
			"waiter_count", len(waiters))
	}

	// Clear active OAuth transaction
	session.ActiveOAuthTx = nil

	return token, nil
}

// CompleteOAuth completes an OAuth flow with the authorization code and state.
func (tb *TokenBroker) CompleteOAuth(sessionKey, userID, code, state string) error {
	// Validate session ownership (defense-in-depth)
	if err := tb.sessionStore.ValidateSession(sessionKey, userID); err != nil {
		return fmt.Errorf("session validation failed: %w", err)
	}

	session, err := tb.sessionStore.GetSession(sessionKey)
	if err != nil {
		return fmt.Errorf("session not found: %w", err)
	}

	if session.ActiveOAuthTx == nil {
		return fmt.Errorf("no active OAuth transaction")
	}

	tb.logger.Info("Completing OAuth flow",
		"session_key", sessionKey,
		"resource_url", session.ActiveOAuthTx.ResourceURL,
		"code_length", len(code),
		"state_length", len(state))

	// Send completion to waiting OAuth flow
	completion := OAuthCompletion{
		Code:  code,
		State: state,
		Error: nil,
	}

	select {
	case session.ActiveOAuthTx.CompletionChan <- completion:
		tb.logger.Debug("OAuth completion sent to waiting flow",
			"session_key", sessionKey)
		return nil
	default:
		return fmt.Errorf("no waiter for OAuth completion")
	}
}

// GetSessionStore returns the session store (for API handlers).
func (tb *TokenBroker) GetSessionStore() SessionStore {
	return tb.sessionStore
}
