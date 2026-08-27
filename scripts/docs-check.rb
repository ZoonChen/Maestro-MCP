#!/usr/bin/env ruby
# frozen_string_literal: true

require "csv"
require "date"
require "set"
require "uri"
require "yaml"

ROOT = File.expand_path("..", __dir__)
DOCS = File.join(ROOT, "docs")

REQUIRED_METADATA = %w[
  doc_id spec_version spec_status implementation_status verification_status
  owner_role approver_roles introduced_in authority_for related_adrs
  related_specs related_tests last_verified_commit
].freeze

REQUIRED_HEADINGS = [
  "目标与非目标",
  "参与者、角色、权限和信任边界",
  "触发条件、输入和前置条件",
  "正常交互及时序图",
  "失败、取消、超时、重试、恢复和用户提示",
  "状态机、规则和不可变式",
  "字段、配置和格式校验",
  "并发、幂等和一致性",
  "安全、Secret、隐私和审计",
  "质量门禁、证据与 fail-closed 规则",
  "指标、SLO、告警和运维动作",
  "验收测试和需求追踪",
  "数据迁移、兼容、发布与回滚"
].freeze

TRACE_HEADERS = [
  "Stage Task ID", "Requirement ID", "Rule/Gate ID", "ADR", "PRD",
  "Technical Design", "API/Schema/Event", "Data Migration",
  "Code Subsystem", "Permission", "Audit Event", "Test ID",
  "CI/Runtime Evidence", "Implementation Status", "Verification Status",
  "Verified Commit"
].freeze

AUTHORITY_DIRS = %w[
  governance delivery prd technical security quality testing operations decisions
].freeze

errors = []

def relative(path)
  path.delete_prefix("#{ROOT}/")
end

def split_frontmatter(text)
  match = text.match(/\A---\n(.*?)\n---\n/m)
  return [nil, text] unless match

  metadata = YAML.safe_load(
    match[1],
    permitted_classes: [Date],
    aliases: true
  )
  [metadata || {}, text[match.end(0)..]]
end

def path_reference?(value)
  value.include?("/") || value.end_with?(".md", ".yaml", ".yml", ".json", ".csv")
end

authority_files = [File.join(DOCS, "README.md")]
AUTHORITY_DIRS.each do |directory|
  authority_files.concat(Dir[File.join(DOCS, directory, "**", "*.md")])
end
authority_files.sort!.uniq!

documents = {}
doc_ids = {}

