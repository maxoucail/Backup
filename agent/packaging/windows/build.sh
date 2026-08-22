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

echo "==> Compilation Windows (amd64)"
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -H=windowsgui -X main.AgentVersion=$VERSION" \
	-o backup-agent.exe "$AGENT_DIR/cmd/backup-agent"

if ! command -v makensis >/dev/null 2>&1; then
	echo "makensis introuvable. Installez NSIS (ex: apt install nsis) puis relancez ce script." >&2
	exit 1
fi

echo "==> Génération de l'installeur"
makensis installer.nsi

echo "==> Terminé : $(pwd)/BackupAgentSetup.exe"
