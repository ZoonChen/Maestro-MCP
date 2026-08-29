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
)

// RulesetVersion identifies the diff rule semantics. Any rule change must
// bump this and update the golden fixtures.
const RulesetVersion = "contract-ruleset-v1"

// Document is a parsed OpenAPI 3 document with its canonical form and hash.
type Document struct {
	OpenAPI       string
	CanonicalHash string
	paths         map[string]map[string]map[string]any // path -> method -> operation object
	components    map[string]any
	security      []any
}

// ParseDocument validates and parses an OpenAPI 3 JSON document. Missing
// openapi/paths fields, a non-3.x version, or non-object shapes fail closed.
func ParseDocument(data []byte) (*Document, error) {
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("contract: invalid JSON: %w", err)
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

// Canonicalize re-encodes a JSON document with sorted object keys and no
// insignificant whitespace. Numeric lexemes are preserved exactly (1.0 and 1
// stay distinct), so canonical inputs must come from one serialization
// pipeline to hash identically.
func Canonicalize(data []byte) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("contract: invalid JSON: %w", err)
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
