#!/usr/bin/env bash
# Installs backup-server on Debian 13 as a systemd service.
#
# Usage:
#   sudo ./install.sh [chemin_stockage_nas]
#
# If a storage path is given (e.g. a mounted NAS share), the server is
# pointed at it from the very first boot; otherwise it defaults to
# /var/lib/backup-server/storage and you can change it later from the
# Paramètres page.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
	echo "Ce script doit être exécuté en root (sudo ./install.sh)." >&2
	exit 1
fi

STORAGE_ROOT="${1:-}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_DIR=/opt/backup-server
DATA_DIR=/var/lib/backup-server

echo "==> Vérification du binaire"
if [ ! -f "$SCRIPT_DIR/backup-server" ]; then
	echo "Binaire backup-server introuvable dans $SCRIPT_DIR." >&2
	echo "Compilez-le d'abord: (cd $SCRIPT_DIR && go build -o backup-server ./cmd/backup-server)" >&2
	exit 1
fi

echo "==> Création de l'utilisateur système backup-server"
if ! id backup-server >/dev/null 2>&1; then
	useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin backup-server
fi

echo "==> Installation du binaire dans $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"
install -m 0755 "$SCRIPT_DIR/backup-server" "$INSTALL_DIR/backup-server"

echo "==> Préparation du répertoire de données $DATA_DIR"
mkdir -p "$DATA_DIR"
if [ -n "$STORAGE_ROOT" ]; then
	mkdir -p "$STORAGE_ROOT"
	chown -R backup-server:backup-server "$STORAGE_ROOT"
fi
chown -R backup-server:backup-server "$DATA_DIR"
chmod 750 "$DATA_DIR"

echo "==> Installation du service systemd"
UNIT_FILE=/etc/systemd/system/backup-server.service
cp "$SCRIPT_DIR/systemd/backup-server.service" "$UNIT_FILE"
if [ -n "$STORAGE_ROOT" ]; then
	sed -i "/^Environment=BACKUP_SERVER_AGENT_PORT/a Environment=BACKUP_SERVER_STORAGE_ROOT=$STORAGE_ROOT" "$UNIT_FILE"
	sed -i "s#^ReadWritePaths=.*#ReadWritePaths=$DATA_DIR $STORAGE_ROOT#" "$UNIT_FILE"
fi

systemctl daemon-reload
systemctl enable --now backup-server

IP="$(hostname -I | awk '{print $1}')"
echo "==> Terminé."
echo "Panneau d'administration : http://$IP (port 80 - à restreindre à votre réseau/VLAN d'administration)"
echo "Trafic agent (enrôlement, sauvegardes) : port 8420 - doit rester joignable depuis les VLAN des postes à sauvegarder"
echo "Ces deux ports se règlent via BACKUP_SERVER_PANEL_PORT / BACKUP_SERVER_AGENT_PORT dans $UNIT_FILE"
echo "Identifiants administrateur initiaux : voir 'journalctl -u backup-server -n 50' ou $DATA_DIR/initial_admin_password.txt"
