package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Audience represents the JWT audience claim which can be either a string or array of strings.
// Per RFC 7519, the "aud" claim can be a single string or an array of strings.
type Audience []string

// UnmarshalJSON implements custom unmarshaling to handle both string and array formats.
func (a *Audience) UnmarshalJSON(data []byte) error {
	// Try to unmarshal as a string first
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = Audience{single}
		return nil
	}

	// If that fails, try to unmarshal as an array
	var multiple []string
	if err := json.Unmarshal(data, &multiple); err != nil {
		return fmt.Errorf("audience must be a string or array of strings: %w", err)
	}
	*a = Audience(multiple)
	return nil
}

// Contains checks if the audience list contains a specific value.
func (a Audience) Contains(value string) bool {
	for _, aud := range a {
		if aud == value {
			return true
		}
	}
	return false
}

// String returns the first audience value, or empty string if none.
func (a Audience) String() string {
	if len(a) > 0 {
		return a[0]
	}
	return ""
}

// JWTClaims represents the JWT claims we extract from the token.
// Phase 1: We do NOT validate the JWT signature - we trust the claims.
// This is acceptable for demo/testing but NOT production-ready.
type JWTClaims struct {
	Sub        string   `json:"sub"`         // Subject - User ID
	Jti        string   `json:"jti"`         // JWT ID - Session Key (fallback)
	SessionUID string   `json:"session_uid"` // Session UID - Session Key (preferred)
	Iss        string   `json:"iss"`         // Issuer
	Aud        Audience `json:"aud"`         // Audience (can be string or array)
	Exp        int64    `json:"exp"`         // Expiration time
	Iat        int64    `json:"iat"`         // Issued at
}

// GetSessionKey returns the session key, preferring session_uid over jti.
func (c *JWTClaims) GetSessionKey() string {
	if c.SessionUID != "" {
		return c.SessionUID
	}
	return c.Jti
}

// ParseJWTWithoutValidation parses a JWT token and extracts claims WITHOUT validating the signature.
//
// WARNING: This function does NOT validate the JWT signature. It trusts the claims as-is.
// This is acceptable for demo/testing environments but NOT production-ready.
//
// Security considerations:
// - An attacker could forge JWTs with arbitrary claims
// - Deploy in trusted network only
// - Use network policies to restrict Token Broker access
//
// Future work (Phase 2+):
// - Implement JWT signature validation using JWKS
// - Fetch public keys from IdP's /.well-known/openid-configuration
// - Validate issuer, audience, expiration
//
// Parameters:
//   - token: The JWT token string (without "Bearer " prefix)
//
// Returns:
//   - *JWTClaims: Parsed claims containing sub (user ID) and jti (session key)
//   - error: If the token is malformed or missing required claims
func ParseJWTWithoutValidation(token string) (*JWTClaims, error) {
	// JWT format: header.payload.signature
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	// Decode payload (second part)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to decode JWT payload: %w", err)
	}

	// Parse claims
	var claims JWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("failed to parse JWT claims: %w", err)
	}

	// Validate required claims are present
	if claims.Sub == "" {
		return nil, fmt.Errorf("missing required claim: sub (user ID)")
	}

	// Require either session_uid or jti
	if claims.SessionUID == "" && claims.Jti == "" {
		return nil, fmt.Errorf("missing required claim: session_uid or jti (session key)")
	}

	return &claims, nil
}

// ExtractBearerToken extracts the token from an Authorization header.
// Expected format: "Bearer <token>"
//
// Parameters:
//   - authHeader: The Authorization header value
//
// Returns:
//   - string: The extracted token (without "Bearer " prefix)
//   - error: If the header is missing or malformed
func ExtractBearerToken(authHeader string) (string, error) {
	if authHeader == "" {
		return "", fmt.Errorf("missing Authorization header")
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return "", fmt.Errorf("invalid Authorization header format: expected 'Bearer <token>'")
	}

	token := strings.TrimPrefix(authHeader, prefix)
	if token == "" {
		return "", fmt.Errorf("empty token in Authorization header")
	}

	return token, nil
}
