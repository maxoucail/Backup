#!/usr/bin/env bash
set -euo pipefail

DEST="/Library/BackupAgent/bin/backup-agent"

if [ -x "$DEST" ]; then
	echo "==> Arrêt et désenregistrement du service (mot de passe administrateur requis)"
	sudo "$DEST" uninstall
fi

sudo rm -rf /Library/BackupAgent

echo "==> Backup Agent désinstallé."
read -r -p "Appuyez sur Entrée pour fermer cette fenêtre..."
