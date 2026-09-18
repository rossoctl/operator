// Package oauth provides OAuth utilities including PKCE support.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCEChallenge represents a PKCE challenge for OAuth 2.0.
type PKCEChallenge struct {
	Verifier  string
	Challenge string
	Method    string
}

// GeneratePKCEChallenge generates a PKCE code verifier and challenge.
// This implements RFC 7636 for enhanced security in OAuth flows.
func GeneratePKCEChallenge() (*PKCEChallenge, error) {
	// Generate 32 random bytes for the verifier
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("failed to generate random verifier: %w", err)
	}

	// Base64url encode the verifier (without padding)
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	// Create SHA256 hash of the verifier
	hash := sha256.Sum256([]byte(verifier))

	// Base64url encode the challenge (without padding)
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])

	return &PKCEChallenge{
		Verifier:  verifier,
		Challenge: challenge,
		Method:    "S256",
	}, nil
}

// GenerateState generates a cryptographically secure random state parameter.
func GenerateState() (string, error) {
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", fmt.Errorf("failed to generate random state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(stateBytes), nil
}
