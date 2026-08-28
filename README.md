# Backup Center

Système de sauvegarde centralisé : un **serveur** à installer sur votre
NAS/Debian 13, avec panneau d'administration web, et des **agents** légers
pour Windows 11 et macOS qui sauvegardent automatiquement les dossiers
utilisateur (Bureau, Documents, Images, Téléchargements) vers ce serveur,
de façon incrémentale.

**Les fichiers sont stockés en clair sur le NAS**, dans un dossier par
ordinateur qui reproduit l'arborescence de la machine :

```
/mnt/nas/backups/
└── PC-Max-2e3e4bb2/                 <- un dossier par ordinateur
    ├── Bureau/
    │   └── rapport.docx             <- la sauvegarde à jour
    ├── Documents/
    │   └── factures/2026.pdf
    └── _anciennes_versions/
        ├── 2026-08-20_14-30/        <- l'état précédent, complet
        └── 2026-08-19_14-30/
```

Pour restaurer : ouvrez le dossier de la machine, copiez ce dont vous avez
besoin, collez-le sur le poste. Il n'y a **pas de fonction de
restauration** dans le logiciel, et c'est délibéré : pas de format
propriétaire, pas d'index à reconstruire, rien qui exige que ce logiciel
tourne encore le jour où vous en avez besoin.

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

- **Sauvegarde incrémentale** : l'agent annonce au serveur tout ce que la
  machine contient (chemin, taille, date de modification) ; le serveur
  compare avec ce qu'il a déjà sur le NAS et ne redemande que les fichiers
  nouveaux ou modifiés. Une photothèque de 4 Go inchangée n'est jamais
  retransférée, ni réécrite.
- **Un fichier modifié revient toujours** : n'importe quel écart de taille
  ou de date compte comme une modification - plus gros, plus petit, plus
  récent, **ou plus ancien** (un fichier remis en place depuis une vieille
  copie). Et parce que les dates n'ont qu'une résolution d'une seconde, un
  fichier modifié pendant la seconde même où l'agent l'a lu serait
  invisible : tout fichier daté au démarrage de la dernière sauvegarde
  réussie ou après est donc renvoyé d'office, une fois. Aucune
  modification ne peut rester coincée sur le poste.
- **Anciennes versions quasi gratuites** : avant chaque mise à jour, l'état
  courant est conservé dans `_anciennes_versions/<date>/`. Chaque version
  est une arborescence complète et navigable, mais un fichier inchangé y
  est un **lien physique** : il n'occupe la place qu'une seule fois, quel
  que soit le nombre de versions qui le référencent. (Sur un montage
  SMB/CIFS qui refuse les liens physiques, le serveur recopie côté serveur
  - plus de disque, mais toujours aucun transfert réseau.)
- **Rétention rotative, jamais à vide** : on choisit combien d'états garder
  par appareil (**minimum 2** : la sauvegarde à jour + au moins une version
  précédente). La nouvelle version est créée **avant** que quoi que ce soit
  ne soit écrasé, et la plus ancienne n'est supprimée qu'**après** la fin
  de la nouvelle sauvegarde : il reste toujours au moins une sauvegarde
  intacte, à tout instant.
- **Identification définitive** : au premier démarrage, l'agent demande
  l'adresse du serveur et une clé d'enrôlement à usage unique générée
  depuis le panneau. Une fois connecté, l'appareil est administré à
  distance en permanence (renommage, politique, sauvegarde à la demande)
  sans jamais redemander la clé.
- **Base de données bornée** : SQLite stocke les comptes, appareils,
  sauvegardes et un historique d'événements ; ce dernier est purgé
  automatiquement (par ancienneté et par nombre de lignes maximum,
  réglable) pour que la base ne grossisse pas indéfiniment.
- **Popup de progression à distance** : un déclenchement manuel ou distant
  (« Sauvegarder maintenant ») ouvre sur l'écran de l'appareil une page
  locale avec barre de progression et temps restant.
  Les sauvegardes planifiées, elles, tournent silencieusement en arrière-plan.
- **Service système, pas une appli qu'on ferme** : l'agent s'installe comme
  un vrai service (Service Windows / LaunchDaemon macOS), démarre dès
  l'écran de connexion et ne peut être arrêté que par un administrateur -
  exactement comme n'importe quel agent de sauvegarde professionnel.
- **File d'attente entre machines** : par défaut une seule machine
  sauvegarde à la fois (réglable dans **Paramètres → Sauvegardes
  simultanées maximum**). Les autres attendent leur tour et démarrent
  automatiquement dès qu'un créneau se libère — sans échec ni sauvegarde
  fantôme. Une machine hors ligne au moment de son tour est sautée, et un
  poste qui disparaît en pleine sauvegarde libère son créneau plutôt que
  de bloquer toute la flotte.
