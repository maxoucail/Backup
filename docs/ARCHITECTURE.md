# Architecture

## Vue d'ensemble

```
        HTTPS/WS                                    fichiers
 ┌──────────────┐   enrôlement, chunks,      ┌────────────────────┐
 │ Agent Windows│──  manifestes, commandes ──│                    │
 │  (Go, seul   │   ────────────────────────▶│  Serveur (Go)      │
 │   binaire)   │◀── progression, config ─────│  - panneau web     │
 └──────────────┘                             │  - API REST        │
 ┌──────────────┐                             │  - hub WebSocket   │
 │ Agent macOS  │◀───────────────────────────▶│  - SQLite          │
 │  (Go)        │                             │  - stockage chunké │
 └──────────────┘                             └────────────────────┘
```

Deux plans de communication séparés entre agent et serveur :

- **Plan de contrôle** (`/ws/agent`, WebSocket) : présence de l'appareil,
  commandes distantes (`backup_now`, `restore`, `cancel`), poussée de
  politique (`config`), remontée de progression et de logs. Léger,
  persistant, faible latence.
- **Plan de données** (`/api/agent/...`, HTTP) : upload/download de blocs,
  soumission de manifeste, cycle de vie des sauvegardes. Un flux HTTP
  classique par opération, ce qui permet la parallélisation native (upload
  de plusieurs blocs à la fois) et des reprises simples.

Garder ces deux plans séparés évite qu'un transfert de plusieurs Go bloque
la réactivité des commandes, et permet à plusieurs appareils de sauvegarder
en même temps sans qu'aucun ne bloque les autres (chaque connexion/requête
tourne dans sa propre goroutine).

## Stockage adressé par contenu

`server/internal/storage` : chaque bloc de données est écrit une seule fois
sur disque, nommé par son SHA-256 (`storage/chunks/ab/cd/<hash>`). Un
fichier plus petit que la taille de bloc configurée (16 Mo par défaut) est
un bloc unique ; un fichier plus gros est découpé séquentiellement.

Une sauvegarde (« snapshot ») est un manifeste JSON
(`storage/manifests/<device_id>/<snapshot_id>.json`) qui liste, pour chaque
fichier, son chemin relatif, sa taille, sa date de modification, son
empreinte globale et la liste des blocs qui le composent. Reconstruire un
fichier au moment d'une restauration, c'est simplement retélécharger ses
blocs dans l'ordre.

Cette conception rend l'incrémental **gratuit** : un fichier identique
resterait toujours le même hash, donc jamais réuploadé - y compris s'il
existe déjà sur le serveur via un autre appareil.

## Rétention et purge

`server/internal/scheduler` fait tourner, en tâche de fond :

- **Rotation de rétention** (après chaque sauvegarde réussie, et en
  balayage périodique de sécurité) : au-delà du nombre de sauvegardes à
  conserver pour un appareil (réglable par appareil ou par défaut global),
  les plus anciennes sont supprimées puis un garbage collector parcourt le
  dépôt de blocs et supprime tout bloc qu'aucun manifeste restant ne
  référence plus.
- **Purge des événements** : la table `events` est bornée par ancienneté
  (jours) et par nombre de lignes maximum, tous deux réglables depuis
  **Paramètres**. Sans cette purge, une base SQLite accumulerait
  indéfiniment des lignes de journal.
- **Détection de déconnexion** : un appareil qui ne s'est pas manifesté
  depuis 10 minutes bascule à "hors ligne" même si la coupure réseau a été
  brutale (pas de fermeture propre du WebSocket).

## Agent : identification et politique

À l'enrôlement, le serveur génère un identifiant d'appareil (UUID) et un
secret à haute entropie ; seul le hash SHA-256 du secret est stocké côté
serveur. L'agent conserve les deux dans sa configuration locale
(par utilisateur, jamais avec des droits administrateur nécessaires après
l'installation) et les envoie en en-têtes (`X-Device-Id`,
`X-Device-Secret`) sur chaque requête HTTP, et à la connexion WebSocket.

La politique effective (intervalle, rétention, dossiers surveillés) est
résolue côté serveur (valeur par appareil si définie, sinon valeur globale)
et transmise à l'agent de deux façons complémentaires : poussée immédiate
via WebSocket dès qu'un opérateur modifie la politique dans le panneau, et
sondage HTTP de secours toutes les 15 minutes au cas où l'agent était
déconnecté au moment de la modification.

## Popup de progression sans dépendance GUI

Plutôt qu'une boîte de dialogue native (fragile à porter sans CGO sur
Windows *et* macOS), l'agent démarre un mini-serveur HTTP local
(`127.0.0.1:<port éphémère>`) qui sert une page de progression, et
l'ouvre dans le navigateur par défaut du système (`os/exec` vers `open` /
`rundll32 url.dll` / `xdg-open`), accompagnée d'une notification native
best-effort (`osascript`/`msg.exe`/`notify-send`). Le même mécanisme sert
à l'assistant de première configuration (saisie de l'adresse serveur et de
la clé d'enrôlement). Approche 100 % portable, sans CGO, sans dépendance
d'interface graphique.

