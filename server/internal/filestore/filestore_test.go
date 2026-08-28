package filestore

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func put(t *testing.T, s *Store, deviceDir, rel, content string, mtime int64) {
	t.Helper()
	if _, err := s.WriteFile(deviceDir, rel, strings.NewReader(content), mtime); err != nil {
		t.Fatalf("WriteFile %s: %v", rel, err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// The entire point of this design: the operator opens the NAS, finds a
// folder named after the PC, and inside it the machine's own folders,
// with the real files in them. Anything else and there's no restoring by
// hand.
func TestFilesAreStoredInClearAtTheirRealPath(t *testing.T) {
	s := newStore(t)
	dir := s.DeviceDir("bdecd372ab", "PC-Max")

	put(t, s, dir, "Bureau/rapport.docx", "contenu du rapport", 1700000000)
	put(t, s, dir, "Documents/factures/2026.pdf", "une facture", 1700000000)

	if got := filepath.Base(dir); got != "PC-Max-bdecd372" {
		t.Fatalf("dossier machine = %q, attendu PC-Max-bdecd372", got)
	}
	if got := read(t, filepath.Join(dir, "Bureau", "rapport.docx")); got != "contenu du rapport" {
		t.Fatalf("contenu = %q", got)
	}
	if got := read(t, filepath.Join(dir, "Documents", "factures", "2026.pdf")); got != "une facture" {
		t.Fatalf("contenu = %q", got)
	}

	// The stored mtime is what the next run compares against; if it isn't
	// preserved, every file is re-sent forever.
	info, err := os.Stat(filepath.Join(dir, "Bureau", "rapport.docx"))
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Unix() != 1700000000 {
		t.Fatalf("mtime = %d, attendu 1700000000", info.ModTime().Unix())
	}
}

// lastBackup stands in for "a backup has since run and settled": it is
// later than every file time used below, so the recently-modified safety
// rule doesn't fire and these tests exercise the size/time comparison on
// its own.
var lastBackup = time.Unix(1700001000, 0)

// Incremental transfer: a file the server already holds, unchanged, must
// not be asked for again. Without this a machine re-sends its whole disk
// on every run.
func TestNeededFilesAsksOnlyForWhatChanged(t *testing.T) {
	s := newStore(t)
	dir := s.DeviceDir("dev1", "PC")

	put(t, s, dir, "Bureau/stable.txt", "inchangé", 1700000000)
	put(t, s, dir, "Bureau/modifie.txt", "ancienne version", 1700000000)

	needed := s.NeededFiles(dir, []FileInfo{
		{Path: "Bureau/stable.txt", Size: int64(len("inchangé")), ModTime: 1700000000},
		{Path: "Bureau/modifie.txt", Size: 42, ModTime: 1700000500}, // touched since
		{Path: "Bureau/nouveau.txt", Size: 10, ModTime: 1700000500}, // never seen
	}, lastBackup)
	sort.Strings(needed)

	want := []string{"Bureau/modifie.txt", "Bureau/nouveau.txt"}
	if len(needed) != len(want) || needed[0] != want[0] || needed[1] != want[1] {
		t.Fatalf("fichiers demandés = %v, attendu %v", needed, want)
	}
}

// A file already on the NAS that gets edited must come back on the next
// backup - whatever form the edit took. Every one of these is a real way a
// file changes on a live machine, and missing any of them means the copy
// on the NAS silently stops matching the PC.
func TestEveryKindOfModificationIsSentAgain(t *testing.T) {
	cases := []struct {
		name        string
		before      string
		after       string
		modTimeThen int64
		modTimeNow  int64
	}{
		{"contenu plus long", "v1", "version 2, plus longue", 1700000000, 1700000900},
		{"contenu plus court", "version 1, longue", "v2", 1700000000, 1700000900},
		{"même taille, contenu différent", "aaaa", "bbbb", 1700000000, 1700000900},
		{"même taille, date inchangée mais taille... non: taille différente, date inchangée",
			"v1", "version 2, plus longue", 1700000000, 1700000000},
		{"date antérieure (fichier remis depuis une vieille copie)", "aaaa", "bbbb", 1700000900, 1700000000},
		{"fichier vidé", "du contenu", "", 1700000000, 1700000900},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			dir := s.DeviceDir("dev1", "PC")
			put(t, s, dir, "Bureau/f.txt", tc.before, tc.modTimeThen)

			needed := s.NeededFiles(dir, []FileInfo{
				{Path: "Bureau/f.txt", Size: int64(len(tc.after)), ModTime: tc.modTimeNow},
			}, lastBackup)

			if len(needed) != 1 {
				t.Fatalf("le fichier modifié n'est pas redemandé (needed=%v) : la copie sur le NAS resterait périmée", needed)
			}

			// ...and once re-sent, the NAS really holds the new content.
			put(t, s, dir, "Bureau/f.txt", tc.after, tc.modTimeNow)
			if got := read(t, filepath.Join(dir, "Bureau", "f.txt")); got != tc.after {
				t.Fatalf("contenu sur le NAS = %q, attendu %q", got, tc.after)
			}
			if again := s.NeededFiles(dir, []FileInfo{
				{Path: "Bureau/f.txt", Size: int64(len(tc.after)), ModTime: tc.modTimeNow},
			}, lastBackup); len(again) != 0 {
				t.Fatalf("le fichier est redemandé alors qu'il vient d'être envoyé: %v", again)
			}
		})
	}
}

// The blind spot of any size+date comparison: a file modified again during
// the very second the agent read it keeps the same size and the same
// second, so nothing ever looks different and the NAS copy stays stale
// forever. Silent data loss - the one failure a backup must not have.
//
// The guard is that anything modified at or after the last successful
// backup started is re-sent unconditionally.
func TestFileModifiedWithinTheSameSecondIsStillSentAgain(t *testing.T) {
	s := newStore(t)
	dir := s.DeviceDir("dev1", "PC")

	backupStart := time.Unix(1700000000, 0)
	// Read and stored during that backup...
	readAt := int64(1700000005)
	put(t, s, dir, "Bureau/notes.txt", "texte original", readAt)

	// ...and saved again in the same second, same length. Identical size,
	// identical second: invisible to the comparison on its own.
	announced := []FileInfo{{Path: "Bureau/notes.txt", Size: int64(len("texte MODIFIE!")), ModTime: readAt}}

	if needed := s.NeededFiles(dir, announced, backupStart); len(needed) != 1 {
		t.Fatal("un fichier modifié dans la même seconde que sa lecture n'est jamais renvoyé : il resterait périmé indéfiniment sur le NAS")
	}

	// The rule must not turn into "re-send everything forever": once a
	// later backup has been through, the file goes back to being skipped.
	put(t, s, dir, "Bureau/notes.txt", "texte MODIFIE!", readAt)
	laterBackup := time.Unix(1700003600, 0)
	if needed := s.NeededFiles(dir, announced, laterBackup); len(needed) != 0 {
		t.Fatalf("fichier renvoyé indéfiniment (%v) : chaque sauvegarde retransférerait tout", needed)
	}
}

// A machine that has never had a successful backup has no cutoff to
// compare against; the size/date comparison must still work rather than
// treating everything as suspect or as fine.
func TestNoPreviousBackupStillComparesNormally(t *testing.T) {
	s := newStore(t)
	dir := s.DeviceDir("dev1", "PC")
	put(t, s, dir, "Bureau/f.txt", "contenu", 1700000000)

	needed := s.NeededFiles(dir, []FileInfo{
		{Path: "Bureau/f.txt", Size: 7, ModTime: 1700000000},
		{Path: "Bureau/autre.txt", Size: 3, ModTime: 1700000000},
	}, time.Time{})
	if len(needed) != 1 || needed[0] != "Bureau/autre.txt" {
		t.Fatalf("fichiers demandés = %v, attendu le seul fichier absent", needed)
	}
}

// The property that makes previous versions worth keeping at all: writing
// the new content must not rewrite the bytes the old version points at.
// Hard links share data, so a naive in-place write silently corrupts every
// version of that file at once - the backup would look intact while
// holding one single state.
func TestPreviousVersionKeepsItsOwnContentAfterAnUpdate(t *testing.T) {
	s := newStore(t)
	dir := s.DeviceDir("dev1", "PC")

	put(t, s, dir, "Bureau/rapport.docx", "VERSION 1", 1700000000)

	versionDir, err := s.SnapshotCurrent(dir, time.Unix(1700003600, 0))
	if err != nil {
		t.Fatalf("SnapshotCurrent: %v", err)
	}
	if versionDir == "" {
		t.Fatal("aucune version créée alors que la machine avait déjà des fichiers")
	}

	put(t, s, dir, "Bureau/rapport.docx", "VERSION 2 (le fichier a été modifié)", 1700003600)

	if got := read(t, filepath.Join(dir, "Bureau", "rapport.docx")); got != "VERSION 2 (le fichier a été modifié)" {
		t.Fatalf("sauvegarde à jour = %q", got)
	}
	if got := read(t, filepath.Join(versionDir, "Bureau", "rapport.docx")); got != "VERSION 1" {
		t.Fatalf("ancienne version = %q, attendu \"VERSION 1\" - l'écriture a écrasé les données partagées", got)
	}
}

// A previous version is a full, browsable tree, but an unchanged file in
// it must not cost a second copy on disk. Otherwise keeping 5 versions of
// a 200 GB machine needs a terabyte.
func TestOldVersionsShareDiskSpaceWithTheCurrentBackup(t *testing.T) {
	s := newStore(t)
	if !s.hardLinksWork() {
		t.Skip("liens physiques non supportés par le système de fichiers de test")
	}
	dir := s.DeviceDir("dev1", "PC")

	content := strings.Repeat("a", 4096)
	put(t, s, dir, "Documents/gros.bin", content, 1700000000)

	before, err := s.UsedBytes()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.SnapshotCurrent(dir, time.Unix(int64(1700003600+i*3600), 0)); err != nil {
			t.Fatalf("SnapshotCurrent: %v", err)
		}
	}
	after, err := s.UsedBytes()
	if err != nil {
		t.Fatal(err)
	}

	if len(s.ListVersions(dir)) != 3 {
		t.Fatalf("versions = %v, attendu 3", s.ListVersions(dir))
	}
	if after != before {
		t.Fatalf("espace utilisé %d -> %d: les anciennes versions ne partagent pas les données", before, after)
	}
	// ...and they're still real, readable files, not dangling references.
	for _, name := range s.ListVersions(dir) {
		if got := read(t, filepath.Join(dir, VersionsDirName, name, "Documents", "gros.bin")); got != content {
			t.Fatalf("version %s illisible ou tronquée", name)
		}
	}
}

