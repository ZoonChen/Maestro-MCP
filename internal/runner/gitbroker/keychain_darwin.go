//go:build darwin

package gitbroker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// platformKeychainRunner executes macOS security(1) lookups.
func platformKeychainRunner() (keychainRunner, error) {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		if err != nil {
			return nil, err
		}
		return out, nil
	}, nil
}

// lookup reads the internet-password item for the host: `-w` answers
// the password; the attribute dump carries the account (defaulting to
// git when the item stores none).
func (k KeychainCredentialSource) lookup(ctx context.Context, host string) (Credential, error) {
	password, err := k.runner(ctx, "security", "find-internet-password", "-s", host, "-w")
	if err != nil {
		return Credential{}, fmt.Errorf("gitbroker keychain: host %q: %w", host, err)
	}
	username := "git"
	if attributes, attrErr := k.runner(ctx, "security", "find-internet-password", "-s", host); attrErr == nil {
		for line := range strings.SplitSeq(string(attributes), "\n") {
			trimmed := strings.TrimSpace(line)
			if value, ok := strings.CutPrefix(trimmed, `acct: "`); ok {
				username = strings.TrimSuffix(value, `"`)
			}
		}
	}
	return Credential{Username: username, Password: trimSecret(password)}, nil
}
