package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	// Reuse existing PKCE implementation
	pkgauth "github.com/rossoctl/operator/pkg/oauth"
)

// Config holds OAuth client configuration for the Token Broker.
// Endpoints are always discovered from resource server's .well-known metadata.
type Config struct {
	// ClientID is the OAuth client ID
	ClientID string

	// ClientSecret is the OAuth client secret
	ClientSecret string
}

// Client handles OAuth operations for the Token Broker.
// The Token Broker acts as the OAuth client, generating PKCE and exchanging tokens.
type Client struct {
	config     *Config
	httpClient *http.Client
	logger     *slog.Logger
}

// NewClient creates a new OAuth client.
func NewClient(config *Config, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		config:     config,
		httpClient: &http.Client{},
		logger:     logger,
	}
}

// BuildAuthorizationURL builds the OAuth authorization URL.
// The state parameter is constructed as: sessionKey + "." + random_nonce
// This allows easy lookup of the session from the state parameter.
// The authEndpoint and scopes are discovered from resource server's .well-known metadata.
// The callbackURL is the Token Broker's own callback URL.
// Returns the authorization URL and the generated state parameter.
func (c *Client) BuildAuthorizationURL(
	authEndpoint, callbackURL, sessionKey string,
	scopes []string,
	pkce *pkgauth.PKCEChallenge,
) (authURL string, state string, err error) {
	u, err := url.Parse(authEndpoint)
	if err != nil {
		return "", "", fmt.Errorf("invalid authorization endpoint: %w", err)
	}

	// Generate random nonce for state
	nonce, err := pkgauth.GenerateState()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate state nonce: %w", err)
	}

	// Construct state as sessionKey.nonce for easy lookup
	state = sessionKey + "." + nonce

	// Build query parameters
	params := url.Values{}
	params.Set("client_id", c.config.ClientID)
	params.Set("redirect_uri", callbackURL)
	params.Set("response_type", "code")
	params.Set("state", state)
	params.Set("code_challenge", pkce.Challenge)
	params.Set("code_challenge_method", pkce.Method)

	if len(scopes) > 0 {
		params.Set("scope", strings.Join(scopes, " "))
	}

	u.RawQuery = params.Encode()

	fullAuthURL := u.String()

	c.logger.Info("built authorization URL",
		"auth_endpoint", authEndpoint,
		"callback_url", callbackURL,
		"session_key", sessionKey,
		"scopes", scopes,
		"has_pkce", true,
		"state_length", len(state))
	c.logger.Debug("full authorization URL", "full_auth_url", fullAuthURL)

	return fullAuthURL, state, nil
}

// ExchangeToken exchanges an authorization code for an access token.
// The Token Broker calls the OAuth provider directly (not through resource server).
// The tokenEndpoint is discovered from resource server's .well-known metadata.
// The callbackURL must match the one used in BuildAuthorizationURL.
func (c *Client) ExchangeToken(
	ctx context.Context,
	tokenEndpoint, code, codeVerifier, callbackURL string,
) (accessToken string, expiresIn int, err error) {
	// Build token exchange request
	data := url.Values{}
	data.Set("client_id", c.config.ClientID)
	data.Set("client_secret", c.config.ClientSecret)
	data.Set("code", code)
	data.Set("code_verifier", codeVerifier)
	data.Set("redirect_uri", callbackURL)
	data.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	c.logger.Info("exchanging authorization code for token",
		"token_endpoint", tokenEndpoint,
		"client_id", c.config.ClientID,
		"has_code_verifier", codeVerifier != "")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to send token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("token exchange failed",
			"status", resp.StatusCode,
			"response", string(body))
		return "", 0, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse token response
	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}

	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", 0, fmt.Errorf("failed to parse token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return "", 0, fmt.Errorf("no access token in response")
	}

	c.logger.Info("token exchange successful",
		"token_length", len(tokenResp.AccessToken),
		"expires_in", tokenResp.ExpiresIn,
		"token_type", tokenResp.TokenType)

	return tokenResp.AccessToken, tokenResp.ExpiresIn, nil
}
