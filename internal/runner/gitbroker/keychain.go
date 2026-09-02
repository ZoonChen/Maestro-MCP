package gitbroker

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// KeychainCredentialSource resolves the member credential from the
// HOST operating system's secret store (M2-GIT-001: the runner host's
// Git broker pushes with member credentials held in the OS keychain —
// the control plane never sees them).
//
// The lookup key is the remote's host: every platform implementation
// reads the generic credential for that host and answers
// username/password for the askpass handshake. A missing entry is a
// fail-closed error: the broker never falls back to an anonymous or
// environment credential.
type KeychainCredentialSource struct {
	// runner executes the platform lookup command; injectable so the
	// argument construction and parsing are testable without touching
	// a real keychain.
	runner keychainRunner
}

// keychainRunner runs one platform command and returns stdout.
type keychainRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Credential implements CredentialSource for one remote.
func (k KeychainCredentialSource) Credential(ctx context.Context, remoteURL string) (Credential, error) {
	host, err := remoteHost(remoteURL)
	if err != nil {
		return Credential{}, err
	}
	if k.runner == nil {
		runner, platformErr := platformKeychainRunner()
		if platformErr != nil {
			return Credential{}, platformErr
		}
		k.runner = runner
	}
	return k.lookup(ctx, host)
}

// remoteHost extracts the hostname from any allowed remote form.
func remoteHost(remoteURL string) (string, error) {
	parsed, err := url.Parse(remoteURL)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("gitbroker keychain: remote %q carries no host", remoteURL)
	}
	return parsed.Hostname(), nil
}

// trimSecret strips trailing newlines the CLI tools append; the
// credential itself is never logged.
func trimSecret(out []byte) string {
	return strings.TrimRight(string(out), "\r\n")
}
