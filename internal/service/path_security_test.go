package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalExistingDirAndRelativePathFailClosed(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	linked := filepath.Join(t.TempDir(), "linked-root")
	require.NoError(t, os.Symlink(root, linked))

	canonical, err := canonicalExistingDir(linked)
	require.NoError(t, err)
	expected, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	assert.Equal(t, expected, canonical)
	for _, value := range []string{"", "bad\x00path", filepath.Join(root, "missing"), file} {
		_, err := canonicalExistingDir(value)
		require.Error(t, err, value)
	}

	for _, tc := range []struct {
		value    string
		allowDot bool
		want     string
		ok       bool
	}{
		{value: ".", allowDot: true, want: ".", ok: true},
		{value: "src/main.go", want: filepath.Join("src", "main.go"), ok: true},
		{value: ""},
		{value: "bad\x00path"},
		{value: root},
		{value: ".."},
		{value: "../secret"},
		{value: "src/../secret"},
		{value: ".", allowDot: false},
	} {
		got, err := validateRelativePath(tc.value, tc.allowDot)
		if tc.ok {
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		} else {
			require.Error(t, err, tc.value)
		}
	}
}

func TestResolvePathWithinRootRejectsSymlinksAndNonDirectories(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := canonicalExistingDir(root)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src", "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "nested", "main.go"), []byte("package main\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "plain"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "linked")))

	got, err := resolvePathWithinRoot(root, ".", true)
	require.NoError(t, err)
	assert.Equal(t, canonicalRoot, got)
	got, err = resolvePathWithinRoot(root, "src/nested/main.go", true)
	require.NoError(t, err)
	assert.True(t, pathWithinRoot(canonicalRoot, got))
	got, err = resolvePathWithinRoot(root, "src/future/file.go", false)
	require.NoError(t, err)
	assert.True(t, pathWithinRoot(canonicalRoot, got))

	for _, tc := range []struct {
		path     string
		required bool
	}{
		{path: "src/missing.go", required: true},
		{path: "linked/file", required: false},
		{path: "plain/child", required: false},
		{path: "../outside", required: false},
	} {
		_, err := resolvePathWithinRoot(root, tc.path, tc.required)
		require.Error(t, err, tc.path)
	}
	assert.False(t, pathWithinRoot(root, filepath.Dir(root)))
	assert.False(t, pathWithinRoot("\x00", root))
}

func TestValidateWorktreeLocationRequiresReservedSubtree(t *testing.T) {
	workspace := t.TempDir()
	reserved := filepath.Join(workspace, ".maestro", "worktrees")
	valid := filepath.Join(reserved, "task-1")
	require.NoError(t, os.MkdirAll(valid, 0o700))
	got, err := validateWorktreeLocation(workspace, valid)
	require.NoError(t, err)
	expected, err := canonicalExistingDir(valid)
	require.NoError(t, err)
	assert.Equal(t, expected, got)

	for _, candidate := range []string{workspace, reserved, t.TempDir(), filepath.Join(reserved, "missing")} {
		_, err := validateWorktreeLocation(workspace, candidate)
		require.Error(t, err, candidate)
	}
	_, err = validateWorktreeLocation(filepath.Join(workspace, "missing"), valid)
	require.Error(t, err)
}