// Retention must never leave a machine with nothing. Rotation runs only
// after a new state exists, and keeps at least one previous version on top
// of the live mirror.
func TestRotateKeepsTheRequestedNumberAndNeverEmptiesTheFolder(t *testing.T) {
	s := newStore(t)
	dir := s.DeviceDir("dev1", "PC")
	put(t, s, dir, "Bureau/a.txt", "contenu", 1700000000)

	for i := 0; i < 5; i++ {
		if _, err := s.SnapshotCurrent(dir, time.Unix(int64(1700003600+i*3600), 0)); err != nil {
			t.Fatalf("SnapshotCurrent: %v", err)
		}
	}
	if got := len(s.ListVersions(dir)); got != 5 {
		t.Fatalf("versions avant rotation = %d, attendu 5", got)
	}

	if deleted := s.Rotate(dir, 2); deleted != 3 {
		t.Fatalf("versions supprimées = %d, attendu 3", deleted)
	}
	versions := s.ListVersions(dir)
	if len(versions) != 2 {
		t.Fatalf("versions après rotation = %v, attendu 2", versions)
	}

	// Asking for zero must not be honoured: a live mirror with no previous
	// version at all is exactly the situation retention exists to prevent.
	s.Rotate(dir, 0)
	if got := len(s.ListVersions(dir)); got != 1 {
		t.Fatalf("versions après Rotate(0) = %d, attendu 1 conservée de force", got)
	}
	if got := read(t, filepath.Join(dir, "Bureau", "a.txt")); got != "contenu" {
		t.Fatal("la sauvegarde à jour a été touchée par la rotation")
	}
}

