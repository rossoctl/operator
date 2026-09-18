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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/jwtvalidation/validation"
	"github.com/rossoctl/operator/internal/tokenbroker/api"
	"github.com/rossoctl/operator/internal/tokenbroker/cache"
	"github.com/rossoctl/operator/internal/tokenbroker/core"
	"github.com/rossoctl/operator/internal/tokenbroker/oauth"
	"github.com/rossoctl/operator/internal/tokenbroker/session"
)

// Config holds the Token Broker configuration.
type Config struct {
	ListenPort         int
	ClientID           string
	ClientSecret       string
	CallbackURL        string
	SessionTimeout     time.Duration
	MaxSessionsPerUser int
	TokenWaitTimeout   time.Duration

	// OAuth metadata (optional - if not set, discovery will be used)
	AuthorizationEndpoint string
	TokenEndpoint         string
	ScopesSupported       []string

	// ResourceScopes maps resource server URL to OAuth scopes (legacy, scopes-only).
	// Format of RESOURCE_SCOPES env var: "url1=scope1 scope2,url2=scope3"
	// Deprecated: use ResourceConfigs (RESOURCE_CONFIG) for richer per-server config.
	ResourceScopes map[string][]string

	// ResourceConfigs maps resource server URL to full per-server OAuth config overrides.
	// Format of RESOURCE_CONFIG env var: JSON map of server URL to ResourceConfig.
	ResourceConfigs map[string]oauth.ResourceConfig

	// AllowedRedirectHosts is the list of permitted hostnames for backend_session_redirect_url.
	// Format of ALLOWED_REDIRECT_HOSTS env var: comma-separated hostnames.
	AllowedRedirectHosts []string

	// JWT validation (required for production).
	JWKSUrl      string
	JWTIssuer    string
	JWTAudiences []string
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		ListenPort:         8190,
		ClientID:           "",
		ClientSecret:       "",
		CallbackURL:        "http://localhost:8190/oauth/callback",
		SessionTimeout:     60 * time.Second,
		MaxSessionsPerUser: 5,
		TokenWaitTimeout:   300 * time.Second,
	}
}

// LoadConfig loads configuration from environment variables.
func LoadConfig() *Config {
	cfg := DefaultConfig()
	loadCoreConfig(cfg)
	loadOAuthDiscoveryConfig(cfg)
	loadResourceConfig(cfg)
	loadSecurityConfig(cfg)
	return cfg
}

func loadCoreConfig(cfg *Config) {
	if port := os.Getenv("TOKEN_BROKER_PORT"); port != "" {
		if _, err := fmt.Sscanf(port, "%d", &cfg.ListenPort); err != nil {
			slog.Default().Warn("invalid TOKEN_BROKER_PORT, using default",
				"value", port, "default", cfg.ListenPort, "error", err)
		}
	}
	if v := os.Getenv("OAUTH_CLIENT_ID"); v != "" {
		cfg.ClientID = v
	}
	if v := os.Getenv("OAUTH_CLIENT_SECRET"); v != "" {
		cfg.ClientSecret = v
	}
	if v := os.Getenv("OAUTH_CALLBACK_URL"); v != "" {
		cfg.CallbackURL = v
	}
	if v := os.Getenv("TOKEN_BROKER_SESSION_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.SessionTimeout = d
		}
	}
	if v := os.Getenv("TOKEN_BROKER_MAX_SESSIONS_PER_USER"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &cfg.MaxSessionsPerUser); err != nil {
			slog.Default().Warn("invalid TOKEN_BROKER_MAX_SESSIONS_PER_USER, using default",
				"value", v, "default", cfg.MaxSessionsPerUser, "error", err)
		}
	}
	if v := os.Getenv("TOKEN_BROKER_TOKEN_WAIT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.TokenWaitTimeout = d
		}
	}
}

func loadOAuthDiscoveryConfig(cfg *Config) {
	if v := os.Getenv("OAUTH_AUTHORIZATION_ENDPOINT"); v != "" {
		cfg.AuthorizationEndpoint = v
	}
	if v := os.Getenv("OAUTH_TOKEN_ENDPOINT"); v != "" {
		cfg.TokenEndpoint = v
	}
	if scopes := os.Getenv("OAUTH_SCOPES_SUPPORTED"); scopes != "" {
		for _, s := range strings.Split(scopes, ",") {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				cfg.ScopesSupported = append(cfg.ScopesSupported, trimmed)
			}
		}
	}
}

func loadResourceConfig(cfg *Config) {
	if serverScopes := os.Getenv("RESOURCE_SCOPES"); serverScopes != "" {
		cfg.ResourceScopes = make(map[string][]string)
		for _, entry := range strings.Split(serverScopes, ",") {
			parts := strings.SplitN(strings.TrimSpace(entry), "=", 2)
			if len(parts) != 2 {
				continue
			}
			serverURL := strings.TrimSpace(parts[0])
			if serverURL == "" {
				continue
			}
			cfg.ResourceScopes[serverURL] = append(cfg.ResourceScopes[serverURL], strings.Fields(parts[1])...)
		}
	}
	if serverConfig := os.Getenv("RESOURCE_CONFIG"); serverConfig != "" {
		if err := json.Unmarshal([]byte(serverConfig), &cfg.ResourceConfigs); err != nil {
			slog.Default().Warn("invalid RESOURCE_CONFIG, ignoring", "error", err)
		}
	}
}

