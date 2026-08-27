package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCoverageEvidenceAcceptsOnlyCompleteExplicitReports(t *testing.T) {
	tests := []struct {
		name    string
		format  string
		content string
		want    float64
	}{
		{
			name:   "go cover",
			format: "go-cover",
			content: "mode: atomic\n" +
				"main.go:1.1,2.1 3 1\n" +
				"main.go:3.1,4.1 1 0\n",
			want: 75,
		},
		{
			name:    "cobertura",
			format:  "cobertura",
			content: `<?xml version="1.0"?><coverage line-rate="0.825"><packages/></coverage>`,
			want:    82.5,
		},
		{
			name:    "jacoco report counter",
			format:  "jacoco",
			content: `<report name="unit"><counter type="LINE" missed="100" covered="0"/><counter type="INSTRUCTION" missed="2" covered="8"/></report>`,
			want:    80,
		},
		{
			name:    "istanbul",
			format:  "istanbul",
			content: `{"src/a.ts":{"s":{"0":1,"1":0}},"src/b.ts":{"s":{"0":2,"1":3}}}`,
			want:    75,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeCoverageFile(t, root, "reports/coverage.data", []byte(tt.content))
			got, err := parseCoverageEvidence("reports/coverage.data", tt.format, root)
			require.NoError(t, err)
			assert.InDelta(t, tt.want, got, 0.0001)
		})
	}
}

func TestParseCoverageEvidenceRejectsMissingUnsafeAndUnboundedFiles(t *testing.T) {
	root := t.TempDir()
	writeCoverageFile(t, root, "valid.out", []byte("mode: set\nmain.go:1.1,2.1 1 1\n"))
	writeCoverageFile(t, root, "empty.out", nil)
	writeCoverageFile(t, root, "invalid-utf8.out", []byte{0xff, 0xfe})
	require.NoError(t, os.Mkdir(filepath.Join(root, "directory.out"), 0o700))

	outside := filepath.Join(t.TempDir(), "outside.out")
	require.NoError(t, os.WriteFile(outside, []byte("mode: set\nx.go:1.1,2.1 1 1\n"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "linked.out")))

	large := filepath.Join(root, "large.out")
	largeFile, err := os.OpenFile(large, os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, largeFile.Truncate(maxValidationFileBytes+1))
	require.NoError(t, largeFile.Close())

	tests := []struct {
		name   string
		path   string
		format string
	}{
		{name: "unknown parser", path: "valid.out", format: "auto"},
		{name: "missing", path: "missing.out", format: "go-cover"},
		{name: "traversal", path: "../outside.out", format: "go-cover"},
		{name: "absolute", path: outside, format: "go-cover"},
		{name: "protected", path: ".git/config", format: "go-cover"},
		{name: "symlink", path: "linked.out", format: "go-cover"},
		{name: "empty", path: "empty.out", format: "go-cover"},
		{name: "directory", path: "directory.out", format: "go-cover"},
		{name: "too large", path: "large.out", format: "go-cover"},
		{name: "invalid utf8", path: "invalid-utf8.out", format: "go-cover"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCoverageEvidence(tt.path, tt.format, root)
			require.Error(t, err)
		})
	}
}

func TestParseGoCoverStrictRejectsPartialOrAmbiguousEvidence(t *testing.T) {
	valid, err := parseGoCoverStrict("\nmode: set\n\nmain.go:1.1,2.1 1 0\n")
	require.NoError(t, err)
	assert.Equal(t, 0.0, valid)

	invalid := []string{
		"",
		"mode: unknown\nmain.go:1.1,2.1 1 1\n",
		"mode: set\n",
		"mode: set\nbad record\n",
		"mode: set\nmain.go 1 1\n",
		"mode: set\nmain.go:1.1,2.1 zero 1\n",
		"mode: set\nmain.go:1.1,2.1 0 1\n",
		"mode: set\nmain.go:1.1,2.1 1 negative\n",
		"mode: set\nmain.go:1.1,2.1 1 -1\n",
		"mode: set\nmain.go:1.1,2.1 9223372036854775807 1\nmain.go:3.1,4.1 1 1\n",
		"mode: set\n" + strings.Repeat("x", 1024*1024+1),
	}
	for i, content := range invalid {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			_, err := parseGoCoverStrict(content)
			require.Error(t, err)
		})
	}
}

