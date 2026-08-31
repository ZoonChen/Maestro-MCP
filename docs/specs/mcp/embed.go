// Package mcpspec embeds the frozen MCP tool catalog
// (docs/specs/mcp/tools.schema.json, version 3.0, frozen by the M1 I1
// contract-freeze sprint) so the tool-to-permission mapping the policy
// guard enforces is derived from the same physical authority CI checks —
// no Go-side copy can drift.
package mcpspec

import (
	"encoding/json"
	"fmt"

	_ "embed"
)

//go:embed tools.schema.json
var ToolsSchemaJSON []byte

// ToolPermissions returns the frozen tool-name -> required-permission
// map. Unknown catalog shapes fail closed.
func ToolPermissions() (map[string]string, error) {
	var document struct {
		Examples []struct {
			Tools []struct {
				Name               string `json:"name"`
				RequiredPermission string `json:"required_permission"`
			} `json:"tools"`
		} `json:"examples"`
	}
	if err := json.Unmarshal(ToolsSchemaJSON, &document); err != nil {
		return nil, fmt.Errorf("mcpspec: parse tool catalog: %w", err)
	}
	if len(document.Examples) == 0 {
		return nil, fmt.Errorf("mcpspec: tool catalog has no example")
	}
	permissions := make(map[string]string, len(document.Examples[0].Tools))
	for _, tool := range document.Examples[0].Tools {
		if tool.Name == "" || tool.RequiredPermission == "" {
			return nil, fmt.Errorf("mcpspec: catalog entry without name or permission")
		}
		permissions[tool.Name] = tool.RequiredPermission
	}
	if len(permissions) == 0 {
		return nil, fmt.Errorf("mcpspec: catalog carries no tools")
	}
	return permissions, nil
}
