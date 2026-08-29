// Package rbacspec embeds the frozen RBAC permission matrix so the binary
// and the reviewed YAML stay one physical source of truth
// (docs/specs/rbac/permissions.yaml, version 3.0, frozen by the M1 I1
// contract-freeze sprint). Changes go through the contract change process,
// never through a divergent copy.
package rbacspec

import _ "embed"

// PermissionsYAML is the authoritative permission matrix bytes.
//
//go:embed permissions.yaml
var PermissionsYAML []byte
