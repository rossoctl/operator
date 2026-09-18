package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestParseJWTWithoutValidation(t *testing.T) {
	tests := []struct {
		name        string
		token       string
		wantSub     string
		wantJti     string
		wantErr     bool
		errContains string
	}{
		{
			name:    "valid JWT with required claims",
			token:   createTestJWT(t, map[string]interface{}{"sub": "user123", "jti": "session456"}),
			wantSub: "user123",
			wantJti: "session456",
			wantErr: false,
		},
		{
			name:    "valid JWT with all claims (string audience)",
			token:   createTestJWT(t, map[string]interface{}{"sub": "user123", "jti": "session456", "iss": "https://idp.example.com", "aud": "token-broker", "exp": 1234567890, "iat": 1234567800}),
			wantSub: "user123",
			wantJti: "session456",
			wantErr: false,
		},
		{
			name:    "valid JWT with array audience",
			token:   createTestJWT(t, map[string]interface{}{"sub": "user123", "jti": "session456", "iss": "https://idp.example.com", "aud": []string{"token-broker", "api-gateway"}, "exp": 1234567890, "iat": 1234567800}),
			wantSub: "user123",
			wantJti: "session456",
			wantErr: false,
		},
		{
			name:    "valid JWT with single-element array audience",
			token:   createTestJWT(t, map[string]interface{}{"sub": "user123", "jti": "session456", "aud": []string{"token-broker"}}),
			wantSub: "user123",
			wantJti: "session456",
			wantErr: false,
		},
		{
			name:    "valid JWT without audience claim",
			token:   createTestJWT(t, map[string]interface{}{"sub": "user123", "jti": "session456", "iss": "https://idp.example.com"}),
			wantSub: "user123",
			wantJti: "session456",
			wantErr: false,
		},
		{
			name:        "missing sub claim",
			token:       createTestJWT(t, map[string]interface{}{"jti": "session456"}),
			wantErr:     true,
			errContains: "missing required claim: sub",
		},
		{
			name:        "missing session_uid and jti claim",
			token:       createTestJWT(t, map[string]interface{}{"sub": "user123"}),
			wantErr:     true,
			errContains: "missing required claim: session_uid or jti",
		},
		{
			name:        "empty sub claim",
			token:       createTestJWT(t, map[string]interface{}{"sub": "", "jti": "session456"}),
			wantErr:     true,
			errContains: "missing required claim: sub",
		},
		{
			name:        "empty session_uid and jti claim",
			token:       createTestJWT(t, map[string]interface{}{"sub": "user123", "session_uid": "", "jti": ""}),
			wantErr:     true,
			errContains: "missing required claim: session_uid or jti",
		},
		{
			name:    "valid JWT with session_uid",
			token:   createTestJWT(t, map[string]interface{}{"sub": "user123", "session_uid": "session789"}),
			wantSub: "user123",
			wantJti: "session789",
		},
		{
			name:        "invalid JWT format - too few parts",
			token:       "header.payload",
			wantErr:     true,
			errContains: "invalid JWT format",
		},
		{
			name:        "invalid JWT format - too many parts",
			token:       "header.payload.signature.extra",
			wantErr:     true,
			errContains: "invalid JWT format",
		},
		{
			name:        "invalid base64 encoding",
			token:       "header.invalid!!!.signature",
			wantErr:     true,
			errContains: "failed to decode JWT payload",
		},
		{
			name:        "invalid JSON in payload",
			token:       "header." + base64.RawURLEncoding.EncodeToString([]byte("{invalid json")) + ".signature",
			wantErr:     true,
			errContains: "failed to parse JWT claims",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := ParseJWTWithoutValidation(tt.token)

			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseJWTWithoutValidation() expected error, got nil")
					return
				}
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Errorf("ParseJWTWithoutValidation() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("ParseJWTWithoutValidation() unexpected error = %v", err)
				return
			}

			if claims.Sub != tt.wantSub {
				t.Errorf("ParseJWTWithoutValidation() sub = %v, want %v", claims.Sub, tt.wantSub)
			}
			if claims.GetSessionKey() != tt.wantJti {
				t.Errorf("ParseJWTWithoutValidation() session_key = %v, want %v", claims.GetSessionKey(), tt.wantJti)
			}
		})
	}
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		name        string
		authHeader  string
		wantToken   string
		wantErr     bool
		errContains string
	}{
		{
			name:       "valid Bearer token",
			authHeader: "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIn0.signature", // #notsecret
			wantToken:  "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIn0.signature",         // #notsecret
			wantErr:    false,
		},
		{
			name:        "missing Authorization header",
			authHeader:  "",
			wantErr:     true,
			errContains: "missing Authorization header",
		},
		{
			name:        "invalid format - no Bearer prefix",
			authHeader:  "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIn0.signature", // #notsecret
			wantErr:     true,
			errContains: "invalid Authorization header format",
		},
		{
			name:        "invalid format - wrong prefix",
			authHeader:  "Basic dXNlcjpwYXNz",
			wantErr:     true,
			errContains: "invalid Authorization header format",
		},
		{
			name:        "empty token after Bearer",
			authHeader:  "Bearer ",
			wantErr:     true,
			errContains: "empty token",
		},
		{
			name:       "Bearer with extra spaces",
			authHeader: "Bearer  eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIn0.signature", // #notsecret
			wantToken:  " eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyMTIzIn0.signature",        // #notsecret
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := ExtractBearerToken(tt.authHeader)

			if tt.wantErr {
				if err == nil {
					t.Errorf("ExtractBearerToken() expected error, got nil")
					return
				}
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Errorf("ExtractBearerToken() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("ExtractBearerToken() unexpected error = %v", err)
				return
			}

			if token != tt.wantToken {
				t.Errorf("ExtractBearerToken() = %v, want %v", token, tt.wantToken)
			}
		})
	}
}

// Helper function to create a test JWT with given claims
func createTestJWT(t *testing.T, claims map[string]interface{}) string {
	t.Helper()

	// Create header
	header := map[string]interface{}{
		"alg": "HS256",
		"typ": "JWT",
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("failed to marshal header: %v", err)
	}
	headerEncoded := base64.RawURLEncoding.EncodeToString(headerJSON)

	// Create payload
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("failed to marshal claims: %v", err)
	}
	payloadEncoded := base64.RawURLEncoding.EncodeToString(payloadJSON)

	// Create signature (dummy - we don't validate it)
	signature := base64.RawURLEncoding.EncodeToString([]byte("dummy-signature"))

	return headerEncoded + "." + payloadEncoded + "." + signature
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