// A file deleted on the PC must disappear from the live mirror - it's
// meant to be a faithful picture of the machine - but must survive in the
// version taken just before, which is what makes an accidental deletion
// recoverable.
func TestDeletedFileLeavesTheMirrorButStaysInThePreviousVersion(t *testing.T) {
	s := newStore(t)
	dir := s.DeviceDir("dev1", "PC")
	put(t, s, dir, "Bureau/garde.txt", "toujours là", 1700000000)
	put(t, s, dir, "Bureau/supprime.txt", "effacé par l'utilisateur", 1700000000)

	versionDir, err := s.SnapshotCurrent(dir, time.Unix(1700003600, 0))
	if err != nil {
		t.Fatal(err)
	}

	removed := s.PruneRemoved(dir, []FileInfo{
		{Path: "Bureau/garde.txt", Size: int64(len("toujours là")), ModTime: 1700000000},
	})
	if removed != 1 {
		t.Fatalf("fichiers retirés du miroir = %d, attendu 1", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "Bureau", "supprime.txt")); !os.IsNotExist(err) {
		t.Fatal("le fichier supprimé sur le PC est encore dans la sauvegarde à jour")
	}
	if got := read(t, filepath.Join(versionDir, "Bureau", "supprime.txt")); got != "effacé par l'utilisateur" {
		t.Fatalf("ancienne version = %q, le fichier supprimé doit y rester récupérable", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "Bureau", "garde.txt")); err != nil {
		t.Fatalf("un fichier toujours présent sur le PC a été retiré: %v", err)
	}
}

