package service

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
)

type diagnosticSecretPattern struct {
	pattern     *regexp.Regexp
	replacement string
}

var secretDiagnosticPatterns = []diagnosticSecretPattern{
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*basic\s+)[A-Za-z0-9+/=]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)((?:[A-Z0-9_.-]*(?:token|secret|password|api[_-]?key|access[_-]?key(?:[_-]?id)?|private[_-]?key|credential)[A-Z0-9_.-]*)\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`), `${1}[REDACTED]@`},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), `[REDACTED]`},
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,255}|github_pat_[A-Za-z0-9_]{20,255}|glpat-[A-Za-z0-9_-]{20,255})\b`), `[REDACTED]`},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`), `[REDACTED]`},
}

// boundedOutput captures at most limit bytes while continuing to consume the
// producer stream. It prevents unbounded buffering and records truncation as
// evidence so callers can fail closed.
type boundedOutput struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int64
	total int64
}

func newBoundedOutput(limit int64) *boundedOutput {
	if limit < 0 {
		limit = 0
	}
	return &boundedOutput{limit: limit}
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))
	remaining := b.limit - int64(b.buf.Len())
	if remaining > 0 {
		toWrite := int64(len(p))
		if toWrite > remaining {
			toWrite = remaining
		}
		_, _ = b.buf.Write(p[:toWrite])
	}
	return len(p), nil
}

func (b *boundedOutput) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *boundedOutput) String() string {
	return string(b.Bytes())
}

func (b *boundedOutput) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total > b.limit
}

func (b *boundedOutput) TotalBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

func sanitizeDiagnostic(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = redactPrivateKeyBlocks(value)
	for _, item := range secretDiagnosticPatterns {
		value = item.pattern.ReplaceAllString(value, item.replacement)
	}
	return value
}

func redactPrivateKeyBlocks(value string) string {
	lines := strings.SplitAfter(value, "\n")
	var output strings.Builder
	insidePrivateKey := false
	for _, line := range lines {
		normalized := strings.ToUpper(strings.TrimSpace(line))
		isBegin := strings.HasPrefix(normalized, "-----BEGIN ") &&
			strings.HasSuffix(normalized, "PRIVATE KEY-----")
		isEnd := strings.HasPrefix(normalized, "-----END ") &&
			strings.HasSuffix(normalized, "PRIVATE KEY-----")
		if isBegin {
			if !insidePrivateKey {
				output.WriteString("[REDACTED PRIVATE KEY]")
				if strings.HasSuffix(line, "\n") {
					output.WriteByte('\n')
				}
			}
			insidePrivateKey = true
			continue
		}
		if insidePrivateKey {
			if isEnd {
				insidePrivateKey = false
			}
			continue
		}
		output.WriteString(line)
	}
	return output.String()
}
