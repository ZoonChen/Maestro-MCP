package contract

import "fmt"

// Change records one semantic difference between two documents. Breaking is
// conservative: anything that could reject or starve an existing consumer of
// the old document counts as breaking under ruleset-v1.
type Change struct {
	Location string
	Detail   string
	Breaking bool
}

// DiffResult summarizes a diff under the current RulesetVersion.
type DiffResult struct {
	Ruleset    string
	Compatible bool
	Changes    []Change
	OldHash    string
	NewHash    string
}

type schemaDirection int

const (
	schemaDirectionRequest schemaDirection = iota
	schemaDirectionResponse
)

// Diff compares an old and a new document under the ruleset-v1 semantics:
//
// Breaking: removing a path/operation/response status; adding a required
// request parameter or required request field; changing a type; narrowing an
// enum (either direction); removing a response field; adding or removing
// effective security requirements.
//
// Non-breaking: adding operations, statuses, optional fields/parameters;
// relaxing request schemas; widening enums; relaxing security.
func Diff(oldDoc, newDoc *Document) *DiffResult {
	result := &DiffResult{
		Ruleset: RulesetVersion,
		OldHash: oldDoc.CanonicalHash,
		NewHash: newDoc.CanonicalHash,
	}
	for path, oldOps := range oldDoc.paths {
		newOps, ok := newDoc.paths[path]
		if !ok {
			result.add(Change{Location: "paths" + path, Detail: "path removed", Breaking: true})
			continue
		}
		for method, oldOp := range oldOps {
			newOp, ok := newOps[method]
			if !ok {
				result.add(Change{Location: opLocation(path, method), Detail: "operation removed", Breaking: true})
				continue
			}
			result.diffOperation(path, method, oldOp, newOp, effectiveSecurity(oldOp, oldDoc.security), effectiveSecurity(newOp, newDoc.security))
		}
	}
	result.Compatible = !result.hasBreaking()
	return result
}

func (r *DiffResult) add(change Change) { r.Changes = append(r.Changes, change) }

func (r *DiffResult) hasBreaking() bool {
	for _, change := range r.Changes {
		if change.Breaking {
			return true
		}
	}
	return false
}

func (r *DiffResult) diffOperation(path, method string, oldOp, newOp map[string]any, oldSecurity, newSecurity []any) {
	location := opLocation(path, method)
	oldParams := parameterList(oldOp)
	newParams := parameterList(newOp)
	for _, oldParam := range oldParams {
		name, _ := oldParam["name"].(string)
		if findParameter(newParams, name) == nil {
			r.add(Change{Location: location + ".parameters." + name, Detail: "request parameter removed", Breaking: false})
		}
	}
	for _, newParam := range newParams {
		name, _ := newParam["name"].(string)
		oldParam := findParameter(oldParams, name)
		required, _ := newParam["required"].(bool)
		if oldParam == nil {
			detail := "optional request parameter added"
			if required {
				detail = "required request parameter added"
			}
			r.add(Change{Location: location + ".parameters." + name, Detail: detail, Breaking: required})
			continue
		}
		if typeOf(schemaOf(oldParam)) != typeOf(schemaOf(newParam)) {
			r.add(Change{Location: location + ".parameters." + name, Detail: "parameter type changed", Breaking: true})
		}
		r.diffEnum(location+".parameters."+name, schemaOf(oldParam), schemaOf(newParam))
	}
	r.diffRequestSchema(location, oldOp, newOp)
	r.diffResponses(location, oldOp, newOp)
	r.diffSecurity(location, oldSecurity, newSecurity)
}

func (r *DiffResult) diffRequestSchema(location string, oldOp, newOp map[string]any) {
	oldBody, oldOK := oldOp["requestBody"].(map[string]any)
	newBody, newOK := newOp["requestBody"].(map[string]any)
	if oldOK && !newOK {
		r.add(Change{Location: location + ".requestBody", Detail: "request body removed", Breaking: false})
		return
	}
	if !oldOK || !newOK {
		return
	}
	oldSchema := contentSchema(oldBody)
	newSchema := contentSchema(newBody)
	if oldSchema == nil || newSchema == nil {
		return
	}
	r.diffSchema(location+".requestBody", oldSchema, newSchema, schemaDirectionRequest)
}

func (r *DiffResult) diffResponses(location string, oldOp, newOp map[string]any) {
	oldResponses, _ := oldOp["responses"].(map[string]any)
	newResponses, _ := newOp["responses"].(map[string]any)
	for status, rawOld := range oldResponses {
		rawNew, ok := newResponses[status]
		if !ok {
			r.add(Change{Location: location + ".responses." + status, Detail: "response status removed", Breaking: true})
			continue
		}
		oldResponse, _ := rawOld.(map[string]any)
		newResponse, _ := rawNew.(map[string]any)
		oldSchema := contentSchema(oldResponse)
		newSchema := contentSchema(newResponse)
		if oldSchema == nil || newSchema == nil {
			continue
		}
		r.diffSchema(location+".responses."+status, oldSchema, newSchema, schemaDirectionResponse)
	}
	for status := range newResponses {
		if _, ok := oldResponses[status]; !ok {
			r.add(Change{Location: location + ".responses." + status, Detail: "response status added", Breaking: false})
		}
	}
}

