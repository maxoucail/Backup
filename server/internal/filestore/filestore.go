// Package filestore keeps every backup as ordinary files and folders on
// the NAS, laid out so a human can restore by hand:
//
//	<root>/<Nom du PC>/Bureau/rapport.docx        <- la sauvegarde à jour
//	<root>/<Nom du PC>/Documents/...
//	<root>/<Nom du PC>/_anciennes_versions/2026-08-20_14-30/Bureau/...
//
// Restoring is opening the machine's folder in a file explorer and copying
// what you need back. There is no proprietary format, nothing that needs
// this software to be running to read a file back - which is the whole
// point: a backup you can only read with the tool that wrote it is a
// backup you might not be able to read. (One hidden bookkeeping file sits
// alongside each machine's folder - see IndexFileName below - but it is
// never needed to read the actual files; only to decide what needs
// re-sending on the next backup.)
//
// The current backup sits at the top level because that's what gets
// restored in practice. Previous versions live in _anciennes_versions and
// exist for the case where the live mirror already picked up a bad change.
//
// # Space and bandwidth
//
// Each version directory is a *complete* tree, but a file that hasn't
// changed is hard-linked rather than copied, so it occupies disk space
// only once no matter how many versions list it. Deleting an old version
// just drops one link; the data goes away when the last version
// referencing it does. Where hard links aren't available (some SMB/CIFS
// mounts), the code falls back to copying server-side - more disk, but
// still no re-transfer over the network.
//
// Over the network only genuinely new or modified files move: the agent
// announces what it has, and Plan replies with the subset the server
// actually needs.
//
// # Detecting a modification correctly
//
// Deciding "has this file changed" never trusts a fresh stat() of the
// copy sitting on the NAS. Two independent reasons:
//
//  1. Some NAS/SMB mounts don't give back the exact modification time
//     they were asked to store - rounded to the nearest 1 or 2 seconds,
//     or silently dropped altogether on some FAT-backed shares. If the
//     comparison depended on that round trip, every file would look
//     "changed" on every single backup, forever, and a backup meant to
//     save bandwidth would instead re-upload the whole machine each time.
//  2. Even a perfectly faithful filesystem only has one-second
//     resolution here. A file edited twice within the same second the
//     agent first read it would keep an identical size and an identical
//     second on both edits - genuinely indistinguishable from an
//     untouched file, and it would stay stale on the NAS forever. Silent
//     data loss, the one failure a backup must not have.
//
// Instead this package keeps its own record, IndexFileName: for every
// file it holds, exactly what the agent reported for it - size and
// modification time, at full precision - at the moment it was actually
// written. That record is this store's own memory of what it has, not a
// question put to a filesystem that might answer differently than it was
// told, and it closes both gaps: the comparison never depends on what the
// NAS reports back, and the modification time it compares against came
// straight from the agent's own clock at full (nanosecond) precision, not
// truncated to a second.
package filestore

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// VersionsDirName holds previous versions inside a machine's folder. The
// leading underscore keeps it sorted away from the real folders (Bureau,
// Documents...) in a file explorer, and names it plainly enough that
// nobody wonders what it is.
const VersionsDirName = "_anciennes_versions"

// versionTimeLayout names version directories in a form that sorts
// chronologically as plain text and reads unambiguously in any locale.
const versionTimeLayout = "2006-01-02_15-04"

// IndexFileName is this store's own record of what it holds for a
// machine: for every file, the size and modification time - at full
// precision, exactly as the agent announced it - at the moment it was
// last actually written. See the package doc for why comparisons are
// made against this rather than against a fresh stat() of the NAS copy.
//
// A dot-prefixed name so it stays out of the way in a file explorer
// (hidden by default on every platform this backs up), and reserved like
// _anciennes_versions: nothing an agent announces may write to this name.
const IndexFileName = ".backup-index.json"

// pendingFilePrefix names the small per-backup record of which files this
// run intends to update (see SavePendingUpdates / ConfirmUpdates). Also
// reserved.
const pendingFilePrefix = ".backup-attente-"

