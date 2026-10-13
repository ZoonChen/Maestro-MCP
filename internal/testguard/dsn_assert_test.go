package testguard

import (
	"errors"
	"net/url"
	"testing"
)

// dsn builds a postgres URL from parts so the table carries no
// credential-shaped literals.
func dsn(user, pw, hostPort, db string) string {
	return (&url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, pw),
		Host:   hostPort,
		Path:   "/" + db,
	}).String()
}

// The guard's verdicts are pinned to the four-incident history: every DSN
// shape that ever wiped the resident database must be refused, every DSN the
// Makefile, CI or a suite helper legitimately produces must pass unchanged.
func TestAssertTestDSN(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		// Incident signature: test DSN on the resident port.
		{name: "resident port and live database is refused", dsn: dsn("maestro", "pw", "127.0.0.1:5434", "maestro"), wantErr: true},
		{name: "resident port refuses even an _test-named database", dsn: dsn("maestro", "pw", "127.0.0.1:5434", "maestro_test"), wantErr: true},
		{name: "resident port by hostname is refused", dsn: dsn("maestro", "pw", "localhost:5434", "maestro"), wantErr: true},
		// Managed Makefile default: disposable instance on 5435 whose
		// POSTGRES_DB is named "maestro".
		{name: "managed test instance passes", dsn: dsn("maestro-test", "pw", "127.0.0.1:5435", "maestro"), wantErr: false},
		// CI service container (m1-runtime.yml).
		{name: "CI maestro_test passes", dsn: dsn("maestro", "ci", "localhost:5435", "maestro_test"), wantErr: false},
		// Per-suite databases derived by fixture helpers.
		{name: "derived suite database passes", dsn: dsn("maestro", "pw", "127.0.0.1:5435", "maestro_handler_test"), wantErr: false},
		{name: "derived suite database off the test port still passes via _test marker", dsn: dsn("maestro", "pw", "127.0.0.1:5432", "maestro_gitlab_test"), wantErr: false},
		// Bare "maestro" database without the test port: the incident
		// signature on any other host port.
		{name: "bare maestro database without test port is refused", dsn: dsn("maestro", "pw", "127.0.0.1:5432", "maestro"), wantErr: true},
		// Fail-closed shapes.
		{name: "empty dsn is refused", dsn: "", wantErr: true},
		{name: "whitespace dsn is refused", dsn: "   ", wantErr: true},
		{name: "garbage dsn is refused", dsn: "not-a-dsn", wantErr: true},
		{name: "keyword-value dsn form is refused", dsn: "host=127.0.0.1 port=5434 dbname=maestro", wantErr: true},
		{name: "dsn without an explicit port is refused", dsn: dsn("maestro", "pw", "127.0.0.1", "maestro"), wantErr: true},
		{name: "zero-padded test port is not the test port", dsn: dsn("maestro", "pw", "127.0.0.1:05435", "maestro"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(allowAnyDSNEnv, "")
			err := AssertTestDSN(tc.dsn)
			if tc.wantErr {
				if !errors.Is(err, ErrDSNGuardBlocked) {
					t.Fatalf("AssertTestDSN(%q) error = %v, want ErrDSNGuardBlocked", tc.dsn, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("AssertTestDSN(%q) unexpected error = %v", tc.dsn, err)
			}
		})
	}
}

// The parsed form must equal the incident DSN as it was actually written,
// proving the table's builder is not weakening the shapes under test.
func TestDSNBuilderFidelity(t *testing.T) {
	got := dsn("maestro", "pw", "127.0.0.1:5434", "maestro")
	// #nosec G101 -- the historical incident DSN shape with a fake
	// password; this literal IS the fixture under test.
	want := "postgres://maestro:pw@127.0.0.1:5434/maestro"
	if got != want {
		t.Fatalf("builder produced %q, want %q", got, want)
	}
}

// The escape hatch must be a deliberate exact "1", and must override even
// the resident-port signature for CI's ephemeral unnamed databases.
func TestAssertTestDSNAllowOverride(t *testing.T) {
	t.Setenv(allowAnyDSNEnv, "1")
	if err := AssertTestDSN(dsn("maestro", "pw", "127.0.0.1:5434", "maestro")); err != nil {
		t.Fatalf("override did not bypass the resident signature: %v", err)
	}
	t.Setenv(allowAnyDSNEnv, "true")
	if err := AssertTestDSN(dsn("maestro", "pw", "127.0.0.1:5434", "maestro")); !errors.Is(err, ErrDSNGuardBlocked) {
		t.Fatalf("non-\"1\" override value must not bypass: %v", err)
	}
}
