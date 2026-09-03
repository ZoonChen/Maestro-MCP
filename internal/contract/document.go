// Package contract implements the M3-CTR-001 engine core: canonical
// normalization and hashing of OpenAPI 3 documents plus a versioned
// compatible/breaking diff rule set. It is deliberately dependency-free and
// pure so the contract store, workflows, and golden-case tests all share one
// deterministic implementation.
//
// W1/W2 preparation slice: JSON documents only (the M0 parser is JSON-only
// too); YAML sources and full JSON-Schema recursive diff land with the W3
// implementation task. Rules are versioned via RulesetVersion so Evidence can
// bind the exact rule semantics that produced a verdict.
package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// RulesetVersion identifies the diff rule semantics. Any rule change must
// bump this and update the golden fixtures.
const RulesetVersion = "contract-ruleset-v1"

// decodeSource turns JSON or YAML bytes into the generic tree the
// parser consumes. YAML numbers decode through json.Number so both
// forms share one numeric identity in the canonical encoding.
func decodeSource(data []byte) (map[string]any, error) {
	if DetectFormat(data) == FormatJSON {
		var root map[string]any
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.UseNumber()
		if err := decoder.Decode(&root); err != nil {
			return nil, fmt.Errorf("contract: invalid JSON: %w", err)
		}
		return root, nil
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("contract: invalid YAML: %w", err)
	}
	root, ok := normalizeYAML(raw).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("contract: YAML root must be a mapping")
	}
	return root, nil
}

// normalizeYAML converts yaml.v3 typed nodes to the JSON-generic tree:
// map[string]any, []any, string, bool, json.Number.
func normalizeYAML(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalizeYAML(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[fmt.Sprintf("%v", key)] = normalizeYAML(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = normalizeYAML(item)
		}
		return out
	case int:
		return json.Number(fmt.Sprintf("%d", typed))
	case int64:
		return json.Number(fmt.Sprintf("%d", typed))
	case uint64:
		return json.Number(fmt.Sprintf("%d", typed))
	case float64:
		return json.Number(jsonNumber(typed))
	default:
		return typed
	}
}

func jsonNumber(f float64) string {
	// Match encoding/json's number formatting so 1.5 and 2 stay
	// distinguishable lexemes in both source forms.
	raw, err := json.Marshal(f)
	if err != nil {
		return fmt.Sprintf("%v", f)
	}
	return string(raw)
}

// Document is a parsed OpenAPI 3 document with its canonical form and hash.
type Document struct {
	OpenAPI       string
	CanonicalHash string
	paths         map[string]map[string]map[string]any // path -> method -> operation object
	components    map[string]any
	security      []any
}

// SourceFormat identifies the wire form a contract arrived in; both
// forms canonicalize to the same bytes and therefore the same hash.
type SourceFormat string

const (
	FormatJSON SourceFormat = "openapi3-json"
	FormatYAML SourceFormat = "openapi3-yaml"
)

// DetectFormat returns the storage format for a contract source: JSON
// when the first non-space byte is '{', YAML otherwise. The loader owns
// no sniffing beyond this — a non-object YAML root fails in the parser.
func DetectFormat(data []byte) SourceFormat {
	trimmed := strings.TrimLeft(string(data), " \t\r\n")
	if strings.HasPrefix(trimmed, "{") {
		return FormatJSON
	}
	return FormatYAML
}

// ParseDocument validates and parses an OpenAPI 3 document in JSON or
// YAML form. Missing openapi/paths fields, a non-3.x version, or
// non-object shapes fail closed. Both forms produce identical
// canonical bytes (JSON numbers normalize via the same UseNumber
// pipeline), so a contract hashes the same regardless of source form.
func ParseDocument(data []byte) (*Document, error) {
	root, err := decodeSource(data)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("contract: empty document")
	}
	version, _ := root["openapi"].(string)
	if !strings.HasPrefix(version, "3.") {
		return nil, fmt.Errorf("contract: unsupported openapi version %q", version)
	}
	rawPaths, ok := root["paths"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("contract: missing paths object")
	}
	paths := make(map[string]map[string]map[string]any, len(rawPaths))
	for path, rawItem := range rawPaths {
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("contract: path %q must start with /", path)
		}
		item, ok := rawItem.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("contract: path item %q must be an object", path)
		}
		ops := make(map[string]map[string]any)
		for method, rawOp := range item {
			if !isHTTPMethod(method) {
				continue
			}
			op, ok := rawOp.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("contract: %s %s operation must be an object", method, path)
			}
			ops[strings.ToUpper(method)] = op
		}
		paths[path] = ops
	}
	doc := &Document{
		OpenAPI:    version,
		paths:      paths,
		components: objectValue(root, "components"),
	}
	if sec, ok := root["security"].([]any); ok {
		doc.security = sec
	}
	canonical, err := Canonicalize(data)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	doc.CanonicalHash = "sha256:" + hex.EncodeToString(sum[:])
	return doc, nil
}

// Canonicalize re-encodes a contract in either source form with sorted
// object keys and no insignificant whitespace. Numeric lexemes are
// preserved exactly (1.0 and 1 stay distinct), and both forms flow
// through the same generic-tree pipeline, so a contract canonicalizes
// identically regardless of how it arrived.
func Canonicalize(data []byte) ([]byte, error) {
	value, err := decodeSource(data)
	if err != nil {
		return nil, err
	}
	var builder strings.Builder
	if err := encodeCanonical(&builder, value); err != nil {
		return nil, err
	}
	return []byte(builder.String()), nil
}

func encodeCanonical(builder *strings.Builder, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		builder.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				builder.WriteByte(',')
			}
			keyJSON, err := json.Marshal(key)
			if err != nil {
				return fmt.Errorf("contract: canonical key: %w", err)
			}
			builder.Write(keyJSON)
			builder.WriteByte(':')
			if err := encodeCanonical(builder, typed[key]); err != nil {
				return err
			}
		}
		builder.WriteByte('}')
		return nil
	case []any:
		builder.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				builder.WriteByte(',')
			}
			if err := encodeCanonical(builder, item); err != nil {
				return err
			}
		}
		builder.WriteByte(']')
		return nil
	case json.Number:
		builder.WriteString(typed.String())
		return nil
	case string:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Errorf("contract: canonical string: %w", err)
		}
		builder.Write(encoded)
		return nil
	case nil:
		builder.WriteString("null")
		return nil
	case bool:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Errorf("contract: canonical bool: %w", err)
		}
		builder.Write(encoded)
		return nil
	default:
		return fmt.Errorf("contract: unsupported canonical value type %T", value)
	}
}

func isHTTPMethod(method string) bool {
	switch strings.ToLower(method) {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	}
	return false
}

func objectValue(root map[string]any, key string) map[string]any {
	if value, ok := root[key].(map[string]any); ok {
		return value
	}
	return nil
}
