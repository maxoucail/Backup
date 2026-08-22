#!/usr/bin/env bash
set -euo pipefail

LABEL="com.backupcenter.agent"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
BIN_DIR="$HOME/Library/Application Support/BackupAgent"

echo "==> Arrêt et désenregistrement de l'agent"
launchctl bootout "gui/$(id -u)" "$PLIST" 2>/dev/null || true
rm -f "$PLIST"
rm -rf "$BIN_DIR"

echo "==> Backup Agent désinstallé."
read -r -p "Appuyez sur Entrée pour fermer cette fenêtre..."
