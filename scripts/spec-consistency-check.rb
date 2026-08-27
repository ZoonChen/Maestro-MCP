#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "set"
require "yaml"

ROOT = File.expand_path("..", __dir__)
errors = []

def load_yaml(path)
  YAML.safe_load(File.read(path), aliases: true)
end

def schema_property_names(schema, document, seen = Set.new)
  return Set.new unless schema.is_a?(Hash)

  if schema["$ref"]&.start_with?("#/components/schemas/")
    reference = schema["$ref"]
    return Set.new if seen.include?(reference)

    seen = seen.dup.add(reference)
    name = reference.split("/").last
    return schema_property_names(document.dig("components", "schemas", name), document, seen)
  end

  names = Set.new
  names.merge(schema.fetch("properties", {}).keys)
  schema.each_value do |value|
    case value
    when Hash
      names.merge(schema_property_names(value, document, seen))
    when Array
      value.each { |item| names.merge(schema_property_names(item, document, seen)) }
    end
  end
  names
end

rbac_path = File.join(ROOT, "docs/specs/rbac/permissions.yaml")
rbac = load_yaml(rbac_path)
permissions = Set.new
%w[roles functional_approvers service_identities bootstrap_identities].each do |group|
  rbac.fetch(group, {}).each_value do |principal|
    permissions.merge(Array(principal["allow"]))
  end
end

errors << "RBAC default_effect must be deny" unless rbac["default_effect"] == "deny"
errors << "final merge must remain human-only" unless rbac.dig("protected_actions", "final_merge") == {"executor" => "human_in_gitlab", "maestro_allowed" => false}
errors << "Agent permissions must be an intersection" unless rbac.dig("delegation", "agent_effective_permissions").to_s.start_with?("intersection(")
%w[agent_may_define_command_network_or_secret agent_may_self_review_waive_or_merge].each do |key|
  errors << "RBAC delegation #{key} must be false" unless rbac.dig("delegation", key) == false
end

bot_denies = Set.new(Array(rbac.dig("service_identities", "gitlab_bot", "deny")))
%w[git.repository.push gitlab.branch.create gitlab.merge protected_branch.merge].each do |permission|
  errors << "GitLab Bot deny missing #{permission}" unless bot_denies.include?(permission)
end

webhook_conditions = rbac.dig("service_identities", "gitlab_webhook_receiver", "conditions", "*", "all") || []
condition_keys = webhook_conditions.flat_map(&:keys).to_set
%w[raw_body_signature_verified inbox_persisted_before_success_response].each do |condition|
  errors << "Webhook receiver condition missing #{condition}" unless condition_keys.include?(condition)
end

openapi_paths = %w[
  docs/specs/openapi/control-plane.yaml
  docs/specs/openapi/runner.yaml
].map { |path| File.join(ROOT, path) }

write_operations = []
forbidden_scope_fields = Set.new(%w[project_id team_id role session_id principal_id])

openapi_paths.each do |path|
  document = load_yaml(path)
  document.fetch("paths", {}).each do |route, path_item|
    path_item.each do |method, operation|
      next unless %w[get post put patch delete].include?(method)
      next unless operation.is_a?(Hash)

      if %w[post put patch delete].include?(method)
        key = "#{File.basename(path)} #{method.upcase} #{route}"
        write_operations << [key, operation]
        %w[x-maestro-permission x-maestro-audit-event x-maestro-idempotency x-maestro-concurrency].each do |metadata|
          errors << "#{key}: missing #{metadata}" if operation[metadata].nil? || operation[metadata].to_s.empty?
        end
        permission = operation["x-maestro-permission"]
        errors << "#{key}: permission not mapped in RBAC: #{permission}" unless permissions.include?(permission)
      end

      request_content = operation.dig("requestBody", "content") || {}
      request_content.each_value do |media_type|
        fields = schema_property_names(media_type["schema"], document)
        forbidden = fields & forbidden_scope_fields
        errors << "#{method.upcase} #{route}: request body overrides scope fields #{forbidden.to_a.sort.join(', ')}" unless forbidden.empty?
      end
    end
  end