func TestParseCoberturaStrictFailsClosed(t *testing.T) {
	for _, rate := range []string{"0", "1", "0.125"} {
		got, err := parseCoberturaStrict([]byte(`<coverage line-rate="` + rate + `"><package/></coverage>`))
		require.NoError(t, err)
		if rate == "0.125" {
			assert.Equal(t, 12.5, got)
		}
	}

	tooDeep := `<coverage line-rate="1">` + strings.Repeat("<x>", 128) + strings.Repeat("</x>", 128) + `</coverage>`
	invalid := []string{
		`<!DOCTYPE coverage><coverage line-rate="1"/>`,
		`<!ENTITY x "bad"><coverage line-rate="1"/>`,
		`<report line-rate="1"/>`,
		`<coverage/>`,
		`<coverage line-rate="nan"/>`,
		`<coverage line-rate="1.1"/>`,
		`<coverage line-rate="-0.1"/>`,
		`<coverage line-rate="nope"/>`,
		`<coverage line-rate="1">`,
		tooDeep,
	}
	for _, content := range invalid {
		_, err := parseCoberturaStrict([]byte(content))
		require.Error(t, err, content)
	}
}

func TestParseJacocoStrictRequiresOneReportLevelInstructionCounter(t *testing.T) {
	got, err := parseJacocoStrict([]byte(`<report><counter type="INSTRUCTION" missed="1" covered="3"/></report>`))
	require.NoError(t, err)
	assert.Equal(t, 75.0, got)

	tooDeep := `<report>` + strings.Repeat("<x>", 128) + strings.Repeat("</x>", 128) + `</report>`
	invalid := []string{
		`<!DOCTYPE report><report><counter type="INSTRUCTION" missed="0" covered="1"/></report>`,
		`<coverage><counter type="INSTRUCTION" missed="0" covered="1"/></coverage>`,
		`<report/>`,
		`<report><package><counter type="INSTRUCTION" missed="0" covered="1"/></package></report>`,
		`<report><counter type="LINE" missed="0" covered="1"/></report>`,
		`<report><counter type="INSTRUCTION" missed="-1" covered="1"/></report>`,
		`<report><counter type="INSTRUCTION" missed="x" covered="1"/></report>`,
		`<report><counter type="INSTRUCTION" missed="1" covered="-1"/></report>`,
		`<report><counter type="INSTRUCTION" missed="1" covered="x"/></report>`,
		`<report><counter type="INSTRUCTION" missed="0" covered="0"/></report>`,
		`<report>`,
		tooDeep,
	}
	for _, content := range invalid {
		_, err := parseJacocoStrict([]byte(content))
		require.Error(t, err, content)
	}
}

func TestParseIstanbulStrictRejectsIncompleteAndTrailingJSON(t *testing.T) {
	got, err := parseIstanbulStrict([]byte(`{"a.ts":{"s":{"0":1,"1":0}}}`))
	require.NoError(t, err)
	assert.Equal(t, 50.0, got)

	invalid := []string{
		``,
		`not-json`,
		`{}`,
		`{"a.ts":{}}`,
		`{"a.ts":{"s":{}}}`,
		`{"a.ts":{"s":{"0":-1}}}`,
		`{"a.ts":{"s":{"0":1}}} {}`,
		`{"a.ts":{"s":{"0":1}}} trailing`,
	}
	for _, content := range invalid {
		_, err := parseIstanbulStrict([]byte(content))
		require.Error(t, err, content)
	}
}

func writeCoverageFile(t *testing.T, root, relative string, content []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, content, 0o600))
}
