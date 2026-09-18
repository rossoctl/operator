package cache

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rossoctl/operator/internal/tokenbroker/core"
)

// TokenEntry represents a cached token.
type TokenEntry struct {
	AccessToken string
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// TokenCache stores and retrieves access tokens per session and resource server.
type TokenCache struct {
	tokens map[string]map[string]*TokenEntry // sessionKey -> resourceURL -> token
	mu     sync.RWMutex
	clock  core.Clock
}

// NewTokenCache creates a new token cache.
func NewTokenCache(clock core.Clock) *TokenCache {
	return &TokenCache{
		tokens: make(map[string]map[string]*TokenEntry),
		clock:  clock,
	}
}

// GetToken retrieves a cached token for a session and resource server.
// Returns the token and true if found and not expired, empty string and false otherwise.
func (tc *TokenCache) GetToken(sessionKey, resourceURL string) (string, bool) {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	sessionTokens, ok := tc.tokens[sessionKey]
	if !ok {
		return "", false
	}

	entry, ok := sessionTokens[resourceURL]
	if !ok {
		return "", false
	}

	// Check if token is expired or near expiry
	if IsTokenExpired(entry.ExpiresAt) {
		slog.Debug("Token expired or near expiry",
			"session_key", sessionKey,
			"resource_url", resourceURL,
			"expires_at", entry.ExpiresAt,
			"now", tc.clock.Now())
		return "", false
	}

	slog.Debug("Token cache hit",
		"session_key", sessionKey,
		"resource_url", resourceURL,
		"expires_at", entry.ExpiresAt)

	return entry.AccessToken, true
}

// SetToken stores a token for a session and resource server.
func (tc *TokenCache) SetToken(sessionKey, resourceURL, token string) error {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	// Parse token expiry
	expiresAt, err := ParseJWTExpiry(token)
	if err != nil {
		return fmt.Errorf("failed to parse token expiry: %w", err)
	}

	// Create session token map if it doesn't exist
	if tc.tokens[sessionKey] == nil {
		tc.tokens[sessionKey] = make(map[string]*TokenEntry)
	}

	// Store token
	tc.tokens[sessionKey][resourceURL] = &TokenEntry{
		AccessToken: token,
		ExpiresAt:   expiresAt,
		CreatedAt:   tc.clock.Now(),
	}

	slog.Info("Token cached",
		"session_key", sessionKey,
		"resource_url", resourceURL,
		"expires_at", expiresAt,
		"ttl", expiresAt.Sub(tc.clock.Now()))

	return nil
}

// DeleteToken removes a token from the cache.
func (tc *TokenCache) DeleteToken(sessionKey, resourceURL string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if sessionTokens, ok := tc.tokens[sessionKey]; ok {
		delete(sessionTokens, resourceURL)
		slog.Info("Token removed from cache",
			"session_key", sessionKey,
			"resource_url", resourceURL)

		// Clean up empty session map
		if len(sessionTokens) == 0 {
			delete(tc.tokens, sessionKey)
		}
	}
}

// GetCacheSize returns the total number of cached tokens.
func (tc *TokenCache) GetCacheSize() int {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	count := 0
	for _, sessionTokens := range tc.tokens {
		count += len(sessionTokens)
	}
	return count
}

// GetSessionTokenCount returns the number of cached tokens for a session.
func (tc *TokenCache) GetSessionTokenCount(sessionKey string) int {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	if sessionTokens, ok := tc.tokens[sessionKey]; ok {
		return len(sessionTokens)
	}
	return 0
}

// CleanupExpiredTokens removes all expired tokens from the cache.
// This can be called periodically to free memory.
func (tc *TokenCache) CleanupExpiredTokens() int {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	removed := 0
	for sessionKey, sessionTokens := range tc.tokens {
		for resourceURL, entry := range sessionTokens {
			if IsTokenExpired(entry.ExpiresAt) {
				delete(sessionTokens, resourceURL)
				removed++
				slog.Debug("Expired token removed",
					"session_key", sessionKey,
					"resource_url", resourceURL)
			}
		}

		// Clean up empty session map
		if len(sessionTokens) == 0 {
			delete(tc.tokens, sessionKey)
		}
	}

	if removed > 0 {
		slog.Info("Expired tokens cleaned up", "removed_count", removed)
	}

	return removed
}