Ce popup n'apparaît que pour un déclenchement manuel ou distant
(« Sauvegarder maintenant », « Restaurer ») - les sauvegardes planifiées
tournent silencieusement, comme la plupart des outils de sauvegarde grand
public.

## Service système et session interactive

L'agent installé (via `backup-agent install`, ce que font les installeurs)
tourne comme un vrai service : Service Windows sous LocalSystem
(`internal/svcmode`, `golang.org/x/sys/windows/svc`) ou LaunchDaemon macOS
sous root (`internal/macdaemon`). Ça lui donne trois propriétés qui ne sont
pas obtenables avec un simple programme lancé au login utilisateur :
démarrage dès l'écran de connexion (avant toute session ouverte), reprise
automatique après un crash (recovery actions `sc.exe` côté Windows,
`KeepAlive` côté launchd), et impossibilité pour un utilisateur standard de
l'arrêter (seul un administrateur peut piloter un service/daemon système -
c'est le mécanisme natif de l'OS, pas une protection ad hoc).

La contrepartie : un service n'a pas de session graphique à lui. Pour
afficher l'assistant de configuration ou une popup de progression, le
service lance un petit processus **dans la session de l'utilisateur
connecté à la console** :

- Windows : `internal/winsession` implémente le enchaînement classique
  `WTSGetActiveConsoleSessionId` → `WTSQueryUserToken` → `DuplicateTokenEx`
  → `CreateEnvironmentBlock` → `CreateProcessAsUser`, documenté par
  Microsoft pour exactement ce cas d'usage.
- macOS : `internal/macdaemon` utilise `launchctl asuser <uid> sudo -u
  <user> <commande>`, la technique standard pour qu'un daemon root fasse
  apparaître quelque chose dans la session graphique d'un utilisateur.

Dans les deux cas, le sous-processus lancé est le même binaire invoqué avec
`--show-url <url>` : il se contente d'ouvrir le navigateur sur l'URL locale
que le service héberge déjà (le serveur HTTP de l'assistant/de la popup
tourne toujours côté service - seule l'ouverture du navigateur doit
traverser la frontière de session).

De la même façon, `userctx.HomeDir` (normalement `os.UserHomeDir`) est
remplacé au démarrage du service par une résolution du profil de
l'utilisateur console (`GetUserProfileDirectoryW` côté Windows, convention
`/Users/<nom>` côté macOS) - sinon le service sauvegarderait le profil du
compte système lui-même, pas celui de l'utilisateur.

**Limite connue et assumée** : le code Windows (appels WTS/CreateProcessAsUser)
et macOS (`launchctl asuser`) a été validé par compilation croisée depuis
Linux (`GOOS=windows|darwin go build` et `go vet` passent), mais pas exécuté
sur une vraie machine Windows/macOS faute d'accès à ce matériel pendant le
développement. À tester sur poste réel avant déploiement à grande échelle.

## Accès complet au disque sur macOS (TCC)

Depuis macOS Catalina, l'accès à Bureau/Documents/Téléchargements/Images
est protégé par TCC (Transparency, Consent and Control) : même un daemon
root n'y a pas accès sans une autorisation explicite accordée par un humain
dans Réglages Système → Confidentialité et sécurité → Accès complet au
disque. Aucun installeur ne peut accorder cette permission à la place de
l'utilisateur - Apple l'interdit délibérément. `scanner.CheckAccess`
détecte les dossiers inaccessibles (erreur de permission distincte d'un
dossier absent) et l'agent envoie une notification native + un événement
dans le panneau pour guider l'opérateur. C'est une limite de la plateforme,
pas du logiciel.

## Sauvegarde manquée et rattrapage programmé

Chaque sauvegarde planifiée réussie enregistre la date de la prochaine
échéance (`NextScheduledAt`, persisté localement). Au démarrage, si cette
échéance est dépassée de plus de 10 minutes, l'agent en déduit que la
machine était éteinte ou en veille pendant la fenêtre prévue et ouvre
l'assistant de reprogrammation (`internal/reschedulewizard`, même
mécanisme de page web locale) : l'opérateur choisit un créneau précis
(aujourd'hui à telle heure, ou un autre jour) ou renonce et laisse le
cycle normal reprendre.

Une fois un rattrapage programmé, un unique goroutine (`internal/progressui`
+ le minuteur dans `cmd/backup-agent`) surveille l'échéance : notification
native 15 minutes avant (« laissez votre ordinateur allumé »), ouverture
d'une popup avec compte à rebours en direct 5 minutes avant, puis
déclenchement automatique de la sauvegarde à l'heure choisie - la même
popup transitionne du compte à rebours vers la barre de progression sans
se refermer. Le planificateur normal est explicitement mis en pause tant
que cette décision n'est pas prise, pour ne jamais lancer une sauvegarde
« dans le dos » de l'opérateur pendant qu'il regarde l'assistant.

## Décommissionnement à distance

Depuis la fiche d'un appareil, l'action « Décommissionner » exige une
confirmation native puis la ressaisie exacte du nom de l'appareil (défense
en profondeur côté serveur aussi : la requête est rejetée si le nom ne
correspond pas). Si l'appareil est connecté, le serveur lui envoie une
commande `uninstall` sur le canal WebSocket ; l'agent désenregistre son
service/daemon, efface sa configuration locale (identifiants, cache de
manifeste) et quitte. Le serveur supprime ensuite l'appareil et ses
sauvegardes côté base et déclenche un garbage collection du dépôt de blocs,
que l'agent ait pu être notifié ou non - si l'appareil était hors ligne au
moment du décommissionnement, sa prochaine tentative de connexion recevra
une erreur d'authentification (secret révoqué) que l'agent interprète comme
« je ne suis plus reconnu » : il efface alors sa propre configuration et
rouvre l'assistant de première configuration, prêt à être réenrôlé avec une
nouvelle clé.

