package service

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// parseCoverageEvidence is the fail-closed coverage parser used by validation.
// It requires an explicit known parser, a bounded regular file inside the
// worktree and fully valid content. Auto-detection and partial parsing are
// deliberately unsupported because either would allow malformed evidence to
// be interpreted as a pass.
func parseCoverageEvidence(coveragePath, coverageFormat, worktreePath string) (float64, error) {
	if coverageFormat != "go-cover" && coverageFormat != "cobertura" && coverageFormat != "jacoco" && coverageFormat != "istanbul" {
		return 0, fmt.Errorf("unsupported coverage parser %q", coverageFormat)
	}
	normalized, err := normalizeRepositoryPath(coveragePath, false)
	if err != nil || isSystemPath(normalized) {
		return 0, fmt.Errorf("unsafe coverage path")
	}
	fullPath, err := resolvePathWithinRoot(worktreePath, normalized, true)
	if err != nil {
		return 0, fmt.Errorf("resolve coverage path: %w", err)
	}
	info, err := os.Lstat(fullPath)
	if err != nil {
		return 0, fmt.Errorf("stat coverage evidence: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxValidationFileBytes {
		return 0, fmt.Errorf("coverage evidence must be a non-empty regular file no larger than %d bytes", maxValidationFileBytes)
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return 0, fmt.Errorf("open coverage evidence: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxValidationFileBytes+1))
	if err != nil {
		return 0, fmt.Errorf("read coverage evidence: %w", err)
	}
	if int64(len(data)) > maxValidationFileBytes || !utf8.Valid(data) {
		return 0, fmt.Errorf("coverage evidence exceeds bounds or is not UTF-8")
	}

	var coverage float64
	switch coverageFormat {
	case "go-cover":
		coverage, err = parseGoCoverStrict(string(data))
	case "cobertura":
		coverage, err = parseCoberturaStrict(data)
	case "jacoco":
		coverage, err = parseJacocoStrict(data)
	case "istanbul":
		coverage, err = parseIstanbulStrict(data)
	}
	if err != nil || math.IsNaN(coverage) || math.IsInf(coverage, 0) || coverage < 0 || coverage > 100 {
		if err == nil {
			err = fmt.Errorf("coverage value is outside 0..100")
		}
		return 0, err
	}
	return coverage, nil
}

func parseGoCoverStrict(content string) (float64, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	modeSeen := false
	records := 0
	var totalStmts, coveredStmts int64
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !modeSeen {
			if line != "mode: set" && line != "mode: count" && line != "mode: atomic" {
				return 0, fmt.Errorf("invalid Go coverage mode line")
			}
			modeSeen = true
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 3 || !strings.Contains(parts[0], ":") {
			return 0, fmt.Errorf("malformed Go coverage record")
		}
		stmts, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || stmts <= 0 {
			return 0, fmt.Errorf("invalid Go coverage statement count")
		}
		count, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || count < 0 {
			return 0, fmt.Errorf("invalid Go coverage execution count")
		}
		if totalStmts > math.MaxInt64-stmts {
			return 0, fmt.Errorf("go coverage counter overflow")
		}
		totalStmts += stmts
		if count > 0 {
			coveredStmts += stmts
		}
		records++
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan Go coverage: %w", err)
	}
	if !modeSeen || records == 0 || totalStmts == 0 {
		return 0, fmt.Errorf("go coverage evidence contains no statements")
	}
	return float64(coveredStmts) / float64(totalStmts) * 100, nil
}

func rejectUnsafeXML(data []byte) error {
	lower := strings.ToLower(string(data))
	if strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<!entity") {
		return fmt.Errorf("DTD/entity declarations are forbidden")
	}
	return nil
}

func parseCoberturaStrict(data []byte) (float64, error) {
	if err := rejectUnsafeXML(data); err != nil {
		return 0, err
	}
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	decoder.Strict = true
	depth := 0
	foundRoot := false
	var rate float64
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("parse Cobertura XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if depth > 128 {
				return 0, fmt.Errorf("cobertura XML nesting exceeds limit")
			}
			if depth == 1 {
				if value.Name.Local != "coverage" {
					return 0, fmt.Errorf("cobertura root element must be coverage")
				}
				for _, attr := range value.Attr {
					if attr.Name.Local == "line-rate" {
						rate, err = strconv.ParseFloat(attr.Value, 64)
						if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
							return 0, fmt.Errorf("invalid Cobertura line-rate")
						}
						foundRoot = true
					}
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	if !foundRoot || depth != 0 {
		return 0, fmt.Errorf("cobertura line-rate evidence is missing")
	}
	return rate * 100, nil
}

func parseJacocoStrict(data []byte) (float64, error) {
	if err := rejectUnsafeXML(data); err != nil {
		return 0, err
	}
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	decoder.Strict = true
	depth := 0
	foundRoot := false
	foundCounter := false
	var missed, covered int64
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("parse JaCoCo XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if depth > 128 {
				return 0, fmt.Errorf("JaCoCo XML nesting exceeds limit")
			}
			if depth == 1 {
				if value.Name.Local != "report" {
					return 0, fmt.Errorf("JaCoCo root element must be report")
				}
				foundRoot = true
			}
			if depth == 2 && value.Name.Local == "counter" {
				attrs := make(map[string]string, len(value.Attr))
				for _, attr := range value.Attr {
					attrs[attr.Name.Local] = attr.Value
				}
				if attrs["type"] == "INSTRUCTION" {
					missed, err = strconv.ParseInt(attrs["missed"], 10, 64)
					if err != nil || missed < 0 {
						return 0, fmt.Errorf("invalid JaCoCo missed counter")
					}
					covered, err = strconv.ParseInt(attrs["covered"], 10, 64)
					if err != nil || covered < 0 {
						return 0, fmt.Errorf("invalid JaCoCo covered counter")
					}
					foundCounter = true
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	if !foundRoot || !foundCounter || depth != 0 || missed+covered <= 0 {
		return 0, fmt.Errorf("JaCoCo report-level instruction counter is missing")
	}
	return float64(covered) / float64(missed+covered) * 100, nil
}

func parseIstanbulStrict(data []byte) (float64, error) {
	var report map[string]struct {
		S map[string]int64 `json:"s"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&report); err != nil {
		return 0, fmt.Errorf("parse Istanbul JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return 0, err
	}
	if len(report) == 0 {
		return 0, fmt.Errorf("istanbul report contains no files")
	}
	var total, covered int64
	for _, file := range report {
		if len(file.S) == 0 {
			return 0, fmt.Errorf("istanbul file contains no statement counters")
		}
		for _, hits := range file.S {
			if hits < 0 {
				return 0, fmt.Errorf("istanbul statement counter is negative")
			}
			total++
			if hits > 0 {
				covered++
			}
		}
	}
	if total == 0 {
		return 0, fmt.Errorf("istanbul report contains no statements")
	}
	return float64(covered) / float64(total) * 100, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("coverage JSON contains multiple values")
		}
		return fmt.Errorf("coverage JSON trailing data: %w", err)
	}
	return nil
}
