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
N'importe quel écart compte comme une modification - plus gros, plus
petit, plus récent **ou plus ancien** (un fichier remis en place depuis
une vieille copie).

La date de modification stockée sur le NAS est celle du fichier au moment
de l'envoi (relue à l'ouverture, pas celle du scan) - sinon la comparaison
du run suivant porterait sur un état que le serveur ne détient pas, et le
fichier repartirait indéfiniment.

### L'angle mort de la seconde, et comment il est fermé

Les dates de modification ont une résolution d'une seconde. À elles
seules, elles manquent donc un cas réel : un fichier modifié à nouveau
**pendant la seconde même où l'agent l'a lu**, en gardant la même taille.
Taille identique, seconde identique : plus rien ne paraîtra jamais
différent, et la copie sur le NAS resterait périmée indéfiniment. C'est
une perte de données silencieuse - la seule défaillance qu'une sauvegarde
ne peut pas se permettre.

D'où la règle complémentaire : **tout fichier dont la date de modification
est postérieure ou égale au démarrage de la dernière sauvegarde réussie
est renvoyé sans condition**. Il a pu changer après avoir été lu, et on ne
peut pas prouver le contraire. Le coût est d'un transfert supplémentaire
pour la poignée de fichiers touchés au moment d'une sauvegarde, et la
règle s'éteint d'elle-même : au passage suivant, la date de la dernière
sauvegarde réussie les a dépassés et ils redeviennent ignorés.

### Un fichier séparé qui contient la date : l'index

Les deux paragraphes précédents décrivaient un correctif ajouté après
coup à une comparaison qui interrogeait le NAS à chaque fois
(`os.Stat` sur le fichier stocké). Ce n'est plus comme ça que ça marche.

