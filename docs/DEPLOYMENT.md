# Déploiement

## Serveur sur Debian 13

Prérequis : rien à installer sur la machine cible - le binaire est
statique. Il faut juste le compiler (sur n'importe quelle machine avec Go
1.24+, y compris via cross-compilation) puis le copier sur le Debian 13.

```bash
# Sur une machine de build (peut être la même, ou un poste de dev) :
cd server
GOOS=linux GOARCH=amd64 go build -o backup-server ./cmd/backup-server
scp backup-server install.sh systemd/backup-server.service debian13-host:/tmp/

# Sur le Debian 13 :
cd /tmp
sudo ./install.sh /mnt/nas/backups   # chemin de stockage optionnel
```

Le script `install.sh` :

- crée un utilisateur système `backup-server` sans shell ;
- installe le binaire dans `/opt/backup-server` ;
- prépare `/var/lib/backup-server` (base SQLite, secrets) et, si fourni, le
  chemin de stockage NAS avec les bons droits ;
- installe et active le service systemd (`backup-server.service`), avec
  durcissement (`ProtectSystem=strict`, `NoNewPrivileges`, etc.) ;
- démarre le service.

Le mot de passe administrateur initial est généré aléatoirement au premier
démarrage et visible via `journalctl -u backup-server -n 50`, ainsi que dans
`/var/lib/backup-server/initial_admin_password.txt`. Connectez-vous sur
`http://<ip>:8420` puis changez-le immédiatement depuis **Paramètres**.

### Mettre à jour le serveur

```bash
sudo systemctl stop backup-server
sudo cp backup-server /opt/backup-server/backup-server
sudo systemctl start backup-server
```

La base SQLite et le stockage sont indépendants du binaire ; une mise à
jour ne touche à aucune donnée.

### Changer l'emplacement de stockage après coup

Depuis **Paramètres**, changez le champ *Chemin de stockage*. Le nouveau
chemin est utilisé immédiatement pour toute nouvelle sauvegarde, mais les
données existantes **ne sont pas migrées automatiquement** - copiez-les
vous-même (`rsync -a ancien/ nouveau/`) si vous voulez les conserver
accessibles.

## Agent Windows 11

### Installer

Téléchargez `BackupAgentSetup.exe` (release GitHub, ou compilé vous-même
avec `agent/packaging/windows/build.sh`) et lancez-le. Aucun droit
administrateur n'est nécessaire : l'installation est faite par utilisateur
(`%LOCALAPPDATA%\BackupAgent`), avec une tâche planifiée « à la connexion »
enregistrée automatiquement pour l'utilisateur courant - c'est ce qui
permet à l'agent d'afficher la popup de progression dans la session
interactive (un service Windows système ne le peut pas).

Au premier lancement, une page s'ouvre dans le navigateur par défaut :
renseignez l'adresse du serveur et la clé d'enrôlement générée depuis le
panneau.

### Compiler soi-même

```bash
cd agent/packaging/windows
./build.sh 1.0.0        # cross-compile + empaquette avec NSIS (apt install nsis)
```

### Désinstaller

Panneau de configuration Windows → Applications → *Backup Agent*, ou le
raccourci *Désinstaller* du menu Démarrer.

## Agent macOS

### Installer

Téléchargez et décompressez `BackupAgent-macOS-*.tar.gz`, puis
double-cliquez sur `install.command`. Le script détecte automatiquement
Intel vs Apple Silicon, copie le binaire dans
`~/Library/Application Support/BackupAgent`, lève l'attribut de
quarantaine (nécessaire car le binaire n'est pas signé par défaut) et
enregistre un *LaunchAgent* pour démarrer l'agent à chaque connexion.

Comme pour Windows, la première exécution ouvre une page dans le
navigateur pour saisir l'adresse du serveur et la clé d'enrôlement.

### Signer et notariser (optionnel, recommandé pour une distribution large)

Sans signature, macOS Gatekeeper affiche un avertissement au premier
lancement (contournable en une fois via *Réglages Système → Confidentialité
et sécurité*, ou déjà géré par `install.command` qui retire l'attribut de
quarantaine du binaire copié). Pour une expérience sans avertissement,
utilisez `agent/packaging/macos/pkgbuild.sh` **sur un Mac**, avec un
certificat Apple Developer :

```bash
export CODESIGN_IDENTITY="Developer ID Application: Votre Nom (TEAMID)"
export PKGSIGN_IDENTITY="Developer ID Installer: Votre Nom (TEAMID)"
export NOTARY_PROFILE="profil-notarytool-dans-le-trousseau"
./pkgbuild.sh 1.0.0
```

Produit un `.pkg` signé et notarié, installable en double-clic sans aucun
avertissement.

### Compiler soi-même sans signer

```bash
cd agent/packaging/macos
./build.sh 1.0.0
```

### Désinstaller

Double-cliquez sur `uninstall.command` (présent à côté de `install.command`
dans l'archive téléchargée), ou manuellement :

```bash
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.backupcenter.agent.plist
rm -rf ~/Library/Application\ Support/BackupAgent ~/Library/LaunchAgents/com.backupcenter.agent.plist
```

## CI/CD

`.github/workflows/build.yml` :

- `server` : compile et vérifie le serveur (`go vet`, `go build`, `go test`).
- `agents` : compile l'agent, installe NSIS, génère l'installeur Windows et
  l'archive macOS - le tout sur un runner Linux standard.
- `agent-macos-pkg` (optionnel, best-effort) : sur un runner macOS, produit
  un `.pkg` signé si les secrets `MACOS_CODESIGN_IDENTITY` /
  `MACOS_PKGSIGN_IDENTITY` sont configurés dans le dépôt, sinon un `.pkg`
  non signé.
- `release` : sur un tag `vX.Y.Z`, publie une release GitHub avec le
  binaire serveur, l'installeur Windows et l'archive macOS.
