package oauth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoverEndpoints_WithConfiguredMetadata(t *testing.T) {
	logger := slog.Default()

	// Create discoverer with configured metadata
	config := &DiscovererConfig{
		AuthorizationEndpoint: "https://configured.example.com/oauth/authorize",
		TokenEndpoint:         "https://configured.example.com/oauth/token",
		ScopesSupported:       []string{"read", "write"},
	}
	discoverer := NewDiscovererWithConfig(config, logger)

	// Call DiscoverEndpoints - should use configured values without making HTTP request
	ctx := context.Background()
	authEndpoint, tokenEndpoint, scopes, err := discoverer.DiscoverEndpoints(ctx, "https://mcp.example.com")

	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if authEndpoint != config.AuthorizationEndpoint {
		t.Errorf("Expected authEndpoint '%s', got '%s'", config.AuthorizationEndpoint, authEndpoint)
	}

	if tokenEndpoint != config.TokenEndpoint {
		t.Errorf("Expected tokenEndpoint '%s', got '%s'", config.TokenEndpoint, tokenEndpoint)
	}

	if len(scopes) != len(config.ScopesSupported) {
		t.Errorf("Expected %d scopes, got %d", len(config.ScopesSupported), len(scopes))
	}

	for i, scope := range config.ScopesSupported {
		if i >= len(scopes) || scopes[i] != scope {
			t.Errorf("Expected scope[%d] '%s', got '%s'", i, scope, scopes[i])
		}
	}
}

func TestDiscoverEndpoints_WithoutConfiguredMetadata_Success(t *testing.T) {
	logger := slog.Default()

	// Create test server that returns discovery metadata
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource" {
			t.Errorf("Expected path '/.well-known/oauth-protected-resource', got '%s'", r.URL.Path)
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		response := map[string]interface{}{
			"authorization_servers": []string{"https://github.com/login/oauth"},
			"scopes_supported":      []string{"repo", "user", "read:org"},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Create discoverer without configured metadata
	discoverer := NewDiscoverer(logger)

	// Call DiscoverEndpoints - should make HTTP request
	ctx := context.Background()
	authEndpoint, tokenEndpoint, scopes, err := discoverer.DiscoverEndpoints(ctx, server.URL)

	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	expectedAuthEndpoint := "https://github.com/login/oauth/authorize"
	if authEndpoint != expectedAuthEndpoint {
		t.Errorf("Expected authEndpoint '%s', got '%s'", expectedAuthEndpoint, authEndpoint)
	}

	expectedTokenEndpoint := "https://github.com/login/oauth/access_token"
	if tokenEndpoint != expectedTokenEndpoint {
		t.Errorf("Expected tokenEndpoint '%s', got '%s'", expectedTokenEndpoint, tokenEndpoint)
	}

	expectedScopes := []string{"repo", "user", "read:org"}
	if len(scopes) != len(expectedScopes) {
		t.Errorf("Expected %d scopes, got %d", len(expectedScopes), len(scopes))
	}

	for i, scope := range expectedScopes {
		if i >= len(scopes) || scopes[i] != scope {
			t.Errorf("Expected scope[%d] '%s', got '%s'", i, scope, scopes[i])
		}
	}
}

func TestDiscoverEndpoints_PartialConfiguredMetadata(t *testing.T) {
	logger := slog.Default()

	// Create test server that returns discovery metadata
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"authorization_servers": []string{"https://github.com/login/oauth"},
			"scopes_supported":      []string{"repo", "user"},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Create discoverer with only authorization endpoint configured (missing token endpoint)
	config := &DiscovererConfig{
		AuthorizationEndpoint: "https://configured.example.com/oauth/authorize",
		// TokenEndpoint is missing - should trigger discovery
	}
	discoverer := NewDiscovererWithConfig(config, logger)

	// Call DiscoverEndpoints - should make HTTP request because config is incomplete
	ctx := context.Background()
	authEndpoint, tokenEndpoint, _, err := discoverer.DiscoverEndpoints(ctx, server.URL)

	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should use discovered values, not configured ones
	expectedAuthEndpoint := "https://github.com/login/oauth/authorize"
	if authEndpoint != expectedAuthEndpoint {
		t.Errorf("Expected authEndpoint '%s', got '%s'", expectedAuthEndpoint, authEndpoint)
	}

	expectedTokenEndpoint := "https://github.com/login/oauth/access_token"
	if tokenEndpoint != expectedTokenEndpoint {
		t.Errorf("Expected tokenEndpoint '%s', got '%s'", expectedTokenEndpoint, tokenEndpoint)
	}
}

func TestDiscoverEndpoints_DiscoveryFailure(t *testing.T) {
	logger := slog.Default()

	// Create test server that returns error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}))
	defer server.Close()

	// Create discoverer without configured metadata
	discoverer := NewDiscoverer(logger)

	// Call DiscoverEndpoints - should fail
	ctx := context.Background()
	_, _, _, err := discoverer.DiscoverEndpoints(ctx, server.URL)

	if err == nil {
		t.Fatal("Expected error, got none")
	}
}

func TestNewDiscoverer(t *testing.T) {
	logger := slog.Default()

	discoverer := NewDiscoverer(logger)

	if discoverer == nil {
		t.Fatal("Expected non-nil discoverer")
	}

	if discoverer.config == nil {
		t.Fatal("Expected non-nil config")
	}

	// Config should be empty
	if discoverer.config.AuthorizationEndpoint != "" {
		t.Errorf("Expected empty AuthorizationEndpoint, got '%s'", discoverer.config.AuthorizationEndpoint)
	}

	if discoverer.config.TokenEndpoint != "" {
		t.Errorf("Expected empty TokenEndpoint, got '%s'", discoverer.config.TokenEndpoint)
	}
}

func TestNewDiscovererWithConfig(t *testing.T) {
	logger := slog.Default()

	config := &DiscovererConfig{
		AuthorizationEndpoint: "https://example.com/oauth/authorize",
		TokenEndpoint:         "https://example.com/oauth/token",
		ScopesSupported:       []string{"read", "write"},
	}

	discoverer := NewDiscovererWithConfig(config, logger)

	if discoverer == nil {
		t.Fatal("Expected non-nil discoverer")
	}

	if discoverer.config != config {
		t.Error("Expected config to be set")
	}

	if discoverer.config.AuthorizationEndpoint != config.AuthorizationEndpoint {
		t.Errorf("Expected AuthorizationEndpoint '%s', got '%s'", config.AuthorizationEndpoint, discoverer.config.AuthorizationEndpoint)
	}
}

func TestNewDiscovererWithConfig_NilConfig(t *testing.T) {
	logger := slog.Default()

	discoverer := NewDiscovererWithConfig(nil, logger)

	if discoverer == nil {
		t.Fatal("Expected non-nil discoverer")
	}

	if discoverer.config == nil {
		t.Fatal("Expected non-nil config (should be initialized to empty)")
	}

	// Config should be empty
	if discoverer.config.AuthorizationEndpoint != "" {
		t.Errorf("Expected empty AuthorizationEndpoint, got '%s'", discoverer.config.AuthorizationEndpoint)
	}
}
