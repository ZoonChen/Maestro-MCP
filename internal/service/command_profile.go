package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type CommandProfileNetwork struct {
	Mode       string   `json:"mode"`
	AllowHosts []string `json:"allow_hosts"`
}

type CommandProfileResources struct {
	CPUMillis int `json:"cpu_millis"`
	MemoryMB  int `json:"memory_mb"`
	DiskMB    int `json:"disk_mb"`
	PIDs      int `json:"pids"`
}

// CommandProfile is server-owned immutable configuration. Tasks reference only
// ID/version/digest and can never submit argv, environment, network or limits.
type CommandProfile struct {
	ID               string                  `json:"id"`
	Version          string                  `json:"version"`
	ImageDigest      string                  `json:"image_digest"`
	Argv             []string                `json:"argv"`
	WorkingDirectory string                  `json:"working_directory"`
	Network          CommandProfileNetwork   `json:"network"`
	Resources        CommandProfileResources `json:"resources"`
	OutputLimitBytes int64                   `json:"output_limit_bytes"`
	TimeoutSeconds   int                     `json:"timeout_seconds"`
	Environment      map[string]string       `json:"environment,omitempty"`
}

type CommandProfileRegistry struct {
	profiles map[string]CommandProfile
}

var (
	commandProfileIDRe      = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
	commandProfileVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	imageDigestRe           = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	environmentNameRe       = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	forbiddenEnvNameRe      = regexp.MustCompile(`(?i)(token|secret|password|key)$`)
)

var forbiddenExecutables = map[string]struct{}{
	"sh": {}, "bash": {}, "zsh": {}, "fish": {}, "cmd": {}, "cmd.exe": {},
	"powershell": {}, "powershell.exe": {}, "pwsh": {}, "pwsh.exe": {},
}

func NewCommandProfileRegistry(profiles []CommandProfile) (*CommandProfileRegistry, error) {
	registry := &CommandProfileRegistry{profiles: make(map[string]CommandProfile, len(profiles))}
	for _, profile := range profiles {
		if err := validateCommandProfile(profile); err != nil {
			return nil, fmt.Errorf("command profile %q: %w", profile.ID, err)
		}
		key := profileKey(profile.ID, profile.Version)
		if _, exists := registry.profiles[key]; exists {
			return nil, fmt.Errorf("duplicate command profile %s", key)
		}
		profile.Argv = append([]string(nil), profile.Argv...)
		profile.Network.AllowHosts = append([]string(nil), profile.Network.AllowHosts...)
		profile.Environment = cloneStringMap(profile.Environment)
		registry.profiles[key] = profile
	}
	return registry, nil
}

func (r *CommandProfileRegistry) Resolve(id, version, digest string) (CommandProfile, error) {
	if r == nil {
		return CommandProfile{}, fmt.Errorf("command profile registry is not configured")
	}
	profile, ok := r.profiles[profileKey(id, version)]
	if !ok {
		return CommandProfile{}, fmt.Errorf("command profile %s@%s is not approved", id, version)
	}
	actualDigest, err := profile.Digest()
	if err != nil {
		return CommandProfile{}, err
	}
	if digest == "" || digest != actualDigest {
		return CommandProfile{}, fmt.Errorf("command profile digest mismatch")
	}
	return profile, nil
}

func (p CommandProfile) Digest() (string, error) {
	// Canonicalize semantically empty optional containers so a registry copy
	// cannot change the digest merely by turning [] into nil (JSON [] vs null).
	if len(p.Network.AllowHosts) == 0 {
		p.Network.AllowHosts = []string{}
	}
	if len(p.Environment) == 0 {
		p.Environment = nil
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal command profile: %w", err)
	}
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func profileKey(id, version string) string {
	return id + "@" + version
}

func validateCommandProfile(profile CommandProfile) error {
	if !commandProfileIDRe.MatchString(profile.ID) || !commandProfileVersionRe.MatchString(profile.Version) {
		return fmt.Errorf("invalid id or version")
	}
	if !imageDigestRe.MatchString(profile.ImageDigest) {
		return fmt.Errorf("image_digest must be an immutable sha256 digest")
	}
	if len(profile.Argv) == 0 || len(profile.Argv) > 32 {
		return fmt.Errorf("argv must contain 1..32 items")
	}
	for i, arg := range profile.Argv {
		if len(arg) > 1024 || strings.ContainsRune(arg, '\x00') || (i == 0 && arg == "") {
			return fmt.Errorf("argv item %d is invalid", i)
		}
	}
	if _, forbidden := forbiddenExecutables[strings.ToLower(filepath.Base(profile.Argv[0]))]; forbidden {
		return fmt.Errorf("shell interpreters are forbidden")
	}
	if _, err := validateRelativePath(profile.WorkingDirectory, true); err != nil {
		return fmt.Errorf("working_directory: %w", err)
	}
	if profile.Network.Mode != "none" || len(profile.Network.AllowHosts) != 0 {
		return fmt.Errorf("M0 local diagnostic profiles require network.mode=none")
	}
	if profile.Resources.CPUMillis < 100 || profile.Resources.CPUMillis > 8000 ||
		profile.Resources.MemoryMB < 128 || profile.Resources.MemoryMB > 16384 ||
		profile.Resources.DiskMB < 128 || profile.Resources.DiskMB > 51200 ||
		profile.Resources.PIDs < 16 || profile.Resources.PIDs > 2048 {
		return fmt.Errorf("resource limits are outside approved bounds")
	}
	if profile.OutputLimitBytes < 1024 || profile.OutputLimitBytes > 10<<20 {
		return fmt.Errorf("output_limit_bytes is outside 1024..10485760")
	}
	if profile.TimeoutSeconds < 1 || profile.TimeoutSeconds > 3600 {
		return fmt.Errorf("timeout_seconds is outside 1..3600")
	}
	for key, value := range profile.Environment {
		if !environmentNameRe.MatchString(key) || forbiddenEnvNameRe.MatchString(key) || len(value) > 4096 || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("environment variable %q is not allowed", key)
		}
	}
	return nil
}