- **Sauvegarde manquée = on vous demande, pas on ignore** : si le poste
  était éteint à l'heure prévue, l'agent le détecte au redémarrage et
  propose de reprogrammer la sauvegarde manquée à une heure précise
  (aujourd'hui ou un autre jour), avec un rappel 15 min avant et un compte
  à rebours affiché 5 min avant le lancement automatique. Même proposition
  si une sauvegarde planifiée échoue, ou si le tour de la machine dans la
  file d'attente n'est jamais venu.
- **Décommissionnement à distance** : depuis la fiche d'un appareil, un
  administrateur peut le décommissionner définitivement (double
  confirmation, y compris ressaisir le nom de l'appareil) ; l'agent reçoit
  l'ordre de se désinstaller et efface ses identifiants locaux. L'appareil
  peut être réenrôlé plus tard avec une nouvelle clé.
- **Dossiers redirigés détectés automatiquement (Windows)** : si Bureau,
  Documents, Téléchargements ou Images ont été déplacés vers un autre
  disque (clic droit → Propriétés → Emplacement), l'agent lit le registre
  pour sauvegarder le bon dossier plutôt que de supposer qu'il est resté
  sous `C:\Users\...`. On peut aussi forcer des chemins précis par
  appareil depuis le panneau, y compris sur un autre disque.
- **Emplacement logique sur le NAS** : un fichier est rangé sous le nom de
  son dossier (`Téléchargements/facture.pdf`), jamais sous son emplacement
  physique. Si l'utilisateur déplace Téléchargements sur un autre disque,
  la sauvegarde reste au même endroit sur le NAS au lieu de repartir de
  zéro sous `E/Téléchargements`. Seuls les chemins personnalisés hors
  dossiers utilisateur sont rangés sous `_outside/`.
- **Le panneau vous dit où aller** : la fiche d'un appareil affiche le
  chemin exact de son dossier sur le NAS (avec un bouton « Copier le
  chemin »), la liste des anciennes versions conservées avec leur taille,
  et un bouton pour en supprimer une.
- **Icône dans la barre des tâches (Windows)** : affiche la date de la
  dernière sauvegarde et permet de sauvegarder maintenant ou de
  reprogrammer la prochaine sauvegarde - sans jamais bloquer un
  déclenchement fait depuis le serveur, qui reste toujours prioritaire.
- **Page de téléchargement en libre-service** : l'administrateur dépose
  l'installeur Windows et l'archive macOS une fois dans le panneau ; tout
  le monde peut ensuite les récupérer sur `/download`, sans compte,
  installer en un double-clic et ne saisir que l'adresse du serveur et une
  clé.
- **Test de connectivité du stockage** : avant de confirmer un chemin NAS,
  le panneau y écrit un fichier témoin, attend 6 secondes, vérifie qu'il
  est toujours lisible puis le supprime - un montage instable est détecté
  avant que de vraies sauvegardes n'y échouent.

Voir `docs/ARCHITECTURE.md` pour le détail du protocole et des choix de
conception, et `docs/DEPLOYMENT.md` pour l'installation pas à pas.

## Ports

Le serveur écoute sur **deux ports séparés**, exprès, pour permettre des
règles de pare-feu/inter-VLAN différentes :

| Port | Contenu | Qui doit pouvoir y accéder |
|---|---|---|
| **80** (`BACKUP_SERVER_PANEL_PORT`) | Panneau admin : connexion, tableau de bord, gestion des appareils/paramètres | Uniquement votre réseau/VLAN d'administration |
| **8420** (`BACKUP_SERVER_AGENT_PORT`) | Trafic agents : enrôlement, envoi des fichiers, WebSocket de contrôle, page `/download` | Tous les VLAN où se trouvent les postes à sauvegarder |

Un agent ne parle jamais au port 80 : la clé d'enrôlement générée dans le
panneau embarque déjà l'adresse du port 8420. Les deux ports sont
réglables via ces variables d'environnement (voir
`server/systemd/backup-server.service`) ; le service systemd fourni
obtient le droit de se lier au port 80 sans tourner en root
(`AmbientCapabilities=CAP_NET_BIND_SERVICE`).

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

1. Dans le panneau, **Paramètres → Installeurs téléchargeables** : déposez
   une fois `BackupAgentSetup-<version>.exe` et l'archive macOS (récupérables sur les
   releases GitHub ou compilés vous-même, voir plus bas).
2. **Paramètres → Enrôler un nouvel appareil** : générez une clé. Le
   panneau affiche alors le lien `/download` et la clé à transmettre à
   l'utilisateur.
3. Sur le poste à sauvegarder : ouvrez `/download`, téléchargez
   l'installeur correspondant, double-cliquez. Au premier lancement,
   l'agent ouvre une page dans le navigateur : renseignez l'adresse du
   serveur et la clé - une confirmation de connexion s'affiche
   immédiatement. L'appareil apparaît alors dans le panneau et est
   administré à distance.

### Agent Windows

Téléchargez `BackupAgentSetup-<version>.exe` (généré par la CI, ou compilé localement
avec `agent/packaging/windows/build.sh`) et exécutez-le **en administrateur**
(l'installeur le demande). Il enregistre un vrai Service Windows qui démarre
au boot et se relance seul en cas d'incident.

### Agent macOS

Téléchargez et décompressez `BackupAgent-macOS-*.tar.gz`, puis double-cliquez
sur `install.command` (mot de passe administrateur demandé via `sudo`). Il
enregistre un LaunchDaemon système. **Important** : macOS protège Bureau/
Documents/Téléchargements/Images (TCC) - autorisez « backup-agent » dans
Réglages Système → Confidentialité et sécurité → Accès complet au disque,
sinon ces dossiers ne pourront pas être lus (l'agent vous le signale par une
notification). Le binaire n'étant pas signé par défaut, voir
`docs/DEPLOYMENT.md` pour signer/notarier via `pkgbuild.sh` si vous disposez
d'un certificat Apple Developer.

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
