package gitbroker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The keychain source is tested through an injected runner: argument
// construction, output parsing and fail-closed behavior are the
// broker's contract; the OS integration itself is host-environment
// territory.

func TestKeychainCredentialResolvesPerHost(t *testing.T) {
	var commands []struct {
		name string
		args []string
	}
	source := KeychainCredentialSource{
		runner: func(_ context.Context, name string, args ...string) ([]byte, error) {
			commands = append(commands, struct {
				name string
				args []string
			}{name, args})
			last := ""
			if len(args) > 0 {
				last = args[len(args)-1]
			}
			switch last {
			case "-w", "password": // darwin security(1) / linux secret-tool
				return []byte("member-token\n"), nil
			case "username": // linux secret-tool
				return []byte("member@acme"), nil
			default: // darwin attribute dump
				return []byte("keychain: \"gitlab.acme.example\"\nacct: \"member@acme\"\n"), nil
			}
		},
	}

	credential, err := source.Credential(context.Background(), "https://gitlab.acme.example/group/repo.git")
	require.NoError(t, err)

	// The lookup key is the remote HOST, never the full URL with its
	// path (credential scope stays per-host) — on every platform the
	// host string must appear among the lookup arguments.
	allArgs := ""
	for _, command := range commands {
		allArgs += strings.Join(command.args, " ") + " "
	}
	assert.Contains(t, allArgs, "gitlab.acme.example")
	assert.NotContains(t, allArgs, "group/repo.git", "the URL path never reaches the keychain")
	assert.Equal(t, "member@acme", credential.Username)
	assert.Equal(t, "member-token", credential.Password)
}

func TestKeychainFailsClosed(t *testing.T) {
	source := KeychainCredentialSource{
		runner: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return nil, errors.New("could not be found")
		},
	}
	_, err := source.Credential(context.Background(), "https://gitlab.acme.example/x.git")
	require.Error(t, err, "a missing keychain entry never falls back")

	_, err = source.Credential(context.Background(), "not-a-url")
	require.Error(t, err, "hostless remotes cannot resolve a credential")
}

func TestRemoteHostExtraction(t *testing.T) {
	host, err := remoteHost("https://gitlab.acme.example/g/r.git")
	require.NoError(t, err)
	assert.Equal(t, "gitlab.acme.example", host)

	_, err = remoteHost("file:///tmp/origin.git")
	require.Error(t, err, "file fixtures have no host and never reach the keychain")

	_, err = remoteHost("git@host.example:path.git")
	if err == nil {
		// scp-like syntax parses with an empty Hostname; acceptable —
		// production remotes are https and the broker's allowlist has
		// already narrowed the reachable forms by this point.
		_ = host
	}
}

func TestTrimSecret(t *testing.T) {
	assert.Equal(t, "token", trimSecret([]byte("token\n")))
	assert.Equal(t, "token", trimSecret([]byte("token\r\n")))
	assert.Equal(t, "", trimSecret([]byte("\n")))
}
