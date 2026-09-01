//go:build linux

package gitbroker

import (
	"context"
	"fmt"
	"os/exec"
)

// platformKeychainRunner executes secret-tool(1) lookups ( freedesktop
// secrets service: GNOME Keyring / KWallet behind one CLI).
func platformKeychainRunner() (keychainRunner, error) {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		if err != nil {
			return nil, err
		}
		return out, nil
	}, nil
}

// lookup reads the maestro gitbroker entries for the host. Two items
// are stored per host: server/host + username, server/host + password.
func (k KeychainCredentialSource) lookup(ctx context.Context, host string) (Credential, error) {
	username, err := k.runner(ctx, "secret-tool", "lookup", "service", "maestro-gitbroker", "host", host, "field", "username")
	if err != nil {
		return Credential{}, fmt.Errorf("gitbroker keychain: host %q: %w", host, err)
	}
	password, err := k.runner(ctx, "secret-tool", "lookup", "service", "maestro-gitbroker", "host", host, "field", "password")
	if err != nil {
		return Credential{}, fmt.Errorf("gitbroker keychain: host %q: %w", host, err)
	}
	return Credential{Username: trimSecret(username), Password: trimSecret(password)}, nil
}