`server/internal/filestore` tient son propre registre, dans un fichier cache
`.backup-index.json` à la racine du dossier de chaque machine (masqué par
le point, jamais dans l'arborescence visible Bureau/Documents) : pour
chaque fichier, la taille et la date de modification **exactement comme
l'agent les a annoncées**, à la nanoseconde, au moment où ce fichier a
réellement été écrit. `NeededFiles` compare contre ce registre - jamais
contre un nouvel appel `os.Stat` sur le fichier posé sur le NAS.

Cette indirection règle les deux problèmes à la fois :

- **Le NAS peut mentir sur la date sans que ça change rien.** Certains
  montages SMB/CIFS arrondissent ou perdent la date de modification qu'on
  leur demande d'écrire. Avec l'ancienne comparaison, ça rendait *chaque*
  fichier "modifié" à *chaque* sauvegarde - la machine retransférait tout
  son disque en boucle, sans rien dans les journaux pour l'expliquer.
  Vérifié en conditions réelles : après avoir trafiqué à la main la date
  du fichier stocké sur le "NAS" (`touch` vers l'année 2001), une
  sauvegarde sans changement continue d'envoyer 0 octet - et une vraie
  modification du même fichier est toujours détectée et envoyée.
- **La date que l'agent envoie est à la nanoseconde**, pas à la seconde :
  ça referme, à la source, la quasi-totalité des collisions que la règle
  `lastBackupStart` (ci-dessus) existait pour rattraper après coup. Les
  deux se cumulent : l'index rend une collision quasiment impossible, et
  `lastBackupStart` couvre le résidu.

**Comment le registre est tenu à jour**, en deux temps qui coûtent chacun
une seule opération par sauvegarde, jamais une par fichier :

1. `handleAgentPlan` note, dans un petit fichier `.backup-attente-<id>.json`
   propre à cette sauvegarde, la liste (seulement les fichiers demandés,
   pas toute la machine) de ce qu'il s'attend à recevoir.
2. `handleAgentFinishSnapshot` appelle `ConfirmUpdates` : pour chaque
   fichier de cette liste, il vérifie que la taille sur disque correspond
   bien à ce qui était annoncé - la taille, jamais la date stockée, pour
   ne pas réintroduire la dépendance à un `os.Stat` fiable qu'on vient
   d'éliminer - puis met à jour le registre avec la date exacte annoncée
   par l'agent. Un fichier prévu mais jamais réellement écrit (échec,
   verrouillage) garde son ancienne entrée : il sera correctement redemandé
   au tour suivant. Appelé quel que soit le statut final (succès, échec,
   annulé) : tout ce qui a vraiment atteint le disque doit arrêter d'être
   signalé comme manquant, même dans une sauvegarde par ailleurs ratée.

**Migration** : une machine qui n'a pas encore d'entrée confirmée pour un
fichier donné (typiquement juste après la mise à jour vers cette version)
retombe sur la comparaison par `os.Stat`, exactement comme avant - la
mise à jour n'oblige donc pas à retransférer une machine déjà sauvegardée
en entier. Un fichier qui ne bouge plus jamais n'a de toute façon rien à
perdre à rester sur cette ancienne comparaison ; c'est seulement le jour
où il est vraiment modifié, et renvoyé, qu'il obtient une entrée confirmée
et la protection complète.

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
- **Stockage utilisé** : calculé en marchant sur tout l'arbre du NAS -
  chaque appareil, chaque ancienne version - ce qui est bon marché sur un
  disque local mais lent sur un vrai montage réseau (CIFS/SMB), où chaque
  fichier rencontré coûte un aller-retour. Recalculé toutes les 5 minutes
  et écrit dans `settings.storage_used_bytes`/`storage_used_at` plutôt que
  calculé à la demande : `GET /api/dashboard/storage` ne fait qu'une
  lecture de ces colonnes, jamais un parcours du NAS sur le chemin de la
  requête. Un calcul a aussi lieu une fois au démarrage du serveur, avant
  d'entrer dans la boucle des tickers, pour qu'un redémarrage (chaque
  déploiement en fait un) n'affiche pas "0 o" pendant 5 minutes.
- **Espace disponible** : contrairement au stockage utilisé, ce n'est pas
  un parcours - un seul appel `statfs()` sur le volume qui porte la racine
  de stockage (`filestore.Store.FreeBytes`), aussi bon marché sur un
  montage réseau qu'en local. Recalculé à chaque tick (une fois par
  minute) et écrit dans `settings.storage_free_bytes`/`storage_free_at`,
  lu par le même `GET /api/dashboard/storage` que le stockage utilisé.
- **Prochaine sauvegarde (estimée)** : le tableau de bord affiche, par
  appareil, un compte à rebours (jour/heure/minute) calculé côté panneau à
  partir de `latest_snapshot.started_at` et de l'intervalle effectif de
  l'appareil (`effective_interval_minutes` dans la réponse de
  `GET /api/dashboard/devices` - la même résolution appareil/défaut que
  `models.EffectiveIntervalMinutes` utilise pour le point de configuration
  de l'agent). Une estimation assumée comme telle : le vrai calendrier,
  rattrapages et reprogrammations compris, est décidé par l'agent, dont ce
  chiffre côté serveur ne sait rien.
- **Proposition de reprogrammation à la reconnexion** : quand un appareil
  se reconnecte (`Hub.OnConnect`, câblé dans `cmd/backup-server/main.go` sur
  `offerRescheduleIfOverdue`), le serveur compare l'heure actuelle à la même
  estimation que ci-dessus (dernière sauvegarde réussie + intervalle
  effectif). S'il est en retard, il envoie `TypeOfferReschedule` sur le
  canal WebSocket. Contrairement au reste de cette section, ceci demande
  une petite modification côté agent : un nouveau cas dans le switch des
  messages entrants (`cmd/backup-agent/main.go`) qui appelle
  `offerCatchUpSlot`, la fonction déjà utilisée quand l'agent détecte
  lui-même une sauvegarde manquée ou échouée - donc sans nouvelle UI, juste
  un déclencheur supplémentaire pour l'assistant de reprogrammation
  existant. Un appareil jamais sauvegardé avec succès n'a rien dont être en
  retard, et n'est jamais concerné.

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
l'utilisateur - Apple l'interdit délibérément. C'est une limite de la
plateforme, pas du logiciel.