func (r *DiffResult) diffSchema(location string, oldSchema, newSchema map[string]any, direction schemaDirection) {
	if typeOf(oldSchema) != typeOf(newSchema) {
		r.add(Change{Location: location, Detail: "schema type changed", Breaking: true})
		return
	}
	r.diffEnum(location, oldSchema, newSchema)
	oldProperties, _ := oldSchema["properties"].(map[string]any)
	newProperties, _ := newSchema["properties"].(map[string]any)
	for name, rawOldProperty := range oldProperties {
		oldProperty, _ := rawOldProperty.(map[string]any)
		rawNewProperty, ok := newProperties[name]
		newProperty, _ := rawNewProperty.(map[string]any)
		if !ok {
			// Removing a property loosens requests but starves consumers
			// reading it from responses; direction decides.
			r.add(Change{Location: location + ".properties." + name, Detail: "property removed", Breaking: direction == schemaDirectionResponse})
			continue
		}
		r.diffSchema(location+".properties."+name, oldProperty, newProperty, direction)
	}
	if direction == schemaDirectionRequest {
		for name := range newProperties {
			if _, ok := oldProperties[name]; !ok && isRequired(newSchema, name) {
				r.add(Change{Location: location + ".properties." + name, Detail: "required request property added", Breaking: true})
			}
		}
	}
}

func (r *DiffResult) diffEnum(location string, oldSchema, newSchema map[string]any) {
	if oldSchema == nil || newSchema == nil {
		return
	}
	oldEnum := enumValues(oldSchema)
	newEnum := enumValues(newSchema)
	if oldEnum == nil || newEnum == nil {
		return
	}
	for _, value := range oldEnum {
		if !containsValue(newEnum, value) {
			r.add(Change{Location: location + ".enum", Detail: "enum value removed (narrowed)", Breaking: true})
			return
		}
	}
}

func (r *DiffResult) diffSecurity(location string, oldSecurity, newSecurity []any) {
	if len(oldSecurity) == 0 && len(newSecurity) > 0 {
		r.add(Change{Location: location + ".security", Detail: "security requirement added", Breaking: true})
		return
	}
	oldSchemes := schemeNames(oldSecurity)
	newSchemes := schemeNames(newSecurity)
	for _, scheme := range oldSchemes {
		if !containsString(newSchemes, scheme) {
			r.add(Change{Location: location + ".security." + scheme, Detail: "security scheme removed", Breaking: true})
		}
	}
}

func opLocation(path, method string) string {
	return method + " " + path
}

// effectiveSecurity resolves OpenAPI security inheritance: an operation that
// omits security inherits the document-level requirement.
func effectiveSecurity(operation map[string]any, documentSecurity []any) []any {
	if requirements, ok := operation["security"].([]any); ok {
		return requirements
	}
	return documentSecurity
}

func parameterList(operation map[string]any) []map[string]any {
	rawList, ok := operation["parameters"].([]any)
	if !ok {
		return nil
	}
	list := make([]map[string]any, 0, len(rawList))
	for _, raw := range rawList {
		if parameter, ok := raw.(map[string]any); ok {
			list = append(list, parameter)
		}
	}
	return list
}

func findParameter(list []map[string]any, name string) map[string]any {
	for _, parameter := range list {
		if parameterName, _ := parameter["name"].(string); parameterName == name {
			return parameter
		}
	}
	return nil
}

func schemaOf(parameter map[string]any) map[string]any {
	if schema, ok := parameter["schema"].(map[string]any); ok {
		return schema
	}
	return parameter
}

func contentSchema(container map[string]any) map[string]any {
	content, ok := container["content"].(map[string]any)
	if !ok {
		return nil
	}
	for _, rawMedia := range content {
		media, ok := rawMedia.(map[string]any)
		if !ok {
			continue
		}
		if schema, ok := media["schema"].(map[string]any); ok {
			return schema
		}
	}
	return nil
}

func typeOf(object map[string]any) string {
	value, _ := object["type"].(string)
	return value
}

func isRequired(schema map[string]any, property string) bool {
	list, ok := schema["required"].([]any)
	if !ok {
		return false
	}
	for _, raw := range list {
		if name, ok := raw.(string); ok && name == property {
			return true
		}
	}
	return false
}

func enumValues(schema map[string]any) []any {
	list, ok := schema["enum"].([]any)
	if !ok {
		return nil
	}
	return list
}

func containsValue(list []any, target any) bool {
	for _, value := range list {
		if fmt.Sprint(value) == fmt.Sprint(target) {
			return true
		}
	}
	return false
}

func schemeNames(requirements []any) []string {
	schemes := make([]string, 0, len(requirements))
	for _, rawRequirement := range requirements {
		requirement, ok := rawRequirement.(map[string]any)
		if !ok {
			continue
		}
		for scheme := range requirement {
			schemes = append(schemes, scheme)
		}
	}
	return schemes
}

func containsString(list []string, target string) bool {
	for _, value := range list {
		if value == target {
			return true
		}
	}
	return false
}
