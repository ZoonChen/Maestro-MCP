package webhook

import (
	"context"
	"crypto/subtle"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// SecretResolver resolves a stored secret reference to its current value.
// References are opaque strings owned by the deployment (gitlab_instances
// webhook_secret_ref); resolution happens per request so rotation takes
// effect without restart, and a failed resolution is a fail-closed
// configuration error, never a bypass.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// envRefName constrains env: references to explicitly-namespaced
// MAESTRO_ variables: an arbitrary variable name would turn the ref into
// a probe for unrelated process state.
var envRefName = regexp.MustCompile(`^MAESTRO_[A-Z0-9_]+$`)

// EnvSecretResolver resolves "env:MAESTRO_*" references against the
// process environment. Values are never logged or cached.
type EnvSecretResolver struct{}

// Resolve implements SecretResolver.
func (EnvSecretResolver) Resolve(_ context.Context, ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, "env:")
	if !ok {
		return "", fmt.Errorf("secret ref %q: only env: references are supported", ref)
	}
	if !envRefName.MatchString(name) {
		return "", fmt.Errorf("secret ref %q: env name must match MAESTRO_[A-Z0-9_]+", ref)
	}
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return "", fmt.Errorf("secret ref %q: environment variable is not set", ref)
	}
	return value, nil
}

// VerifyToken compares the presented X-Gitlab-Token against the resolved
// instance secret in constant time. Empty on either side never verifies:
// the CE shared-token mode has no "unset means open" posture.
func VerifyToken(presented, secret string) bool {
	if presented == "" || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) == 1
}
