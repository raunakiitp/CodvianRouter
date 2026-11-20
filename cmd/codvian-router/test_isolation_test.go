package main

import (
	"path/filepath"
	"testing"
)

// isolateUserDirs points every user directory the CLI resolves at a
// fresh temp dir, so tests never read or write the operator's real
// config: on Windows os.UserConfigDir reads %AppData% (not $HOME)
// and os.UserHomeDir reads %USERPROFILE%, so setting HOME alone
// leaves the config and data dir pointing at the real user profile
// (the PC's C:\Users\LOQ\... config broke 22 tests that assumed a
// fresh machine). On Unix it covers $HOME and $XDG_CONFIG_HOME the
// same way. Returns the temp dir.
func isolateUserDirs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	return dir
}