Deux couches détectent une autorisation manquante ou révoquée, parce
qu'elles ne voient pas le même symptôme :

- `scanner.CheckAccess` fait un simple `os.ReadDir` sur chaque dossier
  configuré avant de démarrer - mais **lister** ces quatre dossiers
  fonctionne déjà sans Accès complet au disque ; seule la **lecture du
  contenu** de chaque fichier l'exige. Cette vérification ne détecte donc
  qu'un dossier totalement bloqué (rare), pas le cas réel le plus fréquent.
- `backupjob.Run` compte, après coup, combien de fichiers l'OS a
  explicitement refusés (`os.IsPermission`, distinct d'un fichier modifié
  ou verrouillé) parmi ceux qu'il fallait envoyer. Une minorité est du
  bruit ordinaire ; une majorité (`isDiskAccessDenied`) est le signe d'une
  autorisation révoquée sur un dossier entier - le scan avait vu et
  chiffré les fichiers via `os.ReadDir`/`os.Stat` (d'où un total en Go
  correct malgré tout), mais presque aucun n'a pu être lu pour l'envoi.
  Dans ce cas la sauvegarde est explicitement marquée en échec
  (`ErrPermissionDenied`) plutôt que rapportée comme un succès qui n'a en
  réalité presque rien protégé, et l'agent envoie une notification native
  avec la marche à suivre.

Autre piège propre à cette plateforme : l'agent n'étant pas signé avec un
certificat Apple Developer, TCC a tendance à lier une autorisation aux
octets exacts du binaire - recompiler pour une mise à jour, même sans rien
changer de comportement, peut suffire à invalider le grant précédent.
`install.command` signe maintenant le binaire en ad-hoc
(`codesign --sign - --identifier com.backupcenter.agent`, gratuit, sans
compte développeur) pour donner à TCC une identité stable d'une mise à
jour à l'autre ; ça aide en pratique mais ne le garantit pas complètement
comme le ferait un vrai certificat Developer ID payant - une réinstallation
après une mise à jour plus profonde peut encore redemander l'autorisation.

Troisième piège, sans rapport avec TCC cette fois : `scanner.Walk` marche
chaque racine avec `filepath.WalkDir`, qui fait un `lstat` sur la racine
elle-même - si Bureau ou Documents est un **lien symbolique** plutôt qu'un
vrai dossier, `WalkDir` le voit comme "pas un répertoire" et en ressort
zéro fichier, silencieusement, sans la moindre erreur. C'est exactement ce
que produit iCloud Drive quand l'option "Bureau et Documents" est activée :
macOS remplace `~/Desktop` et `~/Documents` par des liens vers
`~/Library/Mobile Documents/com~apple~CloudDocs/...`, et une machine qui
sauvegardait 70 Go se retrouve à n'en trouver que ce qu'il reste dans les
dossiers non touchés (Téléchargements, Images). `Walk` résout maintenant
la racine avec `filepath.EvalSymlinks` avant de la marcher (tout en gardant
`root.Name` pour le nommage logique sur le NAS), sans changer le
comportement volontaire de ne jamais suivre un lien symbolique rencontré
*pendant* le parcours.

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

## Icône de la barre des tâches / barre de menu (Windows et macOS)

