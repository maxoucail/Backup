# Backup Center

Système de sauvegarde centralisé façon Proxmox Backup Server / Time Machine :
un **serveur** à installer sur votre NAS/Debian 13, avec panneau
d'administration web, et des **agents** légers pour Windows 11 et macOS qui
sauvegardent automatiquement les dossiers utilisateur (Bureau, Documents,
Images, Téléchargements) vers ce serveur, de façon incrémentale et
déduplicquée.

Écrit en **Go** : un seul binaire statique par composant, quelques
millisecondes de démarrage, quelques Mo de RAM au repos, et une
concurrence native (goroutines) qui permet à plusieurs appareils de
sauvegarder en parallèle sans effort ni surcharge du serveur.

## Composants

```
server/   Serveur (Debian 13) : panneau web + API + stockage
agent/    Agent client (Windows / macOS, tourne aussi sur Linux)
```

## Fonctionnement en bref

- **Sauvegarde incrémentale réelle** : chaque fichier est identifié par son
  empreinte SHA-256 et, pour les gros fichiers, découpé en blocs adressés
  par leur propre empreinte. Un fichier inchangé (taille + date de
  modification identiques) n'est même pas relu. Seuls les blocs que le
  serveur ne possède **encore par personne** sont transférés - la
  déduplication est globale, pas seulement par appareil.
- **Rétention façon Proxmox** : on choisit dans le panneau combien de
  sauvegardes garder par appareil ; la plus ancienne est purgée
  automatiquement dès que la limite est dépassée, et l'espace disque
  correspondant est récupéré (garbage collection des blocs orphelins).
- **Identification définitive** : au premier démarrage, l'agent demande
  l'adresse du serveur et une clé d'enrôlement à usage unique générée
  depuis le panneau. Une fois connecté, l'appareil est administré à
  distance en permanence (renommage, politique, sauvegarde/restauration à
  la demande) sans jamais redemander la clé.
- **Base de données bornée** : SQLite stocke les comptes, appareils,
  sauvegardes et un historique d'événements ; ce dernier est purgé
  automatiquement (par ancienneté et par nombre de lignes maximum,
  réglable) pour que la base ne grossisse pas indéfiniment.
- **Popup de progression à distance** : un déclenchement manuel ou distant
  (« Sauvegarder maintenant », « Restaurer ») ouvre sur l'écran de
  l'appareil une page locale avec barre de progression et temps restant.
  Les sauvegardes planifiées, elles, tournent silencieusement en arrière-plan.

Voir `docs/ARCHITECTURE.md` pour le détail du protocole et des choix de
conception, et `docs/DEPLOYMENT.md` pour l'installation pas à pas.

## Démarrage rapide

### Serveur (Debian 13)

```bash
cd server
go build -o backup-server ./cmd/backup-server
sudo ./install.sh                      # installe en service systemd
# ou avec un chemin de stockage NAS dédié :
sudo ./install.sh /mnt/nas/backups
```

Le mot de passe administrateur initial (compte `admin`) est affiché au
premier démarrage (`journalctl -u backup-server`) et écrit dans
`/var/lib/backup-server/initial_admin_password.txt`. Changez-le depuis
**Paramètres** une fois connecté au panneau, sur `http://<ip-serveur>:8420`.

### Enrôler un appareil

1. Dans le panneau, **Paramètres → Enrôler un nouvel appareil** : générez une clé.
2. Installez l'agent sur le poste Windows ou macOS (voir ci-dessous).
3. Au premier lancement, l'agent ouvre une page dans le navigateur par
   défaut : renseignez l'adresse du serveur et la clé. L'appareil apparaît
   alors dans le panneau et est administré à distance.

### Agent Windows

Téléchargez `BackupAgentSetup.exe` (généré par la CI, ou compilé localement
avec `agent/packaging/windows/build.sh`) et exécutez-le. Installation par
utilisateur, aucun droit administrateur requis.

### Agent macOS

Téléchargez et décompressez `BackupAgent-macOS-*.tar.gz`, puis double-cliquez
sur `install.command`. Le binaire n'étant pas signé par défaut, macOS peut
demander confirmation la première fois (voir `docs/DEPLOYMENT.md` pour
signer/notariser via `pkgbuild.sh` si vous disposez d'un certificat Apple
Developer).

## Compiler soi-même

Aucune dépendance externe au runtime : Go seul suffit, et il cross-compile
nativement vers Windows/macOS depuis Linux.

```bash
cd agent
GOOS=windows GOARCH=amd64 go build -o backup-agent.exe ./cmd/backup-agent
GOOS=darwin  GOARCH=arm64 go build -o backup-agent      ./cmd/backup-agent
```

## CI

`.github/workflows/build.yml` compile le serveur, génère l'installeur
Windows (NSIS, exécuté sur un runner Linux) et l'archive macOS à chaque
push, et publie une release GitHub avec les trois artefacts sur chaque tag
`vX.Y.Z`.
