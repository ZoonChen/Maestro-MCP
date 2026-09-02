#!/usr/bin/env ruby
# frozen_string_literal: true

# M0 coverage is deliberately calculated from an explicit, reviewable source
# manifest. This prevents a high-coverage utility package from masking an
# untested state registry or zero-trust validation path.

MINIMUM = Float(ENV.fetch("M0_CORE_COVERAGE_MIN", "80"))

unless ARGV.length == 4
  warn "usage: core-coverage-check.rb STATE_PROFILE VALIDATION_PROFILE IDENTITY_PROFILE STORE_PROFILE"
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
  },
  # M1 convergence (V1 manual section 4): the unified authorization core —
  # frozen matrix evaluation, token verification and the decision-point
  # middlewares — joins the explicit 80% gate.
  "identity-authorize" => {
    profile: ARGV.fetch(2),
    # Advisory until the V1 audit patch sprint: enforcement flips with
    # M1_CORE_COVERAGE_ENFORCE=1 once the groups hold 80% in CI (the
    # PG-gated suites need MAESTRO_TEST_POSTGRES_DSN, which only the
    # m1-runtime job provides).
    enforce: ENV.fetch("M1_CORE_COVERAGE_ENFORCE", "0") == "1",
    files: %w[
      internal/identity/policy.go
      internal/identity/authorize.go
      internal/identity/token.go
      internal/identity/devicetoken.go
      internal/identity/resolver.go
      internal/handler/identity_middleware.go
    ]
  },
  # The PostgreSQL control-plane critical paths: migration integrity,
  # the frozen store contracts and the fenced claim/lease lifecycle.
  "pg-store" => {
    profile: ARGV.fetch(3),
    enforce: ENV.fetch("M1_CORE_COVERAGE_ENFORCE", "0") == "1",
    files: %w[
      internal/store/postgres.go
      internal/store/postgres_migrations.go
      internal/store/postgres_identity.go
      internal/store/postgres_runner.go
      internal/store/postgres_events.go
      internal/store/postgres_idempotency.go
      internal/store/postgres_workitems.go
      internal/store/import_sqlite_pg.go
      internal/store/postgres_webhook.go
      internal/store/postgres_instances.go
      internal/store/postgres_gitlab.go
      internal/store/postgres_quality.go
    ]
  }
}.freeze

def parse_profile(path)
  lines = File.readlines(path, chomp: true)
  raise "#{path}: missing cover mode header" unless lines.first&.start_with?("mode:")

  # Multi-package runs emit the same block once per contributing test
  # binary; a block reported covered by ANY binary is covered. Keying by
  # the block (not just the file) keeps the denominator honest.
  blocks = Hash.new { |hash, key| hash[key] = 0 }
  lines.drop(1).each_with_index do |line, index|
    match = line.match(/\A(.+?):(\d+\.\d+,\d+\.\d+) (\d+) (\d+)\z/)
    raise "#{path}:#{index + 2}: malformed coverprofile row" unless match

    block = "#{match[1]}:#{match[2]}"
    blocks[block] = [blocks[block], Integer(match[4], 10)].max
  end
  # The block key reuses the profile path (github.com/.../file.go); the
  # consumer strips the module prefix, so normalize here instead.
  blocks.group_by { |block, _| block.split(":").first.sub(%r{\Agithub\.com/ZoonChen/Maestro-MCP/}, "") }
        .transform_values { |rows| [rows.size, rows.count { |_, count| count.positive? }] }
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
  enforced = config.fetch(:enforce, true)
  gate = percent + 1e-9 >= MINIMUM
  status = gate ? "PASS" : (enforced ? "FAIL" : "ADVISORY")
  failed = true if enforced && !gate
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
