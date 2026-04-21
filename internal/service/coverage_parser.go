package service

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// parseCoverage reads a coverage report file and returns the coverage percentage (0-100).
// Supported formats: "go-cover", "cobertura", "jacoco", "istanbul".
// Returns -1 if the file cannot be parsed or the format is unsupported.
func parseCoverage(coveragePath, coverageFormat, worktreePath string) float64 {
	// Resolve path relative to worktree if not absolute.
	fullPath := coveragePath
	if !filepath.IsAbs(fullPath) {
		fullPath = filepath.Join(worktreePath, fullPath)
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return -1
	}

	switch coverageFormat {
	case "go-cover":
		return parseGoCover(string(data))
	case "cobertura":
		return parseCobertura(string(data))
	case "jacoco":
		return parseJacoco(string(data))
	case "istanbul":
		return parseIstanbul(string(data))
	default:
		// Try auto-detect by content.
		trimmed := strings.TrimSpace(string(data))
		if strings.HasPrefix(trimmed, "mode:") {
			return parseGoCover(string(data))
		}
		if strings.Contains(trimmed, "<report") && strings.Contains(trimmed, "<counter") {
			return parseJacoco(string(data))
		}
		if strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, `"s"`) {
			return parseIstanbul(string(data))
		}
		if strings.Contains(trimmed, "<coverage") && strings.Contains(trimmed, "cobertura") {
			return parseCobertura(string(data))
		}
		return -1
	}
}

// parseGoCover parses a Go coverage profile and returns the coverage percentage.
// Format: "mode: set|count|atomic" followed by lines like:
//
//	file.go:10.1,20.2 3 1
//
// The last number on each line is the count (0 = not covered, >0 = covered).
func parseGoCover(content string) float64 {
	scanner := bufio.NewScanner(strings.NewReader(content))
	var totalStmts, coveredStmts int

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip mode line and empty lines.
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}

		// Parse: file:range count
		// Format: "filename.go:line.col,line.col statements count"
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}

		stmts, err := strconv.Atoi(parts[len(parts)-2])
		if err != nil {
			continue
		}
		count, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			continue
		}

		totalStmts += stmts
		if count > 0 {
			coveredStmts += stmts
		}
	}

	if totalStmts == 0 {
		return 0
	}
	return float64(coveredStmts) / float64(totalStmts) * 100
}

// parseCobertura parses a Cobertura XML coverage report and returns the
// line-rate as a percentage. Looks for the line-rate attribute on the
// top-level <coverage> element.
func parseCobertura(content string) float64 {
	// Simple extraction: find line-rate="0.85" attribute.
	// For production use, consider a proper XML parser.
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "<coverage") {
			continue
		}
		// Extract line-rate attribute.
		idx := strings.Index(line, `line-rate="`)
		if idx < 0 {
			continue
		}
		start := idx + len(`line-rate="`)
		end := strings.Index(line[start:], `"`)
		if end < 0 {
			continue
		}
		rate, err := strconv.ParseFloat(line[start:start+end], 64)
		if err != nil {
			continue
		}
		return rate * 100
	}
	return -1
}

// parseJacoco parses a JaCoCo XML coverage report and returns the instruction
// coverage percentage. JaCoCo uses <counter type="INSTRUCTION" missed="X" covered="Y"/>
// elements nested within the report.
func parseJacoco(content string) float64 {
	var totalMissed, totalCovered int

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, `<counter`) {
			continue
		}
		if !strings.Contains(line, `type="INSTRUCTION"`) {
			continue
		}

		missed := extractIntAttr(line, "missed")
		covered := extractIntAttr(line, "covered")
		if missed < 0 || covered < 0 {
			continue
		}

		totalMissed += missed
		totalCovered += covered
	}

	total := totalCovered + totalMissed
	if total == 0 {
		return 0
	}
	return float64(totalCovered) / float64(total) * 100
}

// parseIstanbul parses an Istanbul/nyc JSON coverage report and returns the
// statement coverage percentage. Istanbul JSON is a map of file paths to
// coverage data objects, each with an "s" field mapping statement IDs to hit counts.
func parseIstanbul(content string) float64 {
	// Top-level: map of filepath -> file coverage object
	var report map[string]struct {
		S map[string]int `json:"s"`
	}
	if err := json.Unmarshal([]byte(content), &report); err != nil {
		return -1
	}

	var totalStmts, coveredStmts int
	for _, fileCov := range report {
		for _, hitCount := range fileCov.S {
			totalStmts++
			if hitCount > 0 {
				coveredStmts++
			}
		}
	}

	if totalStmts == 0 {
		return 0
	}
	return float64(coveredStmts) / float64(totalStmts) * 100
}

// extractIntAttr extracts an integer attribute value from an XML/HTML-like tag line.
// Returns -1 if the attribute is not found or cannot be parsed.
func extractIntAttr(line, attr string) int {
	prefix := attr + `="`
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return -1
	}
	start := idx + len(prefix)
	end := strings.Index(line[start:], `"`)
	if end < 0 {
		return -1
	}
	val, err := strconv.Atoi(line[start : start+end])
	if err != nil {
		return -1
	}
	return val
}
