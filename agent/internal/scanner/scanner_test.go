package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A colon only ever shows up here as a Windows drive-letter separator (the
// case filepath.Rel can't relate across volumes), so relPath must strip it
// - restoring a path with a bare "E:" component fails outright on Windows
// ("La syntaxe du nom de fichier... est incorrecte").
func TestRelPathStripsColonForPathOutsideHome(t *testing.T) {
	home := `C:\Users\gdallaverde`
	abs := `E:\$RECYCLE.BIN\S-1-5-21-0\desktop.ini`

	got := relPath(home, abs)

	if strings.Contains(got, ":") {
		t.Fatalf("relPath(%q, %q) = %q, contient encore un caractère illégal sur Windows", home, abs, got)
	}
	if !strings.HasPrefix(got, "_outside/") {
		t.Fatalf("relPath(%q, %q) = %q, attendu un préfixe _outside/", home, abs, got)
	}
}

// A file genuinely under home must keep its ordinary relative path -
// namespacing everything under _outside would be pointless churn for the
// overwhelming common case.
func TestRelPathKeepsOrdinaryRelativePathUnderHome(t *testing.T) {
	home := "/home/gdallaverde"
	abs := "/home/gdallaverde/Documents/rapport.txt"

	got := relPath(home, abs)

	if got != "Documents/rapport.txt" {
		t.Fatalf("relPath = %q, attendu %q", got, "Documents/rapport.txt")
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

	files := Walk([]string{root})

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
