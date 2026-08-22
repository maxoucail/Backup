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