end

errors << "expected 28 OpenAPI write operations, got #{write_operations.length}" unless write_operations.length == 28

control = load_yaml(File.join(ROOT, "docs/specs/openapi/control-plane.yaml"))
component_schemas = control.dig("components", "schemas") || {}
expected_states = {
  "WorkItemState" => %w[draft queued leased executing validating ready_for_human_merge done blocked cancelling cancelled failed needs_human],
  "RunnerState" => %w[pending_approval approved online suspect offline draining revoked],
  "GateState" => %w[pending running passed failed error stale waived],
  "IntegrationRunState" => %w[waiting running passed failed cancelled expired],
  "DefectState" => %w[detected triaged assigned reproducing fixing verifying resolved ignored]
}
expected_states.each do |schema_name, states|
  actual = Array(component_schemas.dig(schema_name, "enum"))
  errors << "#{schema_name} mismatch: #{actual.inspect}" unless actual == states
end

errors << "Evidence must reuse canonical JSON Schema" unless component_schemas.dig("Evidence", "$ref") == "../schemas/evidence.schema.json"
errors << "QualityPolicy must reuse canonical JSON Schema" unless component_schemas.dig("QualityPolicy", "$ref") == "../schemas/quality-policy.schema.json"
waiver_states = Array(component_schemas.dig("Waiver", "properties", "status", "enum"))
errors << "Waiver lifecycle mismatch" unless waiver_states == %w[requested approved rejected active expired revoked]

control.fetch("paths", {}).each do |route, path_item|
  path_item.each do |method, operation|
    next unless operation.is_a?(Hash) && %w[get post put patch delete].include?(method)

    operation_id = operation["operationId"].to_s
    if route.match?(%r{/(merge|push)(/|$)}) || operation_id.match?(/mergeTask|mergeProtected|pushProtected/i)
      errors << "forbidden merge/push operation exposed: #{method.upcase} #{route} #{operation_id}"
    end
  end
end

mcp_path = File.join(ROOT, "docs/specs/mcp/tools.schema.json")
mcp_schema = JSON.parse(File.read(mcp_path))
catalog = Array(mcp_schema["examples"]).first || {}
tools = Array(catalog["tools"])
errors << "expected 14 MCP tools, got #{tools.length}" unless tools.length == 14
errors << "merge_task must not be registered" if tools.any? { |tool| tool["name"] == "merge_task" }

mutating_tools = tools.select { |tool| tool["mutating"] == true }
errors << "expected 9 mutating MCP tools, got #{mutating_tools.length}" unless mutating_tools.length == 9
mcp_forbidden_fields = forbidden_scope_fields | Set.new(%w[command command_string shell argv executable network secret])

tools.each do |tool|
  name = tool["name"] || "<unnamed>"
  permission = tool["required_permission"]
  errors << "MCP #{name}: permission not mapped in RBAC: #{permission}" unless permissions.include?(permission)
  input_schema = tool["input_schema"] || {}
  errors << "MCP #{name}: input additionalProperties must be false" unless input_schema["additionalProperties"] == false
  forbidden = schema_property_names(input_schema, {}) & mcp_forbidden_fields
  errors << "MCP #{name}: forbidden authority/tool field(s) #{forbidden.to_a.sort.join(', ')}" unless forbidden.empty?
  next unless tool["mutating"] == true

  %w[audit_event idempotency concurrency].each do |field|
    errors << "MCP #{name}: missing #{field}" unless tool[field].is_a?(Hash) || (field == "audit_event" && tool[field].is_a?(String))
  end
end

canonical_work_item_states = expected_states.fetch("WorkItemState")
errors << "MCP WorkItem states mismatch" unless catalog["work_item_states"] == canonical_work_item_states

if errors.empty?
  puts "spec-consistency-check: PASS openapi_writes=#{write_operations.length} rbac_permissions=#{permissions.length} mcp_tools=#{tools.length}"
  exit 0
end

warn "spec-consistency-check: FAIL (#{errors.length} issue#{errors.length == 1 ? '' : 's'})"
errors.each { |error| warn "- #{error}" }
exit 1