// reservedNoDot and reservedPendingPrefixNoDot are IndexFileName and
// pendingFilePrefix as they'd read after SanitizeSegment strips a leading
// dot - which is exactly what happens to any agent-supplied path before
// isReservedName ever sees it (see RelPath). Checked in addition to the
// literal dotted forms so an agent can't create something that reads as
// this package's own bookkeeping even without literally colliding with
// it; the literal forms still matter for isReservedName's other callers
// (SnapshotCurrent, PruneRemoved), which check real directory entries on
// disk - written directly by this package, dot and all, never through
// SanitizeSegment.
const reservedNoDot = "backup-index.json"
const reservedPendingPrefixNoDot = "backup-attente-"

// isReservedName reports whether name is one of this package's own
// bookkeeping files/folders rather than something a machine's files could
// legitimately be called.
func isReservedName(name string) bool {
	if strings.EqualFold(name, VersionsDirName) {
		return true
	}
	if strings.EqualFold(name, IndexFileName) || strings.EqualFold(name, reservedNoDot) {
		return true
	}
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, pendingFilePrefix) || strings.HasPrefix(lower, reservedPendingPrefixNoDot)
}

type Store struct {
	Root string

	// linkMu guards the one-time hard-link capability probe.
	linkMu       sync.Mutex
	linkChecked  bool
	linkSupport  bool
	linkWarnOnce sync.Once
}

func New(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("chemin de stockage vide")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("création du stockage %s: %w", root, err)
	}
	return &Store{Root: root}, nil
}

// SanitizeSegment turns one path component into something safe to place on
// disk: no separators, no drive letters, no "..", no reserved characters.
// Everything the agent sends is untrusted input that ends up as a
// filesystem path, so this is a security boundary, not tidiness.
func SanitizeSegment(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			return '_'
		}
		if r < 32 {
			return '_'
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	return s
}

// DeviceDirName is the folder a machine's files live in. The device name
// is what an operator recognises on the NAS, so it leads; the short id
// keeps two machines with the same name apart.
func DeviceDirName(deviceID, deviceName string) string {
	name := SanitizeSegment(deviceName)
	short := deviceID
	if len(short) > 8 {
		short = short[:8]
	}
	if name == "_" {
		return SanitizeSegment(short)
	}
	return name + "-" + SanitizeSegment(short)
}

func (s *Store) DeviceDir(deviceID, deviceName string) string {
	return filepath.Join(s.Root, DeviceDirName(deviceID, deviceName))
}

// RelPath validates and normalises a path announced by an agent, and
// returns it as a path relative to the machine's folder.
//
// Rejects anything that would escape that folder or collide with a name
// this package reserves for its own bookkeeping (the versions directory,
// the index). A manifest arrives over the network: "Bureau/../../etc/
// passwd" must not become a write primitive, and a file called
// "_anciennes_versions" or ".backup-index.json" must not shadow it.
func RelPath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	parts := strings.Split(p, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", fmt.Errorf("chemin non autorisé: %s", p)
		}
		clean = append(clean, SanitizeSegment(part))
	}
	if len(clean) == 0 {
		return "", fmt.Errorf("chemin vide")
	}
	if isReservedName(clean[0]) {
		return "", fmt.Errorf("chemin réservé: %s", p)
	}
	return filepath.Join(clean...), nil
}

// FileInfo is one entry of what a machine currently holds.
type FileInfo struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
}

// SnapshotCurrent copies the machine's live mirror into a new dated
// version directory before that mirror is modified, so a complete previous
// state always survives the update that follows.
//
// The copy is made of hard links: it's near-instant and costs no extra
// space, however large the tree.
func (s *Store) SnapshotCurrent(deviceDir string, at time.Time) (string, error) {
	if _, err := os.Stat(deviceDir); os.IsNotExist(err) {
		return "", nil // first ever backup: nothing to preserve yet
	}
	entries, err := os.ReadDir(deviceDir)
	if err != nil {
		return "", err
	}
	hasContent := false
	for _, e := range entries {
		if !isReservedName(e.Name()) {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return "", nil
	}

	versionDir := filepath.Join(deviceDir, VersionsDirName, at.Format(versionTimeLayout))
	// A second backup within the same minute would otherwise collide with
	// the previous version directory.
	for i := 1; ; i++ {
		if _, err := os.Stat(versionDir); os.IsNotExist(err) {
			break
		}
		versionDir = filepath.Join(deviceDir, VersionsDirName, fmt.Sprintf("%s-%d", at.Format(versionTimeLayout), i))
		if i > 100 {
			return "", fmt.Errorf("impossible de nommer une nouvelle version")
		}
	}
	if err := os.MkdirAll(versionDir, 0o750); err != nil {
		return "", err
	}

	for _, e := range entries {
		if isReservedName(e.Name()) {
			continue
		}
		if err := s.linkTree(filepath.Join(deviceDir, e.Name()), filepath.Join(versionDir, e.Name())); err != nil {
			return "", err
		}
	}
	return versionDir, nil
}

// linkTree recreates src at dst, hard-linking files rather than copying
// their contents.
func (s *Store) linkTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o750); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := s.linkTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	return s.LinkOrCopy(src, dst)
}

