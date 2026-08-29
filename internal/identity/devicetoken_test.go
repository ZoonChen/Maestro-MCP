package identity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeviceTokenMintVerifyRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	minter, err := NewDeviceTokenMinter("0123456789abcdef0123456789abcdef", func() time.Time { return now })
	require.NoError(t, err)

	token, expiry, err := minter.Mint("runner-1")
	require.NoError(t, err)
	assert.True(t, expiry.After(now))

	subject, err := minter.Verify(token)
	require.NoError(t, err)
	assert.Equal(t, "runner-1", subject)

	// Tampered payload and foreign tokens fail.
	_, err = minter.Verify(token + "x")
	assert.Error(t, err)
	other, _ := NewDeviceTokenMinter("fedcba9876543210fedcba9876543210", func() time.Time { return now })
	foreign, _, _ := other.Mint("runner-1")
	_, err = minter.Verify(foreign)
	assert.Error(t, err)

	// Expiry is enforced with the minter's clock.
	later, _ := NewDeviceTokenMinter("0123456789abcdef0123456789abcdef", func() time.Time {
		return now.Add(25 * time.Hour)
	})
	_, err = later.Verify(token)
	assert.Error(t, err)
}

func TestDeviceTokenSecretFloor(t *testing.T) {
	_, err := NewDeviceTokenMinter("short", nil)
	assert.Error(t, err)
}

func TestEnrollmentCodeHashing(t *testing.T) {
	code, codeHash, err := NewEnrollmentCode()
	require.NoError(t, err)
	assert.Len(t, code, 32)
	assert.Equal(t, HashEnrollmentCode(code), codeHash)
	assert.NotEqual(t, HashEnrollmentCode("different"), codeHash)
}
