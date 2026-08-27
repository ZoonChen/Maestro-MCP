#!/usr/bin/env ruby
# frozen_string_literal: true

# M0 coverage is deliberately calculated from an explicit, reviewable source
# manifest. This prevents a high-coverage utility package from masking an
# untested state registry or zero-trust validation path.

MINIMUM = Float(ENV.fetch("M0_CORE_COVERAGE_MIN", "80"))

unless ARGV.length == 2
  warn "usage: core-coverage-check.rb STATE_PROFILE VALIDATION_PROFILE"
  exit 2
end

groups = {
  "state-machine" => {
    profile: ARGV.fetch(0),
    files: ["internal/model/state_machine.go"]
  },
  "zero-trust-validation" => {
    profile: ARGV.fetch(1),
    files: %w[
      internal/service/validation_service.go
      internal/service/validation_policy.go
      internal/service/command_profile.go
      internal/service/boundary_checker.go
      internal/service/coverage_parser.go
      internal/service/git_helper.go
      internal/service/path_security.go
      internal/service/bounded_output.go
    ]
  }
}.freeze

def parse_profile(path)
  lines = File.readlines(path, chomp: true)
  raise "#{path}: missing cover mode header" unless lines.first&.start_with?("mode:")

  blocks = Hash.new { |hash, key| hash[key] = [0, 0] }
  lines.drop(1).each_with_index do |line, index|
    match = line.match(/\A(.+?):\d+\.\d+,\d+\.\d+ (\d+) (\d+)\z/)
    raise "#{path}:#{index + 2}: malformed coverprofile row" unless match

    file = match[1].sub(%r{\Agithub\.com/ZoonChen/Maestro-MCP/}, "")
    statements = Integer(match[2], 10)
    count = Integer(match[3], 10)
    blocks[file][0] += statements
    blocks[file][1] += statements if count.positive?
  end
  blocks
end

failed = false
groups.each do |name, config|
  profile = config.fetch(:profile)
  unless File.file?(profile)
    warn "core-coverage-check: FAIL #{name}: profile not found: #{profile}"
    failed = true
    next
  end

  begin
    blocks = parse_profile(profile)
  rescue StandardError => e
    warn "core-coverage-check: FAIL #{name}: #{e.message}"
    failed = true
    next
  end

  missing = config.fetch(:files).reject { |file| blocks.key?(file) }
  unless missing.empty?
    warn "core-coverage-check: FAIL #{name}: source missing from profile: #{missing.join(', ')}"
    failed = true
    next
  end

  total = config.fetch(:files).sum { |file| blocks.fetch(file)[0] }
  covered = config.fetch(:files).sum { |file| blocks.fetch(file)[1] }
  percent = total.zero? ? 0.0 : covered.fdiv(total) * 100
  status = percent + 1e-9 >= MINIMUM ? "PASS" : "FAIL"
  puts format(
    "core-coverage-check: %s %s %.1f%% (%d/%d statements, minimum %.1f%%)",
    status,
    name,
    percent,
    covered,
    total,
    MINIMUM
  )
  failed = true if status == "FAIL"
end

exit(failed ? 1 : 0)