// PruneRemoved walks the machine's folder; it must step over the versions
// directory rather than treating its contents as files the PC no longer
// has and wiping the entire history.
func TestPruneRemovedNeverTouchesTheVersionsDirectory(t *testing.T) {
	s := newStore(t)
	dir := s.DeviceDir("dev1", "PC")
	put(t, s, dir, "Bureau/a.txt", "contenu", 1700000000)
	if _, err := s.SnapshotCurrent(dir, time.Unix(1700003600, 0)); err != nil {
		t.Fatal(err)
	}

	s.PruneRemoved(dir, []FileInfo{{Path: "Bureau/a.txt", Size: 7, ModTime: 1700000000}})

	if got := len(s.ListVersions(dir)); got != 1 {
		t.Fatalf("versions restantes = %d, attendu 1 - la purge a mangé l'historique", got)
	}
}

// Paths arrive over the network from an agent. They are the direct input
// to a filesystem write, so anything that could escape the machine's
// folder - or shadow the version history - has to be refused outright.
func TestRelPathRefusesEscapesAndReservedNames(t *testing.T) {
	for _, p := range []string{
		"../../../etc/passwd",
		"Bureau/../../../etc/passwd",
		`..\..\Windows\System32\config\SAM`,
		VersionsDirName + "/2026-01-01_00-00/x.txt",
		strings.ToUpper(VersionsDirName) + "/x.txt",
		"",
		"/",
	} {
		if got, err := RelPath(p); err == nil {
			t.Fatalf("RelPath(%q) accepté et transformé en %q, doit être refusé", p, got)
		}
	}

	// ...while ordinary paths, including Windows separators, still work.
	for in, want := range map[string]string{
		"Bureau/rapport.docx":  filepath.Join("Bureau", "rapport.docx"),
		`Documents\sous\a.txt`: filepath.Join("Documents", "sous", "a.txt"),
		"_outside/E/Projets/x": filepath.Join("_outside", "E", "Projets", "x"),
	} {
		got, err := RelPath(in)
		if err != nil {
			t.Fatalf("RelPath(%q) refusé: %v", in, err)
		}
		if got != want {
			t.Fatalf("RelPath(%q) = %q, attendu %q", in, got, want)
		}
	}
}

// WriteFile is the other half of the same boundary: a refused path must
// not write anything anywhere.
func TestWriteFileRefusesToEscapeTheMachineFolder(t *testing.T) {
	s := newStore(t)
	dir := s.DeviceDir("dev1", "PC")

	if _, err := s.WriteFile(dir, "../../evade.txt", strings.NewReader("x"), 0); err == nil {
		t.Fatal("WriteFile a accepté un chemin qui sort du dossier de la machine")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dir)), "evade.txt")); err == nil {
		t.Fatal("un fichier a été écrit hors du stockage")
	}
}

