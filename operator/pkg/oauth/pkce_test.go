package oauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeneratePKCEChallenge(t *testing.T) {
	t.Parallel()

	pkce, err := GeneratePKCEChallenge()
	require.NoError(t, err)
	require.NotNil(t, pkce)

	// Verify verifier is not empty
	assert.NotEmpty(t, pkce.Verifier)

	// Verify challenge is not empty
	assert.NotEmpty(t, pkce.Challenge)

	// Verify method is S256
	assert.Equal(t, "S256", pkce.Method)

	// Verify verifier and challenge are different
	assert.NotEqual(t, pkce.Verifier, pkce.Challenge)

	// Verify they are base64url encoded (no padding)
	assert.NotContains(t, pkce.Verifier, "=")
	assert.NotContains(t, pkce.Challenge, "=")
}

func TestGenerateState(t *testing.T) {
	t.Parallel()

	state, err := GenerateState()
	require.NoError(t, err)
	assert.NotEmpty(t, state)

	// Verify it's base64url encoded (no padding)
	assert.NotContains(t, state, "=")

	// Generate another state and verify they're different
	state2, err := GenerateState()
	require.NoError(t, err)
	assert.NotEqual(t, state, state2)
}
