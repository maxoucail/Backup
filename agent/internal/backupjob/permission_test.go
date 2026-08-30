package backupjob

import (
	"errors"
	"io/fs"
	"sync"
	"testing"
)

// noteFileFailure must count a real OS permission refusal separately from
// any other reason a file couldn't be sent (locked, edited mid-run,
// vanished) - that count is what isDiskAccessDenied below acts on to tell
// a revoked macOS Full Disk Access grant apart from routine noise.
func TestNoteFileFailureCountsOnlyRealPermissionErrors(t *testing.T) {
	var mu sync.Mutex
	var skipped []string
	var permissionDenied int
	var fatal error

	permErr := &fs.PathError{Op: "open", Path: "x", Err: fs.ErrPermission}
	noteFileFailure(&mu, &skipped, &permissionDenied, &fatal, "a.txt", permErr)
	if permissionDenied != 1 {
		t.Fatalf("permissionDenied = %d après une vraie erreur de permission, attendu 1", permissionDenied)
	}

	otherErr := errors.New("le fichier a été modifié pendant la sauvegarde")
	noteFileFailure(&mu, &skipped, &permissionDenied, &fatal, "b.txt", otherErr)
	if permissionDenied != 1 {
		t.Fatalf("permissionDenied = %d après une erreur sans rapport avec les permissions, attendu qu'il reste à 1", permissionDenied)
	}

	if len(skipped) != 2 {
		t.Fatalf("fichiers ignorés = %v, attendu les deux, quelle que soit la cause", skipped)
	}
}

// This is the exact shape of the field failure: 70 GB correctly detected
// (directory listing works without Full Disk Access) but almost nothing
// actually uploaded (opening file contents inside a TCC-protected folder
// doesn't) - a majority of permission refusals must be recognized as a
// systemic access problem, not dismissed as "a couple of locked files".
func TestIsDiskAccessDeniedOnMajorityPermissionFailures(t *testing.T) {
	cases := []struct {
		name             string
		permissionDenied int
		needed           int
		want             bool
	}{
		{"aucun refus", 0, 10, false},
		{"un seul refus sur beaucoup", 1, 10, false},
		{"tout juste la majorité", 5, 10, true},
		{"quasi tout refusé", 9, 10, true},
		{"le seul fichier nécessaire est refusé", 1, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDiskAccessDenied(tc.permissionDenied, tc.needed); got != tc.want {
				t.Fatalf("isDiskAccessDenied(%d, %d) = %v, attendu %v", tc.permissionDenied, tc.needed, got, tc.want)
			}
		})
	}
}