Le service expose une API de contrôle minimaliste en local
(`127.0.0.1:47812`, `startTrayControlAPI` dans `cmd/backup-agent`,
partagée par les deux plateformes) : état (date de dernière sauvegarde,
connexion), déclenchement manuel, reprogrammation. L'icône elle-même est
un processus séparé, lancé dans la session console par le même mécanisme
que l'assistant/la popup (`CreateProcessAsUser` sur Windows, `launchctl
asuser` sur macOS), qui parle à ce service exclusivement via cette API
HTTP locale. Son isolement en processus à part garantit qu'un problème
dans l'icône n'affecte jamais le service de sauvegarde lui-même. Une
reprogrammation locale ne fait que déplacer l'échéance planifiée
normale ; une commande du serveur (`backup_now`, `cancel`) est traitée
indépendamment dans la boucle WebSocket principale et n'est donc jamais
retardée par quoi que ce soit venant de l'icône - c'est ce qui garde le
serveur prioritaire par construction, pas par une règle de priorité
explicite à maintenir.

- **Windows** (`internal/tray`, `backup-agent.exe --tray`) : Win32 pur
  (`Shell_NotifyIconW`, fenêtre cachée, menu contextuel classique) via
  `syscall`, sans CGO. Comme sur macOS, ce processus n'est pas un enfant
  du service - chaque redémarrage du service (mise à jour, crash, arrêt/
  relance manuel) lançait auparavant une nouvelle icône sans jamais tuer
  la précédente, qui s'accumulaient indéfiniment. Le helper écrit
  maintenant son PID dans `%ProgramData%\BackupAgent\tray.pid` au
  démarrage ; `tray.KillRunningHelper` (appelée juste avant chaque
  lancement, et à la désinstallation) le lit et termine ce PID précis
  (`taskkill /F /PID`) avant d'en démarrer un nouveau.
- **macOS** (`internal/macmenubar`, `backup-agent --menubar`) : un
  `NSStatusItem` piloté directement via le runtime Objective-C
  (`objc_msgSend`, `objc_getClass`, `sel_registerName`, une classe créée
  dynamiquement à l'exécution pour porter l'action des trois entrées de
  menu cliquables) grâce à `github.com/ebitengine/purego`, qui gère les
  conventions d'appel (registres entiers vs. flottants) sans CGO. C'est ce
  qui permet de continuer à cross-compiler l'agent macOS depuis un
  toolchain Go Linux ordinaire, sans SDK Xcode ni machine Mac dans la
  chaîne de build - purego résout les frameworks (`libobjc.A.dylib`,
  `AppKit`) au démarrage du processus, sur la machine cible, pas à la
  compilation. La ligne "Dernière sauvegarde : ..." est rafraîchie toutes
  les 30s depuis une goroutine à part, en dehors du thread principal
  verrouillé par `[NSApp run]` - une simple mise à jour de propriété sur
  un `NSMenuItem` déjà existant, sans déclenchement de rendu synchrone, ce
  qui reste dans la pratique le raccourci standard des utilitaires Cocoa
  minimalistes pour éviter d'avoir à faire transiter chaque rafraîchissement
  par le thread principal. À la désinstallation (`macdaemon.Uninstall`),
  ce processus n'est pas un enfant du daemon et ne s'arrête donc pas tout
  seul quand celui-ci est déchargé - `pkill -f "backup-agent --menubar"`
  le termine explicitement (root peut signaler n'importe quel processus
  d'un autre utilisateur directement, sans passer par le détour
  `launchctl asuser` nécessaire pour en *lancer* un dans une session
  console). Sans ça l'icône serait restée visible jusqu'à son propre
  délai d'abandon (cinq minutes sans réponse du service).

  **Statut** : implémenté et vérifié à la compilation (cross-compilation
  `darwin/amd64` et `darwin/arm64`, `go vet` propre) depuis cet
  environnement Linux, mais jamais exécuté sur un vrai Mac - il n'y en a
  pas dans cette chaîne de build/CI. À valider en conditions réelles avant
  de la considérer stable ; des ajustements sont probables au premier
  retour terrain (voir le bug du `$HOME` non défini en mode service,
  découvert exactement de cette façon).

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
