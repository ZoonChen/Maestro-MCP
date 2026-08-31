package webhook

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPayloadCipherRoundTrip(t *testing.T) {
	cipher, err := NewPayloadCipher("test-key-material")
	require.NoError(t, err)

	body := []byte(`{"project_id":42,"object_kind":"push"}`)
	sealed, err := cipher.Seal(body)
	require.NoError(t, err)

	opened, err := cipher.Open(sealed)
	require.NoError(t, err)
	assert.Equal(t, body, opened)
}

func TestPayloadCipherFailsClosed(t *testing.T) {
	cipher, err := NewPayloadCipher("test-key-material")
	require.NoError(t, err)

	_, err = NewPayloadCipher("")
	assert.Error(t, err, "empty key material must not construct a cipher")

	sealed, err := cipher.Seal([]byte("secret body"))
	require.NoError(t, err)

	_, err = cipher.Open(sealed)
	require.NoError(t, err)

	// Tampered ciphertext must fail closed.
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0x01
	_, err = cipher.Open(tampered)
	assert.ErrorIs(t, err, ErrCipherSealed)

	// Truncated storage fails closed.
	_, err = cipher.Open(sealed[:5])
	assert.ErrorIs(t, err, ErrCipherSealed)

	// A different key cannot open another key's ciphertext.
	other, err := NewPayloadCipher("different-key")
	require.NoError(t, err)
	_, err = other.Open(sealed)
	assert.ErrorIs(t, err, ErrCipherSealed)
}

func TestPayloadCipherDistinctNonces(t *testing.T) {
	cipher, err := NewPayloadCipher("test-key-material")
	require.NoError(t, err)

	first, err := cipher.Seal([]byte("same payload"))
	require.NoError(t, err)
	second, err := cipher.Seal([]byte("same payload"))
	require.NoError(t, err)

	assert.NotEqual(t, first, second, "identical payloads must not share ciphertext")
	assert.False(t, bytes.Equal(first[:12], second[:12]), "nonces must differ")
}

func TestVerifyToken(t *testing.T) {
	assert.True(t, VerifyToken("presented", "presented"))
	assert.False(t, VerifyToken("presented", "secret"))
	assert.False(t, VerifyToken("", "secret"), "an absent token never verifies")
	assert.False(t, VerifyToken("secret", ""), "an unset secret never verifies")
}

func TestEnvSecretResolver(t *testing.T) {
	t.Setenv("MAESTRO_GITLAB_WEBHOOK_SECRET_TEST", "value-a")
	resolver := EnvSecretResolver{}

	value, err := resolver.Resolve(t.Context(), "env:MAESTRO_GITLAB_WEBHOOK_SECRET_TEST")
	require.NoError(t, err)
	assert.Equal(t, "value-a", value)

	_, err = resolver.Resolve(t.Context(), "MAESTRO_GITLAB_WEBHOOK_SECRET_TEST")
	assert.Error(t, err, "refs must be env:-prefixed")

	_, err = resolver.Resolve(t.Context(), "env:PATH")
	assert.Error(t, err, "refs outside the MAESTRO_ namespace are rejected")

	_, err = resolver.Resolve(t.Context(), "env:MAESTRO_MISSING_SECRET_XYZ")
	assert.Error(t, err, "unset variables fail closed")
}
