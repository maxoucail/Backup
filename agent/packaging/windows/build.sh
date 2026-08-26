#!/usr/bin/env bash
# Cross-compiles the Windows agent binary and packages it into a
# self-contained installer with NSIS. Runs entirely on Linux/macOS - no
# Windows machine needed, since Go cross-compiles natively and NSIS's
# compiler (makensis) is available as a normal Linux package
# (apt install nsis / brew install makensis).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

AGENT_DIR="../.."
VERSION="${1:-1.0.0}"

# VIProductVersion accepts strictly x.x.x.x and rejects anything else, so
# pad a shorter version ("1.0" -> "1.0.0.0") and strip any suffix
# ("1.0.9-rc1" -> "1.0.9.0") rather than letting makensis fail on it.
VERSION4="$(printf '%s' "$VERSION" | sed 's/[^0-9.].*$//' | awk -F. '{printf "%d.%d.%d.%d", $1, $2, $3, $4}')"

echo "==> Compilation Windows (amd64) - version $VERSION"
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -H=windowsgui -X main.AgentVersion=$VERSION" \
	-o backup-agent.exe "$AGENT_DIR/cmd/backup-agent"

if ! command -v makensis >/dev/null 2>&1; then
	echo "makensis introuvable. Installez NSIS (ex: apt install nsis) puis relancez ce script." >&2
	exit 1
fi

echo "==> Génération de l'installeur"
# Old builds produced an unversioned BackupAgentSetup.exe; remove it so a
# stale one can't be picked up by a glob and published as if it were this
# build.
rm -f BackupAgentSetup.exe
makensis -DAPP_VERSION="$VERSION" -DAPP_VERSION4="$VERSION4" installer.nsi

echo "==> Terminé : $(pwd)/BackupAgentSetup-$VERSION.exe"
