# Architecture

## Vue d'ensemble

```
        HTTP/WS                                     fichiers en clair
 ┌──────────────┐   enrôlement, plan,        ┌────────────────────┐      ┌─────────┐
 │ Agent Windows│──  envoi de fichiers ──────│                    │      │  NAS    │
 │  (Go, seul   │   ────────────────────────▶│  Serveur (Go)      │─────▶│ un      │
 │   binaire)   │◀── progression, config ────│  - panneau web     │      │ dossier │
 └──────────────┘                            │  - API REST        │      │ par PC  │
 ┌──────────────┐                            │  - hub WebSocket   │      └─────────┘
 │ Agent macOS  │◀──────────────────────────▶│  - SQLite          │
 │  (Go)        │                            │  - stockage fichier│
 └──────────────┘                            └────────────────────┘
```

Deux plans de communication séparés entre agent et serveur :

- **Plan de contrôle** (`/ws/agent`, WebSocket) : présence de l'appareil,
  commandes distantes (`backup_now`, `cancel`, `uninstall`), poussée de
  politique (`config`), remontée de progression et de logs. Léger,
  persistant, faible latence.
- **Plan de données** (`/api/agent/...`, HTTP) : établissement du plan
  incrémental, envoi des fichiers, cycle de vie des sauvegardes. Un flux
  HTTP classique par fichier, ce qui permet la parallélisation native
  (plusieurs fichiers à la fois) et des reprises simples.

Il n'y a **pas de plan de restauration** : les fichiers sont sur le NAS en
clair, on les récupère avec un explorateur de fichiers.

Ces deux plans tournent en plus sur des **ports séparés** au niveau
réseau (pas seulement des routes) : le panneau admin sur un port
(80 par défaut), le trafic agent (enrôlement, plans de contrôle et de
données ci-dessus) sur un autre (8420 par défaut) - `cmd/backup-server`
démarre deux `http.Server` distincts, chacun avec son propre
`http.ServeMux` peuplé par `api.RegisterPanel`/`api.RegisterAgent`. Ça
permet d'appliquer des règles de pare-feu différentes aux deux : le port
admin restreint à un VLAN de gestion, le port agent ouvert à tous les
VLAN où vivent des postes à sauvegarder. Voir la section **Ports** du
README pour le détail.

Garder ces deux plans séparés évite qu'un transfert de plusieurs Go bloque
la réactivité des commandes, et permet à plusieurs appareils de sauvegarder
en même temps sans qu'aucun ne bloque les autres (chaque connexion/requête
tourne dans sa propre goroutine).

## Stockage : des fichiers, pas un dépôt

`server/internal/filestore` écrit chaque fichier tel quel, à son propre
chemin, sous un dossier par machine :

```
<racine>/<Nom du PC>-<id court>/Bureau/rapport.docx        <- la sauvegarde à jour
<racine>/<Nom du PC>-<id court>/Documents/factures/2026.pdf
<racine>/<Nom du PC>-<id court>/_anciennes_versions/2026-08-20_14-30/Bureau/...
```

C'est le choix de conception central de ce logiciel, et il est délibéré :
**une sauvegarde qu'on ne peut relire qu'avec l'outil qui l'a écrite est
une sauvegarde qu'on risque de ne pas pouvoir relire**. Ici il n'y a ni
format propriétaire, ni index à reconstruire, ni dépendance au logiciel :
restaurer, c'est ouvrir le dossier de la machine et copier ce qu'on veut.

La version précédente est un vrai arbre complet, navigable, dans lequel
chaque fichier inchangé est un **lien physique** (`os.Link`) vers le
fichier du miroir : la place n'est occupée qu'une fois, quel que soit le
nombre de versions. Supprimer une ancienne version ne fait que retirer un
lien ; les données ne disparaissent qu'avec le dernier. Là où les liens
physiques ne sont pas disponibles (montages SMB/CIFS courants), le serveur
bascule sur une recopie **côté serveur** : plus de disque consommé, mais
toujours aucune retransmission réseau.

Deux détails d'implémentation portent la correction de l'ensemble :

- **Écriture par fichier temporaire + `rename`.** Le `rename` remplace
  l'entrée de répertoire, pas le contenu du fichier : l'ancien inode - vers
  lequel pointent toutes les versions précédentes - garde ses données.
  Écrire directement dans le fichier de destination réécrirait d'un coup
  toutes les versions qui le partagent, laissant un historique qui *paraît*
  intact mais ne contient plus qu'un seul état.
- **Validation des chemins (`RelPath`).** Un chemin arrive du réseau et
  sert directement à écrire sur disque : tout ce qui contient `..`, une
  lettre de lecteur, un caractère réservé ou le nom réservé
  `_anciennes_versions` est refusé, pas nettoyé silencieusement.

## Sauvegarde incrémentale sans empreintes

Le cycle d'une sauvegarde tient en quatre appels :