// RemoveVersion is reachable from the panel with a name from a URL; it
// must only ever delete inside the versions directory.
func TestRemoveVersionOnlyDeletesVersions(t *testing.T) {
	s := newStore(t)
	dir := s.DeviceDir("dev1", "PC")
	put(t, s, dir, "Bureau/a.txt", "contenu", 1700000000)
	if _, err := s.SnapshotCurrent(dir, time.Unix(1700003600, 0)); err != nil {
		t.Fatal(err)
	}
	name := s.ListVersions(dir)[0]

	for _, bad := range []string{"..", "../..", "../Bureau", "Bureau", "inexistante"} {
		if err := s.RemoveVersion(dir, bad); err == nil {
			t.Fatalf("RemoveVersion(%q) a réussi, doit être refusé", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Bureau", "a.txt")); err != nil {
		t.Fatalf("la sauvegarde à jour a été touchée: %v", err)
	}

	if err := s.RemoveVersion(dir, name); err != nil {
		t.Fatalf("RemoveVersion(%q): %v", name, err)
	}
	if got := len(s.ListVersions(dir)); got != 0 {
		t.Fatalf("versions restantes = %d, attendu 0", got)
	}
}

// The storage root must survive any device-level delete: a bug here wipes
// every machine's backups at once.
func TestRemoveDeviceRefusesTheStorageRoot(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{"", s.Root, filepath.Join(s.Root, ".."), "/"} {
		if err := s.RemoveDevice(bad); err == nil {
			t.Fatalf("RemoveDevice(%q) a réussi, doit être refusé", bad)
		}
	}
	if _, err := os.Stat(s.Root); err != nil {
		t.Fatalf("la racine du stockage a disparu: %v", err)
	}

	dir := s.DeviceDir("dev1", "PC")
	put(t, s, dir, "Bureau/a.txt", "contenu", 1700000000)
	if err := s.RemoveDevice(dir); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("le dossier de la machine existe encore")
	}
}

// Two machines an operator happened to name the same must not write into
// each other's folder.
func TestDeviceDirNameKeepsSameNamedMachinesApart(t *testing.T) {
	a := DeviceDirName("aaaaaaaa1111", "Poste Compta")
	b := DeviceDirName("bbbbbbbb2222", "Poste Compta")
	if a == b {
		t.Fatalf("deux machines homonymes partagent le dossier %q", a)
	}
	if !strings.HasPrefix(a, "Poste Compta-") {
		t.Fatalf("dossier = %q, le nom lisible doit rester en tête", a)
	}
	// A device name is operator input and lands in a path.
	if got := DeviceDirName("id", "../../etc"); strings.Contains(got, "/") || strings.Contains(got, `\`) {
		t.Fatalf("nom de dossier = %q, contient un séparateur", got)
	}
}

// The machine's folder is named after the device, so renaming it in the
// panel has to move the folder. Without this the existing backup is
// stranded under the old name, the machine starts again from an empty
// folder - re-uploading its entire disk - and the operator finds two
// folders for one PC with no way to tell which is current.
func TestRenameDeviceMovesTheExistingBackup(t *testing.T) {
	s := newStore(t)
	oldDir := s.DeviceDir("dev1", "PC-Ancien")
	put(t, s, oldDir, "Bureau/rapport.docx", "contenu du rapport", 1700000000)
	if _, err := s.SnapshotCurrent(oldDir, time.Unix(1700003600, 0)); err != nil {
		t.Fatal(err)
	}

	newDir := s.DeviceDir("dev1", "PC-Compta")
	if err := s.RenameDevice(oldDir, newDir); err != nil {
		t.Fatalf("RenameDevice: %v", err)
	}

	if got := read(t, filepath.Join(newDir, "Bureau", "rapport.docx")); got != "contenu du rapport" {
		t.Fatalf("contenu après renommage = %q", got)
	}
	if len(s.ListVersions(newDir)) != 1 {
		t.Fatal("les versions précédentes n'ont pas suivi le renommage")
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("l'ancien dossier existe encore : deux dossiers pour une seule machine")
	}

	// A rename onto a folder that already holds another machine's backup
	// must be refused rather than merge the two.
	other := s.DeviceDir("dev2", "PC-Autre")
	put(t, s, other, "Bureau/b.txt", "autre machine", 1700000000)
	if err := s.RenameDevice(newDir, other); err == nil {
		t.Fatal("RenameDevice a accepté d'écraser le dossier d'une autre machine")
	}
	if got := read(t, filepath.Join(other, "Bureau", "b.txt")); got != "autre machine" {
		t.Fatal("le dossier de l'autre machine a été modifié")
	}
}
