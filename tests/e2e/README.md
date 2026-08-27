# M0 E2E test authority

`playwright.config.ts` runs only `specs-m0/`. These tests use the real
`maestro server` command, exact status codes, authentication and actual MCP
JSON-RPC messages.

The former v2.1 `specs/` and `specs-real-world/` suites were removed because
they contained obsolete states, removed merge behavior, skipped scenarios and
permissive assertions that accepted both success and failure. They cannot be
used as v3 evidence. Reintroducing a scenario requires an exact assertion in
`specs-m0/` against a real binary.

The former REST-based `R05-mcp-protocol.spec.ts` was also removed. MCP
conformance must never be represented by a REST-equivalent test.