// LinkOrCopy hard-links src to dst, falling back to a byte copy when the
// filesystem won't do it.
//
// The fallback is what keeps this working on a NAS mounted over SMB/CIFS,
// where hard links are frequently unavailable: versions then cost real
// disk space, but nothing is re-sent over the network and the layout an
// operator sees is identical either way.
func (s *Store) LinkOrCopy(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if s.hardLinksWork() {
		if err := os.Link(src, dst); err == nil {
			return nil
		} else if !os.IsExist(err) {
			s.linkWarnOnce.Do(func() {
				log.Printf("stockage: liens physiques indisponibles (%v) - les anciennes versions seront des copies complètes", err)
			})
			s.setHardLinks(false)
		} else {
			return nil
		}
	}
	return copyFile(src, dst)
}

// hardLinksWork probes the storage filesystem once.
func (s *Store) hardLinksWork() bool {
	s.linkMu.Lock()
	defer s.linkMu.Unlock()
	if s.linkChecked {
		return s.linkSupport
	}
	s.linkChecked = true
	s.linkSupport = false

	probe := filepath.Join(s.Root, ".lien-test")
	link := probe + "-2"
	_ = os.Remove(probe)
	_ = os.Remove(link)
	if err := os.WriteFile(probe, []byte("x"), 0o640); err != nil {
		return false
	}
	defer os.Remove(probe)
	if err := os.Link(probe, link); err != nil {
		log.Printf("stockage: liens physiques non supportés par %s (%v) - les anciennes versions seront des copies complètes", s.Root, err)
		return false
	}
	os.Remove(link)
	s.linkSupport = true
	return true
}

func (s *Store) setHardLinks(v bool) {
	s.linkMu.Lock()
	s.linkSupport = v
	s.linkMu.Unlock()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".partiel"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	return nil
}

