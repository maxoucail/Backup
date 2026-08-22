#!/usr/bin/env bash
# Builds a proper, double-clickable .pkg installer. Must run on a real Mac
# (or a macOS CI runner - see .github/workflows/build-agents.yml): pkgbuild
# and productbuild are Apple tools with no Linux equivalent, unlike
# everything else in this project.
#
# Usage: ./pkgbuild.sh [version]
#
# Produces an unsigned package by default. To sign and notarize (removes
# the Gatekeeper warning for end users), export these before running:
#   CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)"
#   PKGSIGN_IDENTITY="Developer ID Installer: Your Name (TEAMID)"
#   NOTARY_PROFILE="your-notarytool-keychain-profile"
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

if [ "$(uname)" != "Darwin" ]; then
	echo "Ce script doit être exécuté sur macOS." >&2
	exit 1
fi

VERSION="${1:-1.0.0}"
AGENT_DIR="../.."
ROOT="pkgroot"
IDENTIFIER="com.backupcenter.agent"
INSTALL_PATH="/usr/local/backup-agent"

rm -rf "$ROOT" "*.pkg"
mkdir -p "$ROOT$INSTALL_PATH"

echo "==> Build universel (lipo)"
GOOS=darwin GOARCH=amd64 go build -ldflags "-s -w -X main.AgentVersion=$VERSION" -o backup-agent-amd64 "$AGENT_DIR/cmd/backup-agent"
GOOS=darwin GOARCH=arm64 go build -ldflags "-s -w -X main.AgentVersion=$VERSION" -o backup-agent-arm64 "$AGENT_DIR/cmd/backup-agent"
lipo -create -output "$ROOT$INSTALL_PATH/backup-agent" backup-agent-amd64 backup-agent-arm64

if [ -n "${CODESIGN_IDENTITY:-}" ]; then
	echo "==> Signature du binaire"
	codesign --force --options runtime --sign "$CODESIGN_IDENTITY" "$ROOT$INSTALL_PATH/backup-agent"
fi

mkdir -p scripts
cat > scripts/postinstall <<'EOF'
#!/bin/bash
# Runs as the installing user via the installer's per-user context is not
# guaranteed (pkg postinstall runs as root); launch the agent for the
# logged-in console user so its LaunchAgent gets registered in their
# session, not root's.
CONSOLE_USER=$(stat -f%Su /dev/console)
sudo -u "$CONSOLE_USER" /usr/local/backup-agent/backup-agent &
exit 0
EOF
chmod +x scripts/postinstall

echo "==> pkgbuild"
pkgbuild --root "$ROOT" --identifier "$IDENTIFIER" --version "$VERSION" \
	--scripts scripts --install-location / "BackupAgent-unsigned.pkg"

if [ -n "${PKGSIGN_IDENTITY:-}" ]; then
	echo "==> Signature du package"
	productsign --sign "$PKGSIGN_IDENTITY" "BackupAgent-unsigned.pkg" "BackupAgent-$VERSION.pkg"
	rm "BackupAgent-unsigned.pkg"
else
	mv "BackupAgent-unsigned.pkg" "BackupAgent-$VERSION.pkg"
fi

if [ -n "${NOTARY_PROFILE:-}" ]; then
	echo "==> Notarisation"
	xcrun notarytool submit "BackupAgent-$VERSION.pkg" --keychain-profile "$NOTARY_PROFILE" --wait
	xcrun stapler staple "BackupAgent-$VERSION.pkg"
fi

rm -rf "$ROOT" scripts backup-agent-amd64 backup-agent-arm64
echo "==> Terminé : $(pwd)/BackupAgent-$VERSION.pkg"
