#!/usr/bin/env bash
# Backup Agent - installeur macOS.
#
# Installation machine (mot de passe administrateur demandé par sudo) :
# l'agent est enregistré comme un vrai LaunchDaemon système, démarrant au
# démarrage de la machine (avant toute connexion) et redémarré
# automatiquement s'il s'arrête de façon inattendue. Seul un administrateur
# peut l'arrêter (launchctl/launchd exigent root pour décharger un daemon
# système) - un utilisateur standard ne le peut pas.
#
# Ce script est volontairement un simple .command (double-cliquable dans le
# Finder, s'exécute dans Terminal) plutôt qu'un .pkg signé : il ne
# nécessite ni certificat Apple Developer ni machine macOS pour être
# généré. Un .pkg notarié peut être produit séparément depuis un Mac ou
# une CI macOS via pkgbuild.sh, pour une expérience d'installation plus
# polie une fois que vous disposez d'un certificat de signature.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

DEST_DIR="/Library/BackupAgent/bin"
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

echo "==> Installation pour $ARCH (mot de passe administrateur requis)"
sudo mkdir -p "$DEST_DIR"
sudo cp "$SRC" "$DEST"
sudo chmod +x "$DEST"

# Ad-hoc signature (no Apple Developer certificate involved, free, done
# here on the Mac since the build itself is cross-compiled on Linux and
# has no codesign of its own). Without any signature at all, macOS's TCC
# permission store (Full Disk Access included) tends to key a grant to the
# exact bytes of this file - recompiling for the next update, even with
# nothing behavioral changed, silently invalidates it. Signing with a
# fixed identifier every time gives TCC a stable identity to recognize
# across updates instead. This measurably helps in practice but is not a
# hard guarantee the way a real Developer ID certificate would be -
# reinstalling after a bigger update may still ask you to re-grant it.
sudo codesign --force --sign - --identifier com.backupcenter.agent "$DEST" 2>/dev/null || true

# The binary was extracted from a downloaded archive, so macOS marks it
# quarantined; without removing that flag, Gatekeeper blocks the first
# launch entirely. This is the standard, expected step for distributing
# an unsigned tool outside the App Store / outside notarization.
sudo xattr -dr com.apple.quarantine "$DEST" 2>/dev/null || true

echo "==> Enregistrement du service système (démarre dès l'écran de connexion)"
sudo "$DEST" install

cat <<EOF

==> Terminé. Backup Agent est installé et démarré en service système.

IMPORTANT : macOS protège les dossiers Bureau/Documents/Téléchargements/
Images (TCC). Ouvrez Réglages Système -> Confidentialité et sécurité ->
Accès complet au disque, et autorisez "backup-agent". Si vous sautez
cette étape (ou si une mise à jour future invalide l'autorisation),
l'agent ne pourra plus l'inventer : il détecte que la plupart des
fichiers sont refusés, arrête la sauvegarde au lieu de faire semblant
qu'elle a réussi, et vous préviendra par une notification à l'écran avec
la marche à suivre.

La fenêtre de configuration (adresse du serveur + clé d'enrôlement)
s'ouvre automatiquement dans votre navigateur au premier démarrage.
EOF
read -r -p "Appuyez sur Entrée pour fermer cette fenêtre..."