## Dossiers redirigés (Windows)

`internal/knownfolders` lit `HKCU\...\Explorer\User Shell Folders` (ou
`HKEY_USERS\<SID>\...` depuis le service, `HKCU` y désignant le compte
LocalSystem et non l'utilisateur console) pour résoudre l'emplacement réel
de Bureau/Documents/Téléchargements/Images, y compris quand ils ont été
déplacés vers un autre disque. C'est un complément, pas un remplacement,
au choix manuel de dossiers depuis le panneau (`scanner.ResolveRoots`
accepte déjà des chemins absolus arbitraires) : par défaut l'agent trouve
tout seul le bon disque, et l'opérateur peut toujours forcer des chemins
précis par appareil si besoin.

## Icône de la barre des tâches (Windows)

Le service expose une API de contrôle minimaliste en local
(`127.0.0.1:47812`, `internal/tray` côté client) : état (date de dernière
sauvegarde, connexion), déclenchement manuel, reprogrammation. L'icône
elle-même (`backup-agent.exe --tray`) est un processus séparé, lancé dans
la session console par le même mécanisme `CreateProcessAsUser` que
l'assistant/la popup - implémenté en Win32 pur (`Shell_NotifyIconW`,
fenêtre cachée, menu contextuel), sans CGO. Son isolement en processus à
part garantit qu'un problème dans l'icône n'affecte jamais le service de
sauvegarde lui-même. Une reprogrammation locale ne fait que déplacer
l'échéance planifiée normale ; une commande du serveur (`backup_now`,
`restore`) est traitée indépendamment dans la boucle WebSocket principale
et n'est donc jamais retardée par quoi que ce soit venant de l'icône -
c'est ce qui garde le serveur prioritaire par construction, pas par une
règle de priorité explicite à maintenir.

**Non implémenté dans cette passe** : l'équivalent macOS (icône de barre
de menu) nécessiterait CGO (NSStatusBar), incompatible avec la
cross-compilation depuis Linux sans machine Mac ; à envisager comme job CI
dédié tournant sur un runner macOS si besoin.

## Téléchargement en libre-service et test de stockage

`server/internal/api/downloads.go` sert un dossier `downloads/` sous le
répertoire de données du serveur : liste (`GET /api/downloads`) et
téléchargement (`GET /downloads/{nom}`) sont volontairement non
authentifiés - un installeur n'est pas une donnée sensible, et exiger un
compte pour le récupérer irait à l'encontre du but (n'importe quel poste
à sauvegarder doit pouvoir se le procurer). Seuls l'upload et la
suppression, depuis **Paramètres**, exigent une session admin. Le nom de
fichier est toujours réduit à sa base (`filepath.Base`) et revérifié comme
restant sous `downloads/` avant tout accès disque, contre la traversée de
chemin.

Le test de connectivité du stockage (`POST /api/settings/test-storage`)
écrit un fichier témoin, attend 6 secondes, le relit puis le supprime -
un délai délibéré pour distinguer un montage NAS qui accepte l'écriture
mais la perd (déconnexion, cache local non synchronisé) d'un chemin
réellement fiable, plutôt qu'un simple test d'écriture instantanée qui ne
détecterait pas ce cas.

## Pourquoi Go plutôt que Python/Node

- Binaire statique unique par plateforme : aucune dépendance runtime à
  installer sur le serveur Debian ni sur les postes clients.
- Empreinte mémoire faible et stable (quelques Mo), démarrage instantané.
- Concurrence native (goroutines) : chaque connexion agent, chaque upload
  de bloc, chaque requête HTTP tourne dans sa propre goroutine sans le
  verrou global d'un interpréteur - plusieurs appareils sauvegardent
  réellement en parallèle sans configuration particulière.
- Cross-compilation native vers Windows et macOS depuis Linux
  (`GOOS=windows|darwin go build`), ce qui a permis de générer et de
  tester les trois binaires (serveur, agent Windows, agent macOS) dans cet
  environnement de développement Linux sans jamais avoir besoin d'une
  machine Windows ou Mac.
