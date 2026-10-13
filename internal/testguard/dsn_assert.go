// Package testguard hosts test-side safety sentinels. Its first member stops
// destructive test DDL (DROP DATABASE/SCHEMA/TABLE, TRUNCATE) before the
// statement ever reaches a non-disposable database: four data-loss incidents
// (2026-09-12 → 2026-10-13) shared one root cause — a test DSN pointing at
// the resident pilot PostgreSQL instead of the disposable test instance.
//
// This is the fifth shared-stack defense line. It complements, and never
// replaces, the P3 lines (physical isolation on port 5435, the role swap
// that denies DROP to the app role, and 4-hourly backups): those repair the
// damage's blast radius, this one refuses to let the damage start.
package testguard

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

const (
	// residentPostgresPort is the host port of maestro-resident-postgres,
	// the shared-stack live database. No test DSN may ever target it.
	residentPostgresPort = "5434"
	// testPostgresPort is the host port of maestro-test-postgres, the
	// disposable compose service managed by the Makefile WITH_TEST_PG macro.
	testPostgresPort = "5435"

	// allowAnyDSNEnv, when set to exactly "1", disables the assertion. It
	// exists for rare CI scenarios where the test database is ephemeral and
	// unnamed; production and local `make test` runs must never set it.
	allowAnyDSNEnv = "MAESTRO_ALLOW_ANY_DSN"

	// testDatabaseMarker is the substring shared by per-suite database names
	// (maestro_handler_test, maestro_gitlab_test, ...).
	testDatabaseMarker = "_test"
)

// ErrDSNGuardBlocked is returned when a DSN carries the resident database's
// signature. Fixtures surface it via t.Fatal: a blocked fixture means the
// guard worked, not that the test is broken.
var ErrDSNGuardBlocked = errors.New("dsn targets the resident database (port 5434 or db 'maestro'); use the isolated test PG (port 5435, db 'maestro_test' or 'maestro_handler_test'). Set MAESTRO_ALLOW_ANY_DSN=1 only in CI where the test DB is ephemeral")

// AssertTestDSN checks that a DSN targets the isolated test database, not
// the resident production database. It is called by test fixtures before any
// destructive DDL (DROP DATABASE/SCHEMA/TABLE, TRUNCATE) executes — in
// practice once at fixture entry, on the suite DSN every later connection
// (admin session, derived per-suite database) is rewritten from.
//
// The rules, evaluated fail-closed so any parse ambiguity blocks:
//
//  1. MAESTRO_ALLOW_ANY_DSN=1 short-circuits to allow (CI ephemeral DBs).
//  2. The DSN must parse as a postgres:// URL with a host.
//  3. Port 5434 — the resident pilot PostgreSQL — is always refused, even
//     for _test-named databases: sharing the instance is sharing the risk.
//  4. Otherwise the DSN must carry a positive test-stack marker: port 5435
//     (the disposable test instance, whatever its database name) or a
//     database name containing "_test". A bare "maestro" database on any
//     other port — the exact incident signature minus the resident port —
//     fails here.
func AssertTestDSN(dsn string) error {
	if os.Getenv(allowAnyDSNEnv) == "1" {
		return nil
	}
	blocked := func(reason string) error {
		return fmt.Errorf("%w: %s", ErrDSNGuardBlocked, reason)
	}

	parsed, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil || parsed.Host == "" {
		return blocked("not a parseable postgres:// URL with a host")
	}
	port := parsed.Port()
	if port == "" {
		return blocked("no explicit port; a test-stack marker (port 5435 or an *_test database name) is required")
	}
	if port == residentPostgresPort {
		return blocked(fmt.Sprintf("port %s is the resident pilot PostgreSQL", residentPostgresPort))
	}
	database := strings.Trim(parsed.Path, "/")
	if port != testPostgresPort && !strings.Contains(database, testDatabaseMarker) {
		return blocked(fmt.Sprintf("port %s with database %q carries no test-stack marker", port, database))
	}
	return nil
}
