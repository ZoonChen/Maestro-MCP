package service

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// parseGoCover
// ---------------------------------------------------------------------------

func TestParseGoCover(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    float64
	}{
		{
			name: "valid full coverage",
			content: "mode: set\n" +
				"main.go:10.1,20.2 5 1\n" +
				"main.go:30.1,40.2 3 1\n",
			want: 100.0,
		},
		{
			name: "valid partial coverage",
			content: "mode: count\n" +
				"main.go:10.1,20.2 5 1\n" +
				"main.go:30.1,40.2 3 0\n" +
				"util.go:5.1,10.2 2 1\n",
			want: 70.0, // (5+2) covered / (5+3+2) total = 7/10 = 70%
		},
		{
			name: "valid zero coverage",
			content: "mode: atomic\n" +
				"main.go:10.1,20.2 5 0\n" +
				"main.go:30.1,40.2 3 0\n",
			want: 0.0,
		},
		{
			name:    "empty input returns zero",
			content: "",
			want:    0.0,
		},
		{
			name:    "only mode line returns zero",
			content: "mode: set\n",
			want:    0.0,
		},
		{
			name: "lines with fewer than 3 fields are skipped",
			content: "mode: set\n" +
				"main.go:10.1,20.2\n" +
				"main.go:30.1,40.2 5 1\n",
			want: 100.0, // only the second valid line counts
		},
		{
			name: "non-numeric count field is skipped",
			content: "mode: set\n" +
				"main.go:10.1,20.2 5 abc\n" +
				"main.go:30.1,40.2 3 1\n",
			want: 100.0, // only the second valid line counts (3 stmts, 3 covered)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseGoCover(tt.content)
			if got != tt.want {
				t.Errorf("parseGoCover() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseCobertura
// ---------------------------------------------------------------------------

func TestParseCobertura(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    float64
	}{
		{
			name: "valid cobertura XML with line-rate",
			content: "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
				"<!DOCTYPE coverage SYSTEM \"http://cobertura.sourceforge.net/xml/coverage-04.dtd\">\n" +
				"<coverage line-rate=\"0.85\" branch-rate=\"0.7\" lines-covered=\"85\" lines-valid=\"100\">\n" +
				"  <packages>\n" +
				"    <package name=\"main\" line-rate=\"0.85\">\n" +
				"    </package>\n" +
				"  </packages>\n" +
				"</coverage>",
			want: 85.0,
		},
		{
			name:    "line-rate zero",
			content: "<coverage line-rate=\"0.0\">\n</coverage>",
			want:    0.0,
		},
		{
			name:    "line-rate 1.0 full coverage",
			content: "<coverage line-rate=\"1.0\">\n</coverage>",
			want:    100.0,
		},
		{
			name: "missing line-rate attribute returns -1",
			content: "<?xml version=\"1.0\"?>\n" +
				"<coverage branch-rate=\"0.5\">\n</coverage>",
			want: -1.0,
		},
		{
			name:    "empty content returns -1",
			content: "",
			want:    -1.0,
		},
		{
			name:    "invalid line-rate value returns -1",
			content: "<coverage line-rate=\"notanumber\">\n</coverage>",
			want:    -1.0,
		},
		{
			name:    "partial coverage 42.5 percent",
			content: "<coverage line-rate=\"0.425\">\n</coverage>",
			want:    42.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCobertura(tt.content)
			if got != tt.want {
				t.Errorf("parseCobertura() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseJacoco
// ---------------------------------------------------------------------------

func TestParseJacoco(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    float64
	}{
		{
			name: "valid jacoco with INSTRUCTION counters",
			content: "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
				"<report name=\"JaCoCo Report\">\n" +
				"  <sessioninfo id=\"session\" start=\"1234\" dump=\"5678\"/>\n" +
				"  <package name=\"main\">\n" +
				"    <class name=\"Main\">\n" +
				"      <method name=\"run\">\n" +
				"        <counter type=\"INSTRUCTION\" missed=\"10\" covered=\"90\"/>\n" +
				"      </method>\n" +
				"    </class>\n" +
				"    <counter type=\"INSTRUCTION\" missed=\"10\" covered=\"90\"/>\n" +
				"  </package>\n" +
				"  <counter type=\"INSTRUCTION\" missed=\"10\" covered=\"90\"/>\n" +
				"</report>",
			want: 90.0, // (90+90+90) / (10+90+10+90+10+90) * 100 = 270/300 = 90%
		},
		{
			name: "jacoco zero coverage",
			content: "<report name=\"test\">\n" +
				"  <counter type=\"INSTRUCTION\" missed=\"50\" covered=\"0\"/>\n" +
				"</report>",
			want: 0.0,
		},
		{
			name: "jacoco no INSTRUCTION counter returns 0",
			content: "<report name=\"test\">\n" +
				"  <counter type=\"LINE\" missed=\"5\" covered=\"10\"/>\n" +
				"  <counter type=\"BRANCH\" missed=\"2\" covered=\"3\"/>\n" +
				"</report>",
			want: 0.0, // no INSTRUCTION counters found, total=0, returns 0
		},
		{
			name: "jacoco with mixed counter types picks only INSTRUCTION",
			content: "<report name=\"test\">\n" +
				"  <counter type=\"LINE\" missed=\"100\" covered=\"0\"/>\n" +
				"  <counter type=\"INSTRUCTION\" missed=\"25\" covered=\"75\"/>\n" +
				"</report>",
			want: 75.0, // 75/(75+25)*100 = 75%
		},
		{
			name:    "empty content returns 0 (no counters found)",
			content: "",
			want:    0.0, // no counters found, total=0, returns 0
		},
		{
			name: "jacoco missing covered attr returns 0 for that line",
			content: "<report>\n" +
				"  <counter type=\"INSTRUCTION\" missed=\"10\"/>\n" +
				"</report>",
			want: 0.0, // extractIntAttr returns -1 for missing "covered", line is skipped, total=0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseJacoco(tt.content)
			if got != tt.want {
				t.Errorf("parseJacoco() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseIstanbul
// ---------------------------------------------------------------------------

func TestParseIstanbul(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    float64
	}{
		{
			name:    "valid istanbul with partial coverage",
			content: `{"src/main.go":{"s":{"0":1,"1":0,"2":1,"3":0}}}`,
			want:    50.0, // 2 covered out of 4 total
		},
		{
			name:    "istanbul full coverage",
			content: `{"src/main.go":{"s":{"0":5,"1":10,"2":1}}}`,
			want:    100.0,
		},
		{
			name:    "istanbul zero coverage all hits zero",
			content: `{"src/main.go":{"s":{"0":0,"1":0}}}`,
			want:    0.0,
		},
		{
			name:    "istanbul empty s map returns zero",
			content: `{"src/main.go":{"s":{}}}`,
			want:    0.0,
		},
		{
			name:    "istanbul multiple files aggregated",
			content: `{"src/main.go":{"s":{"0":1,"1":1}},"src/util.go":{"s":{"0":0,"1":0,"2":1}}}`,
			want:    60.0, // 3 covered out of 5 total
		},
		{
			name:    "istanbul invalid JSON returns -1",
			content: "not json",
			want:    -1.0,
		},
		{
			name:    "istanbul empty string returns -1",
			content: "",
			want:    -1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIstanbul(tt.content)
			if got != tt.want {
				t.Errorf("parseIstanbul() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseCoverage auto-detect
// ---------------------------------------------------------------------------

func TestParseCoverageAutoDetect(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		filename string
		want     float64
	}{
		{
			name:     "auto-detect go-cover format",
			content:  "mode: set\nmain.go:10.1,20.2 5 1\n",
			filename: "coverage.out",
			want:     100.0,
		},
		{
			name: "auto-detect cobertura format",
			content: "<?xml version=\"1.0\"?>\n" +
				"<coverage line-rate=\"0.75\" xmlns=\"cobertura\">\n" +
				"</coverage>",
			filename: "coverage.xml",
			want:     75.0,
		},
		{
			name: "auto-detect jacoco format",
			content: "<?xml version=\"1.0\"?>\n" +
				"<report name=\"JaCoCo\">\n" +
				"  <counter type=\"INSTRUCTION\" missed=\"20\" covered=\"80\"/>\n" +
				"</report>",
			filename: "jacoco.xml",
			want:     80.0,
		},
		{
			name:     "auto-detect istanbul format",
			content:  `{"src/main.go":{"s":{"0":1,"1":0}}}`,
			filename: "coverage-final.json",
			want:     50.0,
		},
		{
			name:     "auto-detect unrecognized format returns -1",
			content:  "some random content that is not coverage",
			filename: "unknown.txt",
			want:     -1.0,
		},
		{
			name:     "nonexistent file returns -1",
			content:  "", // empty means we won't write the file
			filename: "nonexistent_file_that_should_not_exist_12345.out",
			want:     -1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var coveragePath string

			if tt.name == "nonexistent file returns -1" {
				// Use a path that definitely does not exist.
				coveragePath = filepath.Join(t.TempDir(), tt.filename)
			} else {
				// Write content to a temp file.
				tmpDir := t.TempDir()
				coveragePath = filepath.Join(tmpDir, tt.filename)
				err := os.WriteFile(coveragePath, []byte(tt.content), 0600) //nolint:gosec // test file
				if err != nil {
					t.Fatalf("failed to write temp file: %v", err)
				}
			}

			// Pass empty format to trigger auto-detect.
			got := parseCoverage(coveragePath, "", "")
			if got != tt.want {
				t.Errorf("parseCoverage() auto-detect = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseCoverage with explicit format
// ---------------------------------------------------------------------------

func TestParseCoverageExplicitFormat(t *testing.T) {
	tests := []struct {
		name    string
		content string
		format  string
		want    float64
	}{
		{
			name:    "explicit go-cover format",
			content: "mode: count\nmain.go:10.1,20.2 4 1\nmain.go:30.1,35.2 1 0\n",
			format:  "go-cover",
			want:    80.0, // 4 covered out of 5 total
		},
		{
			name:    "explicit cobertura format",
			content: "<coverage line-rate=\"0.6\">\n</coverage>",
			format:  "cobertura",
			want:    60.0,
		},
		{
			name: "explicit jacoco format",
			content: "<report>\n" +
				"  <counter type=\"INSTRUCTION\" missed=\"30\" covered=\"70\"/>\n" +
				"</report>",
			format: "jacoco",
			want:   70.0,
		},
		{
			name:    "explicit istanbul format",
			content: `{"src/main.go":{"s":{"0":3,"1":3,"2":3,"3":0}}}`,
			format:  "istanbul",
			want:    75.0, // 3 out of 4
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			coveragePath := filepath.Join(tmpDir, "coverage.dat")
			err := os.WriteFile(coveragePath, []byte(tt.content), 0600) //nolint:gosec // test file
			if err != nil {
				t.Fatalf("failed to write temp file: %v", err)
			}

			got := parseCoverage(coveragePath, tt.format, "")
			if got != tt.want {
				t.Errorf("parseCoverage() format=%q = %v, want %v", tt.format, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseCoverage with relative path and worktree
// ---------------------------------------------------------------------------

func TestParseCoverageRelativePath(t *testing.T) {
	tmpDir := t.TempDir()

	content := "mode: set\nmain.go:1.1,10.2 10 10\n"
	coveragePath := filepath.Join(tmpDir, "sub", "coverage.out")
	if err := os.MkdirAll(filepath.Dir(coveragePath), 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}
	if err := os.WriteFile(coveragePath, []byte(content), 0600); err != nil { //nolint:gosec // test file
		t.Fatalf("failed to write temp file: %v", err)
	}

	// Pass relative path with worktree root.
	got := parseCoverage("sub/coverage.out", "go-cover", tmpDir)
	if got != 100.0 {
		t.Errorf("parseCoverage() relative path = %v, want 100.0", got)
	}
}
