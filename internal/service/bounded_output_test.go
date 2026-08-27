package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeDiagnosticRedactsCommonSecretFormats(t *testing.T) {
	canaries := testDiagnosticSecretCanaries()
	input := strings.Join([]string{
		"Authorization: Bearer " + canaries.github,
		"Authorization: Basic " + canaries.basic,
		"AWS_ACCESS_KEY_ID=" + canaries.awsAccessKey,
		"GITHUB_TOKEN=" + canaries.github,
		"gitlab=" + canaries.gitlab,
		"jwt=" + canaries.jwt,
		"clone " + canaries.credentialURL,
		canaries.pemBegin,
		canaries.pemBody,
		canaries.pemEnd,
	}, "\n")

	sanitized := sanitizeDiagnostic(input)
	for _, secret := range canaries.values() {
		assert.NotContains(t, sanitized, secret)
	}
	assert.Contains(t, sanitized, "[REDACTED]")
	assert.Contains(t, sanitized, "[REDACTED PRIVATE KEY]")
}
