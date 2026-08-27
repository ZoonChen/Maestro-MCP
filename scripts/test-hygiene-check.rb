#!/usr/bin/env ruby
# frozen_string_literal: true

root = File.expand_path("..", __dir__)
active_files = Dir[File.join(root, "tests/e2e/specs-m0/**/*.ts")]
active_files.concat(Dir[File.join(root, "tests/m0/**/*_test.go")])
errors = []

active_files.each do |path|
  text = File.read(path)
  relative = path.delete_prefix("#{root}/")
  errors << "#{relative}: REST-equivalent MCP assertion is forbidden" if text.match?(/REST\s+equivalent/i)
  errors << "#{relative}: required test contains test.skip/fixme" if text.match?(/\btest\.(?:skip|fixme)\b/)
  if text.match?(/expect\(\[(?:[^\]]*\b2\d\d\b[^\]]*\b[45]\d\d\b|[^\]]*\b[45]\d\d\b[^\]]*\b2\d\d\b)[^\]]*\]\)\.toContain/)
    errors << "#{relative}: success and error status codes cannot both pass"
  end
  if text.match?(/if\s*\([^\n]*status[^\n]*\)\s*\{?\s*return\b/m)
    errors << "#{relative}: early return after status check is forbidden"
  end
end

%w[tests/e2e/specs tests/e2e/specs-real-world].each do |legacy_directory|
  path = File.join(root, legacy_directory)
  next unless Dir.exist?(path) && !Dir.empty?(path)

  errors << "legacy fake-pass suite still contains files: #{legacy_directory}"
end

if errors.empty?
  puts "test-hygiene-check: PASS files=#{active_files.length}"
  exit 0
end

warn "test-hygiene-check: FAIL issues=#{errors.length}"
errors.each { |error| warn "- #{error}" }
exit 1
