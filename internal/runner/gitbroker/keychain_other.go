//go:build !darwin && !linux

package gitbroker

import "errors"

// platformKeychainRunner is unsupported on other platforms: the
// member credential must come from a configured source instead of a
// silent fallback.
func platformKeychainRunner() (keychainRunner, error) {
	return nil, errors.New("gitbroker keychain: no OS keychain implementation for this platform")
}
