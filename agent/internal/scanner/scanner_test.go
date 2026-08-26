package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The whole point of the logical-path scheme: a redirected folder's
// physical location (here, Downloads moved to E:) must not leak into the
// manifest. Recording "Downloads/..." is what lets a restore re-resolve
// the folder on whatever machine it runs on, rather than chasing a drive
// letter that may not exist there - or, before this, dumping the file into
// a _outside folder nobody expects.
func TestRelPathUsesLogicalNameForRedirectedKnownFolder(t *testing.T) {
	// Stands in for Downloads redirected to another drive: a real
	// directory that is genuinely not under home, expressed with this
	// platform's separators so filepath behaves as it would on Windows
	// with a drive letter.
	home := t.TempDir()
	redirected := filepath.Join(t.TempDir(), "Downloads")
	root := Root{Path: redirected, Name: "Downloads"}
	abs := filepath.Join(redirected, "facture.pdf")

	got := relPath(root, home, abs)

	if got != "Downloads/facture.pdf" {
		t.Fatalf("relPath = %q, attendu %q", got, "Downloads/facture.pdf")
	}
}

// A known folder that hasn't been moved must produce the exact same
// logical path as a redirected one - that equivalence is what makes old
// manifests (written as plain home-relative paths) keep restoring
// correctly through the new resolution.
func TestRelPathUsesLogicalNameForNonRedirectedKnownFolder(t *testing.T) {
	root := Root{Path: "/home/gdallaverde/Documents", Name: "Documents"}
	home := "/home/gdallaverde"
	abs := "/home/gdallaverde/Documents/rapport.txt"

	got := relPath(root, home, abs)

	if got != "Documents/rapport.txt" {
		t.Fatalf("relPath = %q, attendu %q", got, "Documents/rapport.txt")
	}
}

// A colon only ever shows up here as a Windows drive-letter separator (the
// case filepath.Rel can't relate across volumes), so relPath must strip it
// - restoring a path with a bare "E:" component fails outright on Windows
// ("La syntaxe du nom de fichier... est incorrecte"). This is the residual
// case: a custom configured root, with no logical folder identity.
func TestRelPathStripsColonForCustomPathOutsideHome(t *testing.T) {
	root := Root{Path: `E:\`}
	home := `C:\Users\gdallaverde`
	abs := `E:\$RECYCLE.BIN\S-1-5-21-0\desktop.ini`

	got := relPath(root, home, abs)

	if strings.Contains(got, ":") {
		t.Fatalf("relPath = %q, contient encore un caractère illégal sur Windows", got)
	}
	if !strings.HasPrefix(got, "_outside/") {
		t.Fatalf("relPath = %q, attendu un préfixe _outside/", got)
	}
}

// The restore side of the same contract: a logical path must resolve to
// wherever that folder currently lives for this user. Resolve() reads the
// registry on Windows; on this test platform it's home-relative, which is
// enough to prove the mapping is applied rather than the raw path used.
func TestKnownFolderDestResolvesAgainstCurrentLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got, ok := KnownFolderDest("Downloads/facture.pdf")
	if !ok {
		t.Fatal("KnownFolderDest doit reconnaître un dossier connu")
	}
	want := filepath.Join(home, "Downloads", "facture.pdf")
	if got != want {
		t.Fatalf("KnownFolderDest = %q, attendu %q", got, want)
	}
}

// A path that names no known folder must be left to the caller, not
// force-fitted into one.
func TestKnownFolderDestIgnoresOtherPaths(t *testing.T) {
	for _, p := range []string{"_outside/E/Projets/x.txt", "Projets/x.txt", ""} {
		if _, ok := KnownFolderDest(p); ok {
			t.Fatalf("KnownFolderDest(%q) ne doit pas revendiquer ce chemin", p)
		}
	}
}

// A manifest is data from the network: a "../" smuggled into a path must
// not let a restore write outside the folder it claims to target.
func TestKnownFolderDestRefusesPathEscape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if dest, ok := KnownFolderDest("Downloads/../../../etc/passwd"); ok {
		t.Fatalf("KnownFolderDest a accepté une évasion de chemin: %q", dest)
	}
}

// $RECYCLE.BIN only ever shows up when an operator points a whole drive at
// the backup; it's OS-managed, frequently permission-locked, and useless
// once restored. It must never reach the manifest.
func TestWalkExcludesRecycleBin(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "rapport.txt"), "contenu")

	bin := filepath.Join(root, "$RECYCLE.BIN", "S-1-5-21-0")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(bin, "desktop.ini"), "junk")

	files := Walk([]Root{{Path: root}})

	for _, f := range files {
		if strings.Contains(strings.ToLower(f.AbsPath), "$recycle.bin") {
			t.Fatalf("Walk a inclus un fichier de $RECYCLE.BIN: %s", f.AbsPath)
		}
	}
	if len(files) != 1 || !strings.HasSuffix(files[0].AbsPath, "rapport.txt") {
		t.Fatalf("fichiers trouvés = %v, attendu uniquement rapport.txt", files)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
