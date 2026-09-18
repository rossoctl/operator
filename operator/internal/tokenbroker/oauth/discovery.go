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
)

// ResourceConfig holds per-resource-server OAuth configuration overrides.
type ResourceConfig struct {
	// Scopes overrides the scopes to request for this server, replacing discovered scopes.
	Scopes []string `json:"scopes,omitempty"`

	// AuthorizationEndpoint overrides the authorization endpoint for this server,
	// replacing the one derived from .well-known discovery.
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`

	// TokenEndpoint overrides the token endpoint for this server,
	// replacing the one derived from .well-known discovery.
	TokenEndpoint string `json:"token_endpoint,omitempty"`
}

// DiscovererConfig holds optional configured OAuth metadata.
// If provided, these values will be used instead of discovery.
type DiscovererConfig struct {
	AuthorizationEndpoint string
	TokenEndpoint         string
	ScopesSupported       []string

	// ResourceScopes maps resource server URL to OAuth scopes (legacy, scopes-only override).
	// Deprecated: use ResourceConfigs for richer per-server configuration.
	ResourceScopes map[string][]string

	// ResourceConfigs maps resource server URL to full per-server OAuth config overrides.
	// Takes precedence over ResourceScopes for the same server URL.
	ResourceConfigs map[string]ResourceConfig
}

// Discoverer discovers OAuth endpoints from resource server's .well-known metadata.
// It can also use pre-configured endpoints as a fallback.
type Discoverer struct {
	httpClient *http.Client
	logger     *slog.Logger
	config     *DiscovererConfig
}

// NewDiscoverer creates a new OAuth endpoint discoverer without configured metadata.
func NewDiscoverer(logger *slog.Logger) *Discoverer {
	return NewDiscovererWithConfig(nil, logger)
}

// NewDiscovererWithConfig creates a new OAuth endpoint discoverer with optional configured metadata.
func NewDiscovererWithConfig(config *DiscovererConfig, logger *slog.Logger) *Discoverer {
	if logger == nil {
		logger = slog.Default()
	}
	if config == nil {
		config = &DiscovererConfig{}
	}
	return &Discoverer{
		httpClient: &http.Client{},
		logger:     logger,
		config:     config,
	}
}

// DiscoverEndpoints discovers OAuth endpoints and scopes for a resource server.
//
// Resolution order (highest priority first):
//  1. Per-server config from ResourceConfigs (overrides individual fields)
//  2. Global static config (OAUTH_AUTHORIZATION_ENDPOINT + OAUTH_TOKEN_ENDPOINT)
//  3. .well-known/oauth-protected-resource discovery
//  4. Legacy ResourceScopes (scopes-only override, lower priority than ResourceConfigs)
func (d *Discoverer) DiscoverEndpoints(ctx context.Context, resourceURL string) (string, string, []string, error) {
	// Check for full per-server config first (highest priority)
	if serverCfg, ok := d.config.ResourceConfigs[resourceURL]; ok {
		if serverCfg.AuthorizationEndpoint != "" && serverCfg.TokenEndpoint != "" {
			d.logger.Info("Using per-server OAuth config (skipping discovery)",
				"resource_url", resourceURL,
				"authorization_endpoint", serverCfg.AuthorizationEndpoint,
				"token_endpoint", serverCfg.TokenEndpoint,
				"scopes", serverCfg.Scopes)
			return serverCfg.AuthorizationEndpoint, serverCfg.TokenEndpoint, serverCfg.Scopes, nil
		}
	}

	// Check if we have fully configured global metadata (all required fields)
	hasConfiguredMetadata := d.config.AuthorizationEndpoint != "" && d.config.TokenEndpoint != ""

	if hasConfiguredMetadata {
		scopes := d.config.ScopesSupported
		if serverCfg, ok := d.config.ResourceConfigs[resourceURL]; ok && len(serverCfg.Scopes) > 0 {
			scopes = serverCfg.Scopes
		} else if override, ok := d.config.ResourceScopes[resourceURL]; ok {
			scopes = override
		}
		d.logger.Info("Using global OAuth metadata (skipping discovery)",
			"resource_url", resourceURL,
			"authorization_endpoint", d.config.AuthorizationEndpoint,
			"token_endpoint", d.config.TokenEndpoint,
			"scopes", scopes)
		return d.config.AuthorizationEndpoint, d.config.TokenEndpoint, scopes, nil
	}

	// resourceURL originates from the X-Server-Url header set by the authbridge sidecar
	// from operator-configured routes — it is trusted infrastructure, not end-user input.
	// The scheme/host validation below is defence-in-depth, not a SSRF boundary.
	parsedResource, err := url.Parse(resourceURL)
	if err != nil || (parsedResource.Scheme != "http" && parsedResource.Scheme != "https") || parsedResource.Host == "" {
		return "", "", nil, fmt.Errorf("invalid resource URL %q: must be http or https with a non-empty host", resourceURL)
	}

	// Attempt .well-known discovery
	// lgtm[go/request-forgery] resourceURL is operator-configured, not user-controlled
	wellKnownURL := strings.TrimSuffix(resourceURL, "/") + "/.well-known/oauth-protected-resource"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnownURL, nil)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to create discovery request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	d.logger.Debug("discovering OAuth endpoints and scopes",
		"well_known_url", wellKnownURL)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to send discovery request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", nil, fmt.Errorf("discovery endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to read discovery response: %w", err)
	}

	var metadata struct {
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}

	if err := json.Unmarshal(body, &metadata); err != nil {
		return "", "", nil, fmt.Errorf("failed to parse discovery response: %w", err)
	}

	if len(metadata.AuthorizationServers) == 0 {
		return "", "", nil, fmt.Errorf("no authorization servers found in discovery response")
	}

	// Derive endpoints from authorization_servers[0].
	// Example: "https://github.com/login/oauth" →
	//   authorization_endpoint: https://github.com/login/oauth/authorize
	//   token_endpoint:         https://github.com/login/oauth/access_token
	authServerBase := strings.TrimSuffix(metadata.AuthorizationServers[0], "/")
	authorizationEndpoint := authServerBase + "/authorize"
	tokenEndpoint := authServerBase + "/access_token"

	// Apply per-server endpoint overrides (partial — only endpoints, not full skip-discovery path)
	if serverCfg, ok := d.config.ResourceConfigs[resourceURL]; ok {
		if serverCfg.AuthorizationEndpoint != "" {
			authorizationEndpoint = serverCfg.AuthorizationEndpoint
		}
		if serverCfg.TokenEndpoint != "" {
			tokenEndpoint = serverCfg.TokenEndpoint
		}
	}

	// Resolve scopes: ResourceConfigs > ResourceScopes > discovered
	scopes := metadata.ScopesSupported
	if serverCfg, ok := d.config.ResourceConfigs[resourceURL]; ok && len(serverCfg.Scopes) > 0 {
		d.logger.Info("Using configured scopes for resource server (overriding discovered)",
			"resource_url", resourceURL,
			"discovered_scopes", scopes,
			"configured_scopes", serverCfg.Scopes)
		scopes = serverCfg.Scopes
	} else if override, ok := d.config.ResourceScopes[resourceURL]; ok {
		d.logger.Info("Using legacy configured scopes for resource server (overriding discovered)",
			"resource_url", resourceURL,
			"discovered_scopes", scopes,
			"configured_scopes", override)
		scopes = override
	}

	d.logger.Info("OAuth endpoints and scopes resolved",
		"resource_url", resourceURL,
		"authorization_endpoint", authorizationEndpoint,
		"token_endpoint", tokenEndpoint,
		"scopes", scopes)

	return authorizationEndpoint, tokenEndpoint, scopes, nil
}

// SendSyntheticRequest sends an unauthenticated resource request to trigger OAuth elicitation.
// This is optional and used for validation purposes.
// Returns true if the server responds with 401, false otherwise.
func (d *Discoverer) SendSyntheticRequest(ctx context.Context, resourceURL string) (bool, error) {
	// Build synthetic tools/list request
	requestBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params":  map[string]interface{}{},
	}

	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return false, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Send POST request to /mcp endpoint without Authorization header
	resourceEndpoint := strings.TrimSuffix(resourceURL, "/") + "/mcp"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resourceEndpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	d.logger.Debug("sending synthetic resource request",
		"endpoint", resourceEndpoint,
		"method", "tools/list")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response body for logging
	body, _ := io.ReadAll(resp.Body)

	d.logger.Debug("synthetic resource request response",
		"status", resp.StatusCode,
		"www_authenticate", resp.Header.Get("WWW-Authenticate"),
		"body_length", len(body))

	// Check if we got 401 Unauthorized
	if resp.StatusCode == http.StatusUnauthorized {
		d.logger.Info("OAuth elicitation triggered (401 response)",
			"resource_url", resourceURL)
		return true, nil
	}

	// If we got a different status, OAuth might not be required
	d.logger.Warn("expected 401 but got different status",
		"status", resp.StatusCode,
		"resource_url", resourceURL)

	return false, nil
}
