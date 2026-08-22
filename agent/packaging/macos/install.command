#!/usr/bin/env bash
# Backup Agent - installeur macOS.
#
# Ce script est volontairement un simple .command (double-cliquable dans le
# Finder, s'exécute dans Terminal) plutôt qu'un .pkg signé : il ne
# nécessite ni certificat Apple Developer ni machine macOS pour être
# généré. Un .pkg notarié peut être produit séparément depuis un Mac ou
# une CI macOS via pkgbuild.sh, pour une expérience d'installation plus
# polie une fois que vous disposez d'un certificat de signature.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

DEST_DIR="$HOME/Library/Application Support/BackupAgent/bin"
DEST="$DEST_DIR/backup-agent"

ARCH="$(uname -m)"
case "$ARCH" in
	arm64) SRC="backup-agent-arm64" ;;
	x86_64) SRC="backup-agent-amd64" ;;
	*) echo "Architecture non supportée: $ARCH" >&2; exit 1 ;;
esac

if [ ! -f "$SRC" ]; then
	echo "Binaire $SRC introuvable à côté de ce script." >&2
	exit 1
fi

echo "==> Installation pour $ARCH"
mkdir -p "$DEST_DIR"
cp "$SRC" "$DEST"
chmod +x "$DEST"

# The binary was extracted from a downloaded archive, so macOS marks it
# quarantined; without removing that flag, Gatekeeper blocks the first
# launch entirely. This is the standard, expected step for distributing
# an unsigned tool outside the App Store / outside notarization.
xattr -dr com.apple.quarantine "$DEST" 2>/dev/null || true

echo "==> Démarrage de l'agent (ouverture de l'assistant de configuration dans votre navigateur)"
"$DEST" >/dev/null 2>&1 &
disown

echo "==> Terminé. L'agent est installé dans : $DEST_DIR"
echo "Il démarre désormais automatiquement à chaque connexion."
read -r -p "Appuyez sur Entrée pour fermer cette fenêtre..."
