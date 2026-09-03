package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v3"
)

// The two source forms are one contract: a YAML rendering of a JSON
// fixture must parse, canonicalize and hash identically to the JSON.
func TestYAMLAndJSONShareIdentity(t *testing.T) {
	cases, err := filepath.Glob("../../tests/fixtures/openapi-golden/*/base.json")
	require.NoError(t, err)
	require.NotEmpty(t, cases)

	for _, jsonPath := range cases {
		raw, err := os.ReadFile(jsonPath)
		require.NoError(t, err, jsonPath)

		var generic map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		require.NoError(t, decoder.Decode(&generic), jsonPath)

		// yaml.v3 renders json.Number as a bare scalar; quoting makes
		// round-trip number handling explicit.
		yamlBytes, err := yaml.Marshal(generic)
		require.NoError(t, err, jsonPath)

		jsonDoc, err := ParseDocument(raw)
		require.NoError(t, err, jsonPath)
		yamlDoc, err := ParseDocument(yamlBytes)
		require.NoError(t, err, jsonPath)

		assert.Equal(t, FormatJSON, DetectFormat(raw))
		assert.Equal(t, FormatYAML, DetectFormat(yamlBytes))
		assert.Equal(t, jsonDoc.CanonicalHash, yamlDoc.CanonicalHash,
			"%s: both forms must hash identically", filepath.Base(filepath.Dir(jsonPath)))
	}
}

// YAML failure modes stay fail-closed like JSON's.
func TestParseDocumentYAMLFailsClosed(t *testing.T) {
	for name, source := range map[string]string{
		"scalar root":    `just a string`,
		"sequence root":  `- a\n- b\n`,
		"broken yaml":    "openapi: 3.0.0\npaths: [unclosed",
		"wrong version":  "openapi: 2.0\npaths: {}\n",
		"missing paths":  "openapi: 3.0.0\n",
		"path without /": "openapi: 3.0.0\npaths:\n  users:\n    get:\n      responses: {}\n",
	} {
		_, err := ParseDocument([]byte(source))
		assert.Error(t, err, name)
	}
}
