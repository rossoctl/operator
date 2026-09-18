/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfig_Defaults(t *testing.T) {
	clearEnv()

	cfg := LoadConfig()

	if cfg.ListenPort != 8190 {
		t.Errorf("Expected default ListenPort 8190, got %d", cfg.ListenPort)
	}

	if cfg.SessionTimeout != 60*time.Second {
		t.Errorf("Expected default SessionTimeout 60s, got %v", cfg.SessionTimeout)
	}

	if cfg.MaxSessionsPerUser != 5 {
		t.Errorf("Expected default MaxSessionsPerUser 5, got %d", cfg.MaxSessionsPerUser)
	}

	if cfg.TokenWaitTimeout != 300*time.Second {
		t.Errorf("Expected default TokenWaitTimeout 300s, got %v", cfg.TokenWaitTimeout)
	}

	if cfg.AuthorizationEndpoint != "" {
		t.Errorf("Expected empty AuthorizationEndpoint, got %s", cfg.AuthorizationEndpoint)
	}

	if cfg.TokenEndpoint != "" {
		t.Errorf("Expected empty TokenEndpoint, got %s", cfg.TokenEndpoint)
	}

	if len(cfg.ScopesSupported) != 0 {
		t.Errorf("Expected empty ScopesSupported, got %v", cfg.ScopesSupported)
	}
}

func TestLoadConfig_OAuthMetadata(t *testing.T) {
	clearEnv()

	_ = os.Setenv("OAUTH_CLIENT_ID", "test-client-id")
	_ = os.Setenv("OAUTH_CLIENT_SECRET", "test-client-secret")
	_ = os.Setenv("OAUTH_CALLBACK_URL", "http://localhost:8190/oauth/callback")
	_ = os.Setenv("OAUTH_AUTHORIZATION_ENDPOINT", "https://github.com/login/oauth/authorize")
	_ = os.Setenv("OAUTH_TOKEN_ENDPOINT", "https://github.com/login/oauth/access_token")
	_ = os.Setenv("OAUTH_SCOPES_SUPPORTED", "repo,user,read:org")
	defer clearEnv()

	cfg := LoadConfig()

	if cfg.ClientID != "test-client-id" {
		t.Errorf("Expected ClientID 'test-client-id', got %s", cfg.ClientID)
	}

	if cfg.ClientSecret != "test-client-secret" {
		t.Errorf("Expected ClientSecret 'test-client-secret', got %s", cfg.ClientSecret)
	}

	if cfg.AuthorizationEndpoint != "https://github.com/login/oauth/authorize" {
		t.Errorf("Expected AuthorizationEndpoint, got %s", cfg.AuthorizationEndpoint)
	}

	if cfg.TokenEndpoint != "https://github.com/login/oauth/access_token" {
		t.Errorf("Expected TokenEndpoint, got %s", cfg.TokenEndpoint)
	}

	expectedScopes := []string{"repo", "user", "read:org"}
	if len(cfg.ScopesSupported) != len(expectedScopes) {
		t.Fatalf("Expected %d scopes, got %d", len(expectedScopes), len(cfg.ScopesSupported))
	}

	for i, scope := range expectedScopes {
		if cfg.ScopesSupported[i] != scope {
			t.Errorf("Expected scope[%d] '%s', got '%s'", i, scope, cfg.ScopesSupported[i])
		}
	}
}

func TestLoadConfig_ScopesWithSpaces(t *testing.T) {
	clearEnv()

	_ = os.Setenv("OAUTH_CLIENT_ID", "id")
	_ = os.Setenv("OAUTH_CLIENT_SECRET", "secret")
	_ = os.Setenv("OAUTH_SCOPES_SUPPORTED", "  repo  ,  user  ,  read:org  ")
	defer clearEnv()

	cfg := LoadConfig()

	expectedScopes := []string{"repo", "user", "read:org"}
	if len(cfg.ScopesSupported) != len(expectedScopes) {
		t.Fatalf("Expected %d scopes, got %d", len(expectedScopes), len(cfg.ScopesSupported))
	}

	for i, scope := range expectedScopes {
		if cfg.ScopesSupported[i] != scope {
			t.Errorf("Expected scope[%d] '%s', got '%s'", i, scope, cfg.ScopesSupported[i])
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name        string
		config      *Config
		expectError bool
	}{
		{
			name: "valid config",
			config: &Config{
				ListenPort:         8190,
				ClientID:           "test-client-id",
				ClientSecret:       "test-client-secret",
				CallbackURL:        "http://localhost:8190/oauth/callback",
				SessionTimeout:     60 * time.Second,
				MaxSessionsPerUser: 5,
				TokenWaitTimeout:   300 * time.Second,
			},
			expectError: false,
		},
		{
			name: "missing client ID",
			config: &Config{
				ListenPort:         8190,
				ClientSecret:       "test-client-secret",
				CallbackURL:        "http://localhost:8190/oauth/callback",
				SessionTimeout:     60 * time.Second,
				MaxSessionsPerUser: 5,
				TokenWaitTimeout:   300 * time.Second,
			},
			expectError: true,
		},
		{
			name: "missing client secret",
			config: &Config{
				ListenPort:         8190,
				ClientID:           "test-client-id",
				CallbackURL:        "http://localhost:8190/oauth/callback",
				SessionTimeout:     60 * time.Second,
				MaxSessionsPerUser: 5,
				TokenWaitTimeout:   300 * time.Second,
			},
			expectError: true,
		},
		{
			name: "invalid port",
			config: &Config{
				ListenPort:         0,
				ClientID:           "test-client-id",
				ClientSecret:       "test-client-secret",
				CallbackURL:        "http://localhost:8190/oauth/callback",
				SessionTimeout:     60 * time.Second,
				MaxSessionsPerUser: 5,
				TokenWaitTimeout:   300 * time.Second,
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.expectError && err == nil {
				t.Error("Expected error but got none")
			}
			if !tt.expectError && err != nil {
				t.Errorf("Expected no error but got: %v", err)
			}
		})
	}
}

func clearEnv() {
	_ = os.Unsetenv("TOKEN_BROKER_PORT")
	_ = os.Unsetenv("OAUTH_CLIENT_ID")
	_ = os.Unsetenv("OAUTH_CLIENT_SECRET")
	_ = os.Unsetenv("OAUTH_CALLBACK_URL")
	_ = os.Unsetenv("TOKEN_BROKER_SESSION_TIMEOUT")
	_ = os.Unsetenv("TOKEN_BROKER_MAX_SESSIONS_PER_USER")
	_ = os.Unsetenv("TOKEN_BROKER_TOKEN_WAIT_TIMEOUT")
	_ = os.Unsetenv("OAUTH_AUTHORIZATION_ENDPOINT")
	_ = os.Unsetenv("OAUTH_TOKEN_ENDPOINT")
	_ = os.Unsetenv("OAUTH_SCOPES_SUPPORTED")
}