authority_files.each do |file|
  text = File.read(file)
  metadata, body = split_frontmatter(text)
  unless metadata
    errors << "#{relative(file)}: missing YAML frontmatter"
    next
  end

  REQUIRED_METADATA.each do |key|
    errors << "#{relative(file)}: missing metadata key #{key}" unless metadata.key?(key)
  end

  errors << "#{relative(file)}: spec_version must be 3.0" unless metadata["spec_version"].to_s == "3.0"
  errors << "#{relative(file)}: invalid spec_status" unless %w[draft review approved superseded].include?(metadata["spec_status"])
  errors << "#{relative(file)}: invalid implementation_status" unless %w[not_started partial implemented].include?(metadata["implementation_status"])
  errors << "#{relative(file)}: invalid verification_status" unless %w[unverified failed passed].include?(metadata["verification_status"])
  errors << "#{relative(file)}: invalid introduced_in" unless %w[M0 M1 M2 M3 M4].include?(metadata["introduced_in"])

  %w[approver_roles authority_for related_adrs related_specs related_tests].each do |key|
    errors << "#{relative(file)}: #{key} must be an array" unless metadata[key].is_a?(Array)
  end

  doc_id = metadata["doc_id"]
  if doc_ids.key?(doc_id)
    errors << "duplicate doc_id #{doc_id}: #{relative(doc_ids[doc_id])}, #{relative(file)}"
  else
    doc_ids[doc_id] = file
  end

  headings = body.scan(/^## (\d+)\. (.+)$/).map { |number, title| [number.to_i, title] }
  expected = REQUIRED_HEADINGS.each_with_index.map { |title, index| [index + 1, title] }
  errors << "#{relative(file)}: required 13-section template mismatch" unless headings == expected

  documents[file] = {metadata: metadata, text: text}
end

all_authority_text = documents.values.map { |document| document[:text] }.join("\n")

documents.each do |file, document|
  metadata = document[:metadata]

  %w[related_specs related_tests].each do |key|
    Array(metadata[key]).each do |reference|
      if path_reference?(reference.to_s)
        target = File.expand_path(reference.to_s, File.dirname(file))
        errors << "#{relative(file)}: missing #{key} target #{reference}" unless File.exist?(target)
        errors << "#{relative(file)}: #{key} cannot reference v2.1 archive" if target.include?("/docs/archive/v2.1/")
      elsif key == "related_tests"
        errors << "#{relative(file)}: unresolved related test #{reference}" unless all_authority_text.include?(reference.to_s)
      else
        errors << "#{relative(file)}: #{key} entry must be a path: #{reference}"
      end
    end
  end

  Array(metadata["related_adrs"]).each do |reference|
    next if doc_ids.key?(reference)

    if path_reference?(reference.to_s)
      target = File.expand_path(reference.to_s, File.dirname(file))
      errors << "#{relative(file)}: missing related_adrs target #{reference}" unless File.exist?(target)
      errors << "#{relative(file)}: related_adrs cannot reference v2.1 archive" if target.include?("/docs/archive/v2.1/")
    else
      errors << "#{relative(file)}: unresolved related ADR #{reference}"
    end
  end

  verified_commit = metadata["last_verified_commit"]
  next unless metadata["verification_status"] == "passed"

  # A literal `HEAD` binds the document to the very commit that carries it
  # (self-referential target-commit binding). An embedded SHA of the carrying
  # commit is unforgeable: the commit hash covers the document content, so no
  # fixed point exists. The check still guards against post-flip edits by
  # requiring the working tree to match HEAD.
  if verified_commit.to_s == "HEAD"
    unless system("git", "diff", "--quiet", "HEAD", "--", relative(file), chdir: ROOT)
      errors << "#{relative(file)}: verified content differs from HEAD"
    end
    next
  end

  if verified_commit.nil? || verified_commit.to_s !~ /\A[0-9a-f]{7,40}\z/
    errors << "#{relative(file)}: passed verification requires last_verified_commit"
    next
  end

  unless system("git", "cat-file", "-e", "#{verified_commit}^{commit}", chdir: ROOT, out: File::NULL, err: File::NULL)
    errors << "#{relative(file)}: last_verified_commit does not resolve"
    next
  end

  unless system("git", "diff", "--quiet", verified_commit.to_s, "--", relative(file), chdir: ROOT)
    errors << "#{relative(file)}: verified content changed after last_verified_commit"
  end
end

# Local Markdown links are checked independently from the external-link crawler.
documents.each do |file, document|
  document[:text].scan(/\[[^\]]*\]\(([^)]+)\)/).flatten.each do |destination|
    destination = destination.strip.sub(/\A</, "").sub(/>\z/, "")
    destination = destination.split(/\s+["']/).first.to_s
    next if destination.empty? || destination.start_with?("#", "http://", "https://", "mailto:", "data:")

    path = URI::DEFAULT_PARSER.unescape(destination.split("#", 2).first)
    target = File.expand_path(path, File.dirname(file))
    errors << "#{relative(file)}: broken local link #{destination}" unless File.exist?(target)
  end
end

# IDs are allowed to be cited many times, but a definition-like occurrence must
# not be spread across multiple authority files.
definition_files = Hash.new { |hash, key| hash[key] = Set.new }
id_pattern = /`((?:M[0-4]-[A-Z]+|[A-Z][A-Z0-9-]*-(?:REQ|RULE|GATE|INV)|TC-[A-Z0-9-]+)-\d{3})`/
documents.each do |file, document|
  section = nil
  document[:text].each_line do |line|
    section_match = line.match(/^## (\d+)\./)
    section = section_match[1].to_i if section_match
    line.to_enum(:scan, id_pattern).each do
      match = Regexp.last_match
      identifier = match[1]
      suffix = line[match.end(0), 3].to_s
      prefix = line[0...match.begin(0)].rstrip
      has_definition_marker = suffix.start_with?("：", ":", " |") || prefix.end_with?("|", "-")
      definition_like = if identifier.start_with?("TC-")
                          section == 12 && has_definition_marker
                        elsif identifier.match?(/\AM[0-4]-/)
                          relative(file).start_with?("docs/delivery/") && [6, 7].include?(section) && has_definition_marker
                        elsif identifier.include?("-REQ-")
                          section == 1 && has_definition_marker
                        else
                          [6, 10].include?(section) && has_definition_marker
                        end
      definition_files[identifier] << file if definition_like
    end
  end
end
definition_files.each do |identifier, files|
  next unless files.length > 1

  errors << "duplicate definition #{identifier}: #{files.map { |file| relative(file) }.sort.join(', ')}"
end

matrix_path = File.join(DOCS, "governance", "traceability-matrix.csv")
matrix = CSV.read(matrix_path, headers: true)
errors << "traceability matrix headers differ from v3 contract" unless matrix.headers == TRACE_HEADERS
errors << "traceability matrix must contain 31 M0-M4 tasks" unless matrix.length == 31

seen_tasks = Set.new

matrix.each_with_index do |row, index|
  line_number = index + 2
  TRACE_HEADERS.reject { |header| header == "Verified Commit" }.each do |header|
    errors << "traceability row #{line_number}: blank #{header}" if row[header].nil? || row[header].strip.empty?
  end

  task_id = row["Stage Task ID"]
  errors << "traceability row #{line_number}: duplicate task #{task_id}" unless seen_tasks.add?(task_id)
  errors << "traceability row #{line_number}: task not defined in delivery docs #{task_id}" unless all_authority_text.include?(task_id)

  %w[Requirement\ ID Rule/Gate\ ID Test\ ID].each do |column|
    row[column].to_s.split(";").map(&:strip).reject(&:empty?).each do |identifier|
      errors << "traceability row #{line_number}: unresolved #{column} #{identifier}" unless all_authority_text.include?(identifier)
    end
  end

  row["ADR"].to_s.split(";").map(&:strip).reject(&:empty?).each do |reference|
    if path_reference?(reference)
      target = File.expand_path(reference, ROOT)
      errors << "traceability row #{line_number}: missing ADR path #{reference}" unless File.exist?(target)
    else
      errors << "traceability row #{line_number}: unresolved ADR #{reference}" unless doc_ids.key?(reference)
    end
  end

  %w[PRD Technical\ Design API/Schema/Event].each do |column|
    row[column].to_s.split(";").map(&:strip).reject(&:empty?).each do |reference|
      target = File.expand_path(reference, ROOT)
      errors << "traceability row #{line_number}: missing #{column} path #{reference}" unless File.exist?(target)
      errors << "traceability row #{line_number}: archive cannot be authoritative #{reference}" if target.include?("/docs/archive/v2.1/")
    end
  end

  implementation_status = row["Implementation Status"]
  verification_status = row["Verification Status"]
  errors << "traceability row #{line_number}: invalid implementation status" unless %w[not_started partial implemented].include?(implementation_status)
  errors << "traceability row #{line_number}: invalid verification status" unless %w[unverified failed passed].include?(verification_status)
  if verification_status == "passed" && row["Verified Commit"] != "HEAD" &&
     row["Verified Commit"].to_s !~ /\A[0-9a-f]{7,40}\z/
    errors << "traceability row #{line_number}: passed verification requires Verified Commit"
  end
end

if errors.empty?
  puts "docs-check: PASS"
  puts "authoritative_markdown=#{authority_files.length} unique_doc_ids=#{doc_ids.length} traceability_rows=#{matrix.length}"
  exit 0
end

warn "docs-check: FAIL (#{errors.length} issue#{errors.length == 1 ? '' : 's'})"
errors.each { |error| warn "- #{error}" }
exit 1
