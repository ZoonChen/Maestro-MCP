#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "tmpdir"

ROOT = File.expand_path("..", __dir__)
AJV = if ENV["AJV_BIN"] && !ENV["AJV_BIN"].empty?
        [ENV["AJV_BIN"]]
      else
        %w[npx --yes -p ajv-cli@5.0.0 -p ajv-formats@3.0.1 ajv]
      end.freeze

SCHEMAS = (
  Dir[File.join(ROOT, "docs/specs/schemas/*.schema.json")] +
  [File.join(ROOT, "docs/specs/mcp/tools.schema.json")]
).sort.freeze

def run_ajv(*arguments)
  stdout, stderr, status = Open3.capture3(*AJV, *arguments, chdir: ROOT)
  print stdout
  warn stderr unless stderr.empty?
  raise "AJV command failed: #{arguments.join(' ')}" unless status.success?
end

compile_arguments = ["compile", "--spec=draft2020", "--strict=false", "-c", "ajv-formats"]
SCHEMAS.each { |schema| compile_arguments.concat(["-s", schema]) }
run_ajv(*compile_arguments)

example_count = 0
input_schema_count = 0

Dir.mktmpdir("maestro-schema-check-") do |directory|
  SCHEMAS.each do |schema_path|
    schema = JSON.parse(File.read(schema_path))
    Array(schema["examples"]).each_with_index do |example, index|
      example_path = File.join(directory, "#{File.basename(schema_path)}.example-#{index}.json")
      File.write(example_path, JSON.pretty_generate(example))
      run_ajv(
        "validate", "--spec=draft2020", "--strict=false", "-c", "ajv-formats",
        "-s", schema_path, "-d", example_path
      )
      example_count += 1
    end
  end

  evidence_schema = File.join(ROOT, "docs/specs/schemas/evidence.schema.json")
  Dir[File.join(ROOT, "docs/specs/examples/evidence.*.json")].sort.each do |example_path|
    run_ajv(
      "validate", "--spec=draft2020", "--strict=false", "-c", "ajv-formats",
      "-s", evidence_schema, "-d", example_path
    )
    example_count += 1
  end

  tool_catalog = JSON.parse(File.read(File.join(ROOT, "docs/specs/mcp/tools.schema.json")))
  catalog_example = Array(tool_catalog["examples"]).first || {}
  Array(catalog_example["tools"]).each_with_index do |tool, index|
    schema = tool.fetch("input_schema")
    tool_name = tool.fetch("name")
    input_path = File.join(directory, format("tool-%02d-%s.schema.json", index, tool_name))
    File.write(input_path, JSON.pretty_generate(schema))
    run_ajv(
      "compile", "--spec=draft2020", "--strict=false", "-c", "ajv-formats",
      "-s", input_path
    )
    input_schema_count += 1
  end
end

puts "schema-check: PASS schemas=#{SCHEMAS.length} examples=#{example_count} mcp_input_schemas=#{input_schema_count}"
