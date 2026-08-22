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
  durcissement (`ProtectSystem=strict`, `NoNewPrivileges`, etc.) et le droit
  de se lier au port 80 sans tourner en root
  (`AmbientCapabilities=CAP_NET_BIND_SERVICE`) ;
- démarre le service.

Le mot de passe administrateur initial est généré aléatoirement au premier
démarrage et visible via `journalctl -u backup-server -n 50`, ainsi que dans
`/var/lib/backup-server/initial_admin_password.txt`. Connectez-vous sur
`http://<ip>` (port 80, panneau admin) puis changez-le immédiatement depuis
**Paramètres**. Voir la section **Ports** du README pour le détail des deux
ports (80 = panneau, 8420 = trafic agent) et comment les changer.

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
avec `agent/packaging/windows/build.sh`) et lancez-le **en administrateur**
(l'installeur le demande via l'élévation UAC). Il installe le binaire dans
`Program Files\BackupAgent` et l'enregistre comme un vrai **Service
Windows** (`BackupAgent`, démarrage automatique, LocalSystem) avec des
actions de récupération configurées (`sc.exe failure`) pour qu'il redémarre
seul après un incident. Un utilisateur standard ne peut pas l'arrêter -
seul un administrateur le peut, via `services.msc`, `net stop
BackupAgent`, ou en désinstallant.

Le service n'ayant pas de session graphique propre, il lance un petit
processus dans la session de l'utilisateur connecté (voir
`docs/ARCHITECTURE.md`) pour afficher l'assistant de configuration au
premier démarrage et les popups de progression ensuite. Au premier
lancement, une page s'ouvre donc dans le navigateur par défaut : renseignez
l'adresse du serveur et la clé d'enrôlement générée depuis le panneau.

### Compiler soi-même

```bash
cd agent/packaging/windows
./build.sh 1.0.0        # cross-compile + empaquette avec NSIS (apt install nsis)
```

### Installer/désinstaller manuellement (sans passer par l'installeur)

```powershell
backup-agent.exe install     # enregistre et démarre le service (admin requis)
backup-agent.exe uninstall   # arrête et désenregistre le service (admin requis)
```

### Désinstaller

Panneau de configuration Windows → Applications → *Backup Agent*, ou le
raccourci *Désinstaller* du menu Démarrer. Dans les deux cas, ceci arrête
et désenregistre proprement le service avant de retirer les fichiers.

## Agent macOS

### Installer

Téléchargez et décompressez `BackupAgent-macOS-*.tar.gz`, puis
double-cliquez sur `install.command` (demande le mot de passe administrateur
via `sudo`). Le script détecte automatiquement Intel vs Apple Silicon, copie
le binaire dans `/Library/BackupAgent/bin`, lève l'attribut de quarantaine
(nécessaire car le binaire n'est pas signé par défaut) et l'enregistre comme
un **LaunchDaemon système** démarrant au boot, avant toute connexion, et
redémarré automatiquement s'il s'arrête de façon inattendue (`KeepAlive`).
Seul un administrateur peut l'arrêter.

Comme pour Windows, la première exécution ouvre une page dans le
navigateur (via `launchctl asuser`, voir `docs/ARCHITECTURE.md`) pour
saisir l'adresse du serveur et la clé d'enrôlement.

**Étape manuelle obligatoire** : macOS protège Bureau/Documents/
Téléchargements/Images derrière TCC, même pour un daemon root. Ouvrez
*Réglages Système → Confidentialité et sécurité → Accès complet au disque*
et autorisez `backup-agent` - sinon ces dossiers ne seront pas lisibles.
L'agent détecte le blocage et envoie une notification de rappel plutôt que
d'échouer silencieusement.

### Installer/désinstaller manuellement (sans passer par install.command)

```bash
sudo /Library/BackupAgent/bin/backup-agent install     # enregistre et démarre le daemon
sudo /Library/BackupAgent/bin/backup-agent uninstall   # arrête et désenregistre le daemon
```

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
sudo /Library/BackupAgent/bin/backup-agent uninstall
sudo rm -rf /Library/BackupAgent
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
