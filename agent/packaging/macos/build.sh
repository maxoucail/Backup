#!/usr/bin/env bash
# Cross-compiles the macOS agent (Intel + Apple Silicon) and bundles it
# with a double-clickable install.command into a distributable tarball.
# Runs entirely on Linux - no Mac needed for this "just works, unsigned"
# package. For a signed/notarized .pkg, run pkgbuild.sh on an actual Mac
# (or a macOS CI runner) once you have an Apple Developer certificate.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

AGENT_DIR="../.."
VERSION="${1:-1.0.0}"
DIST="dist"

rm -rf "$DIST"
mkdir -p "$DIST"

echo "==> Compilation macOS (amd64 - Intel)"
GOOS=darwin GOARCH=amd64 go build -ldflags "-s -w -X main.AgentVersion=$VERSION" \
	-o "$DIST/backup-agent-amd64" "$AGENT_DIR/cmd/backup-agent"

echo "==> Compilation macOS (arm64 - Apple Silicon)"
GOOS=darwin GOARCH=arm64 go build -ldflags "-s -w -X main.AgentVersion=$VERSION" \
	-o "$DIST/backup-agent-arm64" "$AGENT_DIR/cmd/backup-agent"

cp install.command uninstall.command "$DIST/"
chmod +x "$DIST/install.command" "$DIST/uninstall.command"

tar -C "$DIST" -czf "BackupAgent-macOS-$VERSION.tar.gz" .
echo "==> Terminé : $(pwd)/BackupAgent-macOS-$VERSION.tar.gz"
echo "L'utilisateur décompresse l'archive puis double-clique sur install.command."