// indexEntry is one file's confirmed record in IndexFileName: exactly
// what the agent announced for it - size and modification time in
// nanoseconds - at the moment it was last actually written here.
type indexEntry struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"mtime"` // nanoseconds since epoch, as announced by the agent
}

type fileIndex map[string]indexEntry // keyed by RelPath

func (s *Store) indexPath(deviceDir string) string {
	return filepath.Join(deviceDir, IndexFileName)
}

func (s *Store) loadIndex(deviceDir string) fileIndex {
	data, err := os.ReadFile(s.indexPath(deviceDir))
	if err != nil {
		return fileIndex{}
	}
	var idx fileIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return fileIndex{}
	}
	return idx
}

func (s *Store) saveIndex(deviceDir string, idx fileIndex) error {
	if err := os.MkdirAll(deviceDir, 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	tmp := s.indexPath(deviceDir) + ".partiel"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.indexPath(deviceDir)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// NeededFiles compares what the machine holds against what this store has
// confirmed it already holds, and returns the paths it doesn't have an
// identical copy of.
//
// The comparison is against IndexFileName - this store's own record of
// what it has - never against a fresh stat() of the file sitting on the
// NAS. See the package doc for why: some NAS mounts don't preserve the
// exact modification time they were told to store, and even a faithful
// one only has one-second resolution, which on its own can't tell apart
// an untouched file from one edited twice within the same second.
//
// A path with no confirmed record yet - typically right after upgrading
// from a version that didn't keep one - falls back to comparing against
// whatever is actually stored, so introducing the index doesn't force a
// one-time re-upload of an entire, already-backed-up machine. The first
// time that file is genuinely modified and re-sent, ConfirmUpdates gives
// it a real record and this fallback stops applying to it. A file that
// never changes again is never at risk either way - there's nothing for
// any comparison method to miss.
//
// On top of either comparison, a file whose modification time is at or
// after the start of the last successful backup is always re-sent
// regardless: it may have changed after being read, and no comparison
// this coarse can prove otherwise. This is what actually closes the
// same-second gap - the index's nanosecond precision makes it extremely
// unlikely on its own, but "extremely unlikely" is not the bar for a
// backup. The cost is one extra transfer for the handful of files
// touched around backup time, and it is self-limiting: on the run after
// that, lastBackupStart has moved past them and they go back to being
// skipped. A zero lastBackupStart (no successful backup yet) disables
// this rule.
func (s *Store) NeededFiles(deviceDir string, files []FileInfo, lastBackupStart time.Time) []string {
	idx := s.loadIndex(deviceDir)
	needed := make([]string, 0, len(files))
	unstableFrom := int64(0)
	if !lastBackupStart.IsZero() {
		unstableFrom = lastBackupStart.UnixNano()
	}
	for _, f := range files {
		rel, err := RelPath(f.Path)
		if err != nil {
			continue
		}

		if entry, known := idx[rel]; known {
			if entry.Size != f.Size || entry.ModTime != f.ModTime {
				needed = append(needed, f.Path)
				continue
			}
		} else {
			announcedSeconds := time.Unix(0, f.ModTime).Unix()
			info, err := os.Stat(filepath.Join(deviceDir, rel))
			if err != nil || info.Size() != f.Size || info.ModTime().Unix() != announcedSeconds {
				needed = append(needed, f.Path)
				continue
			}
		}

		if unstableFrom != 0 && f.ModTime >= unstableFrom {
			needed = append(needed, f.Path)
		}
	}
	return needed
}

func (s *Store) pendingPath(deviceDir, snapshotID string) string {
	return filepath.Join(deviceDir, pendingFilePrefix+SanitizeSegment(snapshotID)+".json")
}

// SavePendingUpdates records which files this backup intends to write and
// what the agent announced for them, so ConfirmUpdates can later update
// the index for whichever of them actually made it to disk. Called once
// per backup with just the files NeededFiles asked for - not the whole
// machine - so this costs nothing close to a full index rewrite.
//
// deviceDir may not exist yet: on a machine's very first backup, Plan
// runs (and calls this) before WriteFile has created anything.
func (s *Store) SavePendingUpdates(deviceDir, snapshotID string, files []FileInfo) error {
	if err := os.MkdirAll(deviceDir, 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(files)
	if err != nil {
		return err
	}
	return os.WriteFile(s.pendingPath(deviceDir, snapshotID), data, 0o640)
}

// ConfirmUpdates finalises the index for one backup.
//
// For every file that backup intended to write, it checks whether the
// file actually sitting on disk now matches the size that was announced -
// deliberately just the size, not the stored modification time, since
// trusting that would reintroduce the exact NAS round-trip problem this
// index exists to avoid. A size match means the write reached disk, and
// the index is given the agent's own precise modification time; a
// mismatch (the file was skipped, failed, or this backup never got to it)
// leaves whatever entry was already there, so that file is correctly
// still seen as needing to be sent on the next backup.
//
// This is where the index is actually written to - once per backup, at
// the size of what changed, never at the size of the whole machine.
//
// Safe to call with no matching pending file, and safe to call more than
// once; both are no-ops. Called regardless of whether the backup as a
// whole succeeded, failed or was cancelled: whatever files did land on
// disk before that are genuinely there and should stop being flagged as
// needed.
func (s *Store) ConfirmUpdates(deviceDir, snapshotID string) {
	path := s.pendingPath(deviceDir, snapshotID)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	defer os.Remove(path)

	var pending []FileInfo
	if err := json.Unmarshal(data, &pending); err != nil {
		log.Printf("index: fichier d'attente illisible pour %s: %v", deviceDir, err)
		return
	}

	idx := s.loadIndex(deviceDir)
	changed := false
	for _, f := range pending {
		rel, err := RelPath(f.Path)
		if err != nil {
			continue
		}
		info, err := os.Stat(filepath.Join(deviceDir, rel))
		if err != nil || info.Size() != f.Size {
			continue
		}
		idx[rel] = indexEntry{Size: f.Size, ModTime: f.ModTime}
		changed = true
	}
	if changed {
		if err := s.saveIndex(deviceDir, idx); err != nil {
			log.Printf("index: enregistrement pour %s: %v", deviceDir, err)
		}
	}
}

// PruneRemoved deletes from the live mirror anything the machine no longer
// has, so the top-level folder is a faithful picture of the PC as it is
// now. Nothing is lost by this: the version directory created just before
// still holds those files for as long as retention keeps it.
func (s *Store) PruneRemoved(deviceDir string, files []FileInfo) (removed int) {
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		if rel, err := RelPath(f.Path); err == nil {
			keep[rel] = true
		}
	}

	var walk func(dir string) bool // returns true if dir still has content
	walk = func(dir string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		any := false
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			rel, err := filepath.Rel(deviceDir, full)
			if err != nil {
				any = true
				continue
			}
			if isReservedName(e.Name()) && filepath.Dir(full) == deviceDir {
				any = true
				continue
			}
			if e.IsDir() {
				if walk(full) {
					any = true
				} else if err := os.Remove(full); err != nil {
					any = true
				}
				continue
			}
			if keep[rel] {
				any = true
				continue
			}
			if err := os.Remove(full); err == nil {
				removed++
			} else {
				any = true
			}
		}
		return any
	}
	walk(deviceDir)
	s.pruneIndex(deviceDir, keep)
	return removed
}

// pruneIndex drops index entries for files no longer on the machine, so
// the index never claims to hold something PruneRemoved just deleted.
func (s *Store) pruneIndex(deviceDir string, keep map[string]bool) {
	idx := s.loadIndex(deviceDir)
	changed := false
	for rel := range idx {
		if !keep[rel] {
			delete(idx, rel)
			changed = true
		}
	}
	if changed {
		if err := s.saveIndex(deviceDir, idx); err != nil {
			log.Printf("index: purge pour %s: %v", deviceDir, err)
		}
	}
}

// WriteFile stores one uploaded file in the live mirror, at the same
// relative location it had on the machine.
func (s *Store) WriteFile(deviceDir, relPath string, r io.Reader, modTime int64) (int64, error) {
	rel, err := RelPath(relPath)
	if err != nil {
		return 0, err
	}
	dest := filepath.Join(deviceDir, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return 0, err
	}

	// Written to a temporary name and renamed into place. That protects
	// two things at once: an interrupted transfer can never leave a
	// half-written file looking like a good backup, and - because rename
	// swaps the directory entry rather than the file's contents - the old
	// inode, which every previous version is hard-linked to, keeps its
	// data. Writing into dest directly would rewrite all of them at once,
	// leaving a history that looks intact while holding a single state.
	tmp := dest + ".partiel"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, r)
	if err != nil {
		out.Close()
		os.Remove(tmp)
		return 0, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return 0, err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	// Not needed on Linux, where rename replaces an existing entry; here
	// for the filesystems that refuse it (Windows, some SMB servers).
	_ = os.Remove(dest)
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if modTime > 0 {
		// modTime is nanoseconds since epoch, exactly as the agent's own
		// clock reported it - see IndexFileName. Applied here purely for
		// an operator browsing the NAS to see a sensible date; comparison
		// never depends on this surviving the round trip.
		t := time.Unix(0, modTime)
		_ = os.Chtimes(dest, t, t)
	}
	return n, nil
}

// ListVersions returns a machine's previous versions, newest first.
func (s *Store) ListVersions(deviceDir string) []string {
	entries, err := os.ReadDir(filepath.Join(deviceDir, VersionsDirName))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

// VersionDir resolves one previous version by name to its folder on disk.
//
// The name comes from the panel, which got it from ListVersions - but it
// still arrives as a URL path segment, so it is validated here rather than
// trusted: only a direct child of the versions directory is ever returned.
func (s *Store) VersionDir(deviceDir, name string) (string, error) {
	clean := SanitizeSegment(name)
	if clean != name || clean == "_" {
		return "", fmt.Errorf("nom de version invalide: %s", name)
	}
	dir := filepath.Join(deviceDir, VersionsDirName, clean)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("version introuvable: %s", name)
	}
	return dir, nil
}

// RemoveVersion deletes one previous version. Only the version directory
// can be removed this way - never the live mirror, which is the machine's
// current backup.
func (s *Store) RemoveVersion(deviceDir, name string) error {
	dir, err := s.VersionDir(deviceDir, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// Rotate keeps the most recent keepVersions previous versions and deletes
// the rest.
//
// keepVersions counts *previous* versions; the live mirror is always
// there in addition, so a retention of 2 means the current backup plus one
// previous - and rotation only ever runs after a new version has been
// safely created, never before, so there is no moment with nothing to
// fall back on.
func (s *Store) Rotate(deviceDir string, keepVersions int) (deleted int) {
	if keepVersions < 1 {
		keepVersions = 1
	}
	versions := s.ListVersions(deviceDir)
	if len(versions) <= keepVersions {
		return 0
	}
	for _, name := range versions[keepVersions:] {
		full := filepath.Join(deviceDir, VersionsDirName, name)
		if err := os.RemoveAll(full); err != nil {
			log.Printf("rétention: suppression de %s impossible: %v", full, err)
			continue
		}
		deleted++
	}
	return deleted
}

// UsedBytes reports the storage actually consumed, counting a file shared
// between versions by hard links only once - so the number matches what
// the NAS itself reports rather than the sum of every version's apparent
// size.
//
// A full walk of the whole storage tree - every device, every previous
// version - so this is genuinely expensive on a real network mount (a
// stat() per file, over the network). Not meant to be called on a
// request path: the scheduler calls this periodically in the background
// and records the result (see scheduler.refreshStorageUsage), and the
// dashboard just reads that.
func (s *Store) UsedBytes() (int64, error) {
	var total int64
	seen := make(map[uint64]bool)
	err := filepath.Walk(s.Root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		if ino, nlink, ok := fileIdentity(info); ok && nlink > 1 {
			if seen[ino] {
				return nil
			}
			seen[ino] = true
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// DeviceUsedBytes is UsedBytes for one machine's folder.
func (s *Store) DeviceUsedBytes(deviceDir string) int64 {
	var total int64
	seen := make(map[uint64]bool)
	_ = filepath.Walk(deviceDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		if ino, nlink, ok := fileIdentity(info); ok && nlink > 1 {
			if seen[ino] {
				return nil
			}
			seen[ino] = true
		}
		total += info.Size()
		return nil
	})
	return total
}

// RenameDevice moves a machine's folder when an operator renames it in the
// panel.
//
// The folder is named after the device, so without this a rename would
// silently strand every existing backup under the old name and start the
// machine again from an empty folder - the next run would re-upload its
// entire disk, and the operator would find two folders for one PC with no
// indication which is current.
func (s *Store) RenameDevice(oldDir, newDir string) error {
	if oldDir == newDir {
		return nil
	}
	if err := s.checkInsideRoot(oldDir); err != nil {
		return err
	}
	if err := s.checkInsideRoot(newDir); err != nil {
		return err
	}
	if _, err := os.Stat(oldDir); os.IsNotExist(err) {
		return nil // never backed up under the old name: nothing to move
	}
	if _, err := os.Stat(newDir); err == nil {
		return fmt.Errorf("le dossier %s existe déjà", filepath.Base(newDir))
	}
	return os.Rename(oldDir, newDir)
}

// checkInsideRoot refuses any path that isn't a folder under the storage
// root. Device names reach these paths from the panel, so "inside the
// root" is a boundary to enforce, not an assumption to make.
func (s *Store) checkInsideRoot(dir string) error {
	if dir == "" || filepath.Clean(dir) == filepath.Clean(s.Root) {
		return fmt.Errorf("refus d'agir sur la racine du stockage")
	}
	if !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(s.Root)+string(filepath.Separator)) {
		return fmt.Errorf("dossier hors du stockage")
	}
	return nil
}

// RemoveCurrent wipes a machine's live mirror - its up-to-date backup -
// while leaving every previous version in _anciennes_versions untouched.
// An operator reaches for this to force a completely clean re-capture
// (a live copy suspected corrupted or infected, say) without losing the
// historical states already kept.
//
// This also clears the index (see IndexFileName): with the files gone,
// any index entries that survived would make the next backup wrongly
// believe the NAS still holds them, and skip re-uploading exactly what
// was just deleted. Deleting everything except VersionsDirName already
// takes the index with it, since it lives at the same level as the real
// folders, not inside VersionsDirName.
func (s *Store) RemoveCurrent(deviceDir string) error {
	if err := s.checkInsideRoot(deviceDir); err != nil {
		return err
	}
	entries, err := os.ReadDir(deviceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // never backed up: nothing to remove
		}
		return err
	}
	for _, e := range entries {
		if e.Name() == VersionsDirName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(deviceDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// RemoveDevice deletes a machine's whole folder, versions included.
func (s *Store) RemoveDevice(deviceDir string) error {
	if err := s.checkInsideRoot(deviceDir); err != nil {
		return err
	}
	return os.RemoveAll(deviceDir)
}