func loadSecurityConfig(cfg *Config) {
	if allowedHosts := os.Getenv("ALLOWED_REDIRECT_HOSTS"); allowedHosts != "" {
		for _, h := range strings.Split(allowedHosts, ",") {
			if trimmed := strings.TrimSpace(h); trimmed != "" {
				cfg.AllowedRedirectHosts = append(cfg.AllowedRedirectHosts, trimmed)
			}
		}
	}
	if v := os.Getenv("JWT_JWKS_URL"); v != "" {
		cfg.JWKSUrl = v
	}
	if v := os.Getenv("JWT_ISSUER"); v != "" {
		cfg.JWTIssuer = v
	}
	if audiences := os.Getenv("JWT_AUDIENCE"); audiences != "" {
		for _, a := range strings.Split(audiences, ",") {
			if trimmed := strings.TrimSpace(a); trimmed != "" {
				cfg.JWTAudiences = append(cfg.JWTAudiences, trimmed)
			}
		}
	}
}

// Validate checks if the configuration is valid.
func (c *Config) Validate() error {
	if c.ListenPort <= 0 || c.ListenPort > 65535 {
		return fmt.Errorf("invalid listen port: %d", c.ListenPort)
	}

	if c.ClientID == "" {
		return fmt.Errorf("OAuth client ID is required (set OAUTH_CLIENT_ID)")
	}

	if c.ClientSecret == "" {
		return fmt.Errorf("OAuth client secret is required (set OAUTH_CLIENT_SECRET)")
	}

	if c.CallbackURL == "" {
		return fmt.Errorf("OAuth callback URL is required (set OAUTH_CALLBACK_URL)")
	}

	if c.SessionTimeout <= 0 {
		return fmt.Errorf("session timeout must be positive")
	}

	if c.MaxSessionsPerUser <= 0 {
		return fmt.Errorf("max sessions per user must be positive")
	}

	if c.TokenWaitTimeout <= 0 {
		return fmt.Errorf("token wait timeout must be positive")
	}

	return nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	logger.Info("Starting Token Broker service")

	cfg := LoadConfig()
	if err := cfg.Validate(); err != nil {
		logger.Error("Invalid configuration", "error", err)
		os.Exit(1)
	}

	logger.Info("Configuration loaded",
		"listen_port", cfg.ListenPort,
		"callback_url", cfg.CallbackURL,
		"session_timeout", cfg.SessionTimeout,
		"max_sessions_per_user", cfg.MaxSessionsPerUser,
		"token_wait_timeout", cfg.TokenWaitTimeout,
		"has_configured_auth_endpoint", cfg.AuthorizationEndpoint != "",
		"has_configured_token_endpoint", cfg.TokenEndpoint != "",
		"configured_scopes_count", len(cfg.ScopesSupported),
		"allowed_redirect_hosts", cfg.AllowedRedirectHosts,
		"jwt_validation_enabled", cfg.JWKSUrl != "",
		"jwt_issuer", cfg.JWTIssuer)

	if len(cfg.AllowedRedirectHosts) == 0 {
		logger.Warn("ALLOWED_REDIRECT_HOSTS is not set — all redirect hosts are permitted; set this in production")
	}
	if cfg.JWKSUrl == "" {
		logger.Warn("JWT_JWKS_URL is not set — JWT signatures are NOT validated",
			"recommendation", "set JWT_JWKS_URL, JWT_ISSUER, and JWT_AUDIENCE in production")
	}

	// Initialize components
	clock := &core.RealClock{}
	tokenCache := cache.NewTokenCache(clock)
	sessionManager := session.NewSessionManager(cfg.SessionTimeout, cfg.MaxSessionsPerUser, clock, logger)

	oauthConfig := &oauth.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
	}
	oauthClient := oauth.NewClient(oauthConfig, logger)

	discovererConfig := &oauth.DiscovererConfig{
		AuthorizationEndpoint: cfg.AuthorizationEndpoint,
		TokenEndpoint:         cfg.TokenEndpoint,
		ScopesSupported:       cfg.ScopesSupported,
		ResourceScopes:        cfg.ResourceScopes,
		ResourceConfigs:       cfg.ResourceConfigs,
	}
	discoverer := oauth.NewDiscovererWithConfig(discovererConfig, logger)

	broker := core.NewTokenBroker(
		sessionManager,
		tokenCache,
		discoverer,
		oauthClient,
		cfg.CallbackURL,
		cfg.TokenWaitTimeout,
		logger,
	)

	// Create HTTP router
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// middleware.RealIP intentionally omitted: it trusts X-Forwarded-For /
	// X-Real-IP headers (staticcheck SA1019), which can be spoofed. For a
	// security-sensitive service we keep r.RemoteAddr as the real peer address.
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(cfg.TokenWaitTimeout + 10*time.Second))

	// Health endpoints for Kubernetes probes — registered without Logger middleware
	// to avoid flooding logs with probe traffic.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// All other routes get request logging.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Logger)
		// Register API handlers
		apiHandler := api.NewHandler(broker, sessionManager, logger, cfg.AllowedRedirectHosts)
		if cfg.JWKSUrl != "" {
			verifier := validation.NewLazyJWKSVerifier(cfg.JWKSUrl, cfg.JWTIssuer)
			apiHandler.WithJWTVerifier(verifier, cfg.JWTAudiences)
		}
		apiHandler.RegisterRoutes(r)
	})

	// Create HTTP server
	addr := fmt.Sprintf(":%d", cfg.ListenPort)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: cfg.TokenWaitTimeout + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		logger.Info("Token Broker listening", "address", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				removed := tokenCache.CleanupExpiredTokens()
				if removed > 0 {
					logger.Info("Periodic token cache cleanup", "removed", removed)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	<-ctx.Done()

	logger.Info("Shutting down Token Broker")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server shutdown error", "error", err)
	}

	sessionManager.Shutdown()
	logger.Info("Token Broker stopped")
}