type commandExecutionResult struct {
	ExitCode  int
	Output    string
	Duration  time.Duration
	Truncated bool
	TimedOut  bool
	Cancelled bool
}

func executeCommandProfile(ctx context.Context, profile CommandProfile, worktreePath string, cfg TestExecutionConfig) (commandExecutionResult, error) {
	result := commandExecutionResult{ExitCode: -1}
	if !cfg.AllowHostExecution {
		return result, fmt.Errorf("local host profile execution is disabled")
	}
	workingDir, err := resolvePathWithinRoot(worktreePath, profile.WorkingDirectory, true)
	if err != nil {
		return result, fmt.Errorf("resolve profile working directory: %w", err)
	}
	info, err := os.Stat(workingDir)
	if err != nil || !info.IsDir() {
		return result, fmt.Errorf("profile working directory is unavailable")
	}

	pathEnv := safeExecutableSearchPath()
	executable, err := resolveApprovedExecutable(profile.Argv[0], pathEnv)
	if err != nil {
		return result, err
	}
	timeout := time.Duration(profile.TimeoutSeconds) * time.Second
	if cfg.DefaultTimeout > 0 && cfg.DefaultTimeout < timeout {
		timeout = cfg.DefaultTimeout
	}
	limit := profile.OutputLimitBytes
	if cfg.MaxOutputBytes > 0 && int64(cfg.MaxOutputBytes) < limit {
		limit = int64(cfg.MaxOutputBytes)
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	temporaryHome, err := os.MkdirTemp("", "maestro-validation-home-")
	if err != nil {
		return result, fmt.Errorf("create isolated HOME: %w", err)
	}
	defer os.RemoveAll(temporaryHome)

	cmd := exec.Command(executable, profile.Argv[1:]...)
	cmd.Dir = workingDir
	cmd.Env = []string{"PATH=" + pathEnv, "HOME=" + temporaryHome, "LANG=C", "LC_ALL=C", "CI=true"}
	for key, value := range profile.Environment {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if runtime.GOOS != "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	output := newBoundedOutput(limit)
	cmd.Stdout = output
	cmd.Stderr = output

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start approved profile: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-execCtx.Done():
		if cmd.Process != nil {
			if runtime.GOOS == "windows" {
				_ = cmd.Process.Kill()
			} else {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}
		waitErr = <-done
		result.TimedOut = execCtx.Err() == context.DeadlineExceeded
		result.Cancelled = !result.TimedOut
	}
	result.Duration = time.Since(started)
	result.Truncated = output.Truncated()
	result.Output = sanitizeDiagnostic(output.String())
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if execCtx.Err() != nil {
		return result, execCtx.Err()
	}
	if waitErr != nil {
		return result, waitErr
	}
	if result.Truncated {
		return result, fmt.Errorf("profile output exceeded %d bytes", limit)
	}
	return result, nil
}

func safeExecutableSearchPath() string {
	seen := map[string]struct{}{}
	paths := make([]string, 0)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(entry) || strings.ContainsRune(entry, '\x00') {
			continue
		}
		clean := filepath.Clean(entry)
		if _, exists := seen[clean]; exists {
			continue
		}
		info, err := os.Stat(clean)
		if err != nil || !info.IsDir() {
			continue
		}
		seen[clean] = struct{}{}
		paths = append(paths, clean)
	}
	return strings.Join(paths, string(os.PathListSeparator))
}

func resolveApprovedExecutable(name, pathEnv string) (string, error) {
	if strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("approved executable contains NUL")
	}
	if strings.ContainsRune(name, filepath.Separator) {
		if !filepath.IsAbs(name) {
			return "", fmt.Errorf("approved executable path must be absolute")
		}
		return validateExecutable(name)
	}
	for _, directory := range filepath.SplitList(pathEnv) {
		candidate := filepath.Join(directory, name)
		if executable, err := validateExecutable(candidate); err == nil {
			return executable, nil
		}
	}
	return "", fmt.Errorf("approved executable %q was not found in safe PATH", name)
}

func validateExecutable(candidate string) (string, error) {
	info, err := os.Lstat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("executable is missing, non-regular, non-executable or a symlink")
	}
	if _, forbidden := forbiddenExecutables[strings.ToLower(filepath.Base(candidate))]; forbidden {
		return "", fmt.Errorf("shell interpreters are forbidden")
	}
	return filepath.Clean(candidate), nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