1. `POST /api/agent/snapshots` — demande un créneau (voir file d'attente).
2. `POST /api/agent/snapshots/{id}/plan` — l'agent annonce **tout** ce que
   la machine contient (chemin, taille, date de modification). Le serveur
   conserve d'abord l'état courant en nouvelle version, aligne le miroir
   sur ce que la machine n'a plus, puis répond avec la seule liste des
   fichiers dont il n'a pas déjà une copie identique.
3. `PUT /api/agent/files?path=…&mtime=…` — un appel par fichier demandé,
   le corps est le fichier brut.
4. `POST /api/agent/snapshots/{id}/finish` — clôture, libère le créneau,
   déclenche la rotation de rétention.

« Identique » = même taille et même date de modification. C'est ce que
font tous les outils incrémentaux : lire et hacher chaque fichier des deux
côtés à chaque passage coûterait bien plus cher que ce que ça économise.
La date de modification stockée sur le NAS est celle du fichier au moment
de l'envoi (relue à l'ouverture, pas celle du scan) - sinon la comparaison
du run suivant porterait sur un état que le serveur ne détient pas, et le
fichier repartirait indéfiniment.

L'endpoint `plan` est volontairement **sans état entre les appels** : tout
ce qu'il sait, il le lit sur le disque du NAS. Un serveur redémarré en
plein milieu ne perd donc rien d'autre que la sauvegarde en cours.

## File d'attente des sauvegardes

`server/internal/queue` sérialise les sauvegardes à l'échelle de la flotte.
Le serveur est le seul à savoir ce que fait chaque machine : c'est donc lui
qui arbitre, pas les agents entre eux. Un agent demande un créneau au
moment de créer sa sauvegarde ; si la limite (`max_concurrent_backups`,
réglable depuis le panneau, 1 par défaut) est atteinte, il est mis en file
FIFO et reçoit une réponse « en attente » — **volontairement pas une
erreur** : aucun snapshot n'est créé, donc une machine qui patiente ne
laisse aucune sauvegarde en échec derrière elle. Dès qu'un créneau se
libère, le serveur envoie un `backup_now` au suivant.

Trois cas dégradés sont traités explicitement, parce que chacun bloquerait
sinon toute la flotte :

- **Machine hors ligne au moment de son tour** : le créneau lui est
  repris et proposé au suivant, au lieu d'être gaspillé.
- **Machine qui disparaît en pleine sauvegarde** : la déconnexion du
  WebSocket libère son créneau (`Hub.OnDisconnect`). Attention au cas
  d'une simple reconnexion : l'ancienne connexion se ferme *après* que la
  nouvelle soit enregistrée, donc seule la fermeture de la connexion
  *courante* compte — sinon on libérerait le créneau d'une machine encore
  en train de sauvegarder.
- **Coupure de courant** (ni fin, ni déconnexion propre) : un balayage
  périodique libère les créneaux détenus depuis plus de 12 h et clôture
  les snapshots restés « en cours ».

Côté agent, attendre est normal et généralement court. Mais si le tour ne
vient pas au bout de 90 minutes, l'agent considère que la sauvegarde
n'aura pas lieu et propose un créneau à l'utilisateur via le même
assistant que pour une sauvegarde manquée — plutôt que de sauter
silencieusement le cycle.

## Rétention et purge

`server/internal/scheduler` fait tourner, en tâche de fond :

- **Rotation de rétention** (après chaque sauvegarde réussie, et en
  balayage périodique de sécurité) : au-delà du nombre d'états à conserver
  pour un appareil (réglable par appareil ou par défaut global, **minimum
  2**), les plus anciennes versions sont supprimées.

  L'ordre des opérations est ce qui garantit qu'il reste toujours une
  sauvegarde utilisable : la nouvelle version est créée **avant** que le
  miroir ne soit modifié (dans `plan`), et la rotation ne tourne
  qu'**après** la fin de la sauvegarde (dans `finish`). Il n'existe donc
  aucun instant où la plus ancienne a été supprimée alors que la plus
  récente est encore en cours d'écriture.

  Le minimum de 2 n'est pas arbitraire : avec un seul état conservé, la
  seule copie existante est le miroir en train d'être écrasé - un fichier
  corrompu sur le PC se propage au NAS sans rien vers quoi revenir.

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
(« Sauvegarder maintenant ») - les sauvegardes planifiées
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
service/daemon, efface sa configuration locale (identifiants) et quitte.
Le serveur supprime ensuite l'appareil en base **et son dossier complet
sur le NAS**, versions précédentes comprises (en tâche de fond : effacer
plusieurs Go sur un montage réseau ne doit pas faire pendre le panneau).
Rien n'est partagé entre machines - chacune a son propre arbre - donc il
n'y a rien à collecter ensuite. Que l'agent ait pu être notifié ou non - si
l'appareil était hors ligne au
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
`cancel`) est traitée indépendamment dans la boucle WebSocket principale
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
- Concurrence native (goroutines) : chaque connexion agent, chaque envoi
  de fichier, chaque requête HTTP tourne dans sa propre goroutine sans le
  verrou global d'un interpréteur - plusieurs appareils sauvegardent
  réellement en parallèle sans configuration particulière.
- Cross-compilation native vers Windows et macOS depuis Linux
  (`GOOS=windows|darwin go build`), ce qui a permis de générer et de
  tester les trois binaires (serveur, agent Windows, agent macOS) dans cet
  environnement de développement Linux sans jamais avoir besoin d'une
  machine Windows ou Mac.
