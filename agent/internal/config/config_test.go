package config

import (
	"strings"
	"testing"

	"backup-agent/internal/userctx"
)

// A privileged system service (Windows Service under LocalSystem, macOS
// LaunchDaemon under root) doesn't have $HOME/%AppData% set to anything
// useful - on macOS it's simply unset, and the stdlib's os.UserConfigDir()
// hard-fails with "$HOME is not defined" instead of falling back. Dir()
// must resolve through userctx.HomeDir - the override point service
// bootstrap code points at the console user's real home - rather than
// reading the process environment directly, or the agent can never get
// past its first config read while running as a service.
func TestDirUsesUserctxHomeDirNotTheProcessEnvironment(t *testing.T) {
	fakeHome := t.TempDir()
	old := userctx.HomeDir
	userctx.HomeDir = func() (string, error) { return fakeHome, nil }
	defer func() { userctx.HomeDir = old }()

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if !strings.HasPrefix(dir, fakeHome) {
		t.Fatalf("dir = %q, attendu un sous-répertoire de %q (le home résolu par userctx)", dir, fakeHome)
	}
}

// The exact failure this agent hit in the field: userctx.HomeDir erroring
// (as it legitimately can - nobody logged in at the console yet) must
// surface as an error from Dir(), not panic or silently fall back to
// somewhere wrong.
func TestDirPropagatesUserctxHomeDirError(t *testing.T) {
	old := userctx.HomeDir
	userctx.HomeDir = func() (string, error) { return "", errFakeNoHome }
	defer func() { userctx.HomeDir = old }()

	if _, err := Dir(); err == nil {
		t.Fatal("attendu une erreur quand userctx.HomeDir échoue, obtenu nil")
	}
}

var errFakeNoHome = fakeErr("aucun utilisateur connecté à la console")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
