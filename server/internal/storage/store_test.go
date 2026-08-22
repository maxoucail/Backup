package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"
)

func writeChunk(t *testing.T, s *Store, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	if _, err := s.WriteChunk(hash, strings.NewReader(content)); err != nil {
		t.Fatalf("WriteChunk(%s): %v", content, err)
	}
	return hash
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// A backup that has uploaded some chunks but hasn't submitted its manifest
// yet owns data that no manifest references. Garbage collection must not
// treat those chunks as garbage - deleting them makes the in-flight backup
// fail at manifest submission, and (if it raced the manifest write) could
// leave a snapshot recorded as successful but missing its data.
func TestGarbageCollectKeepsChunksOfInFlightBackup(t *testing.T) {
	s := newStore(t)
	inFlight := writeChunk(t, s, "chunk being uploaded right now")

	// No manifests at all: as far as the DB is concerned nothing references
	// this chunk yet, exactly the state during a long first backup.
	if _, _, err := s.GarbageCollect(nil, time.Now().Add(-chunkGracePeriod)); err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}

	if !s.HasChunk(inFlight) {
		t.Fatal("le chunk d'une sauvegarde en cours a été supprimé par le garbage collector")
	}
}

// Chunks that are genuinely unreferenced and old enough to be past the
// grace period are what GC exists to reclaim.
func TestGarbageCollectRemovesOldUnreferencedChunks(t *testing.T) {
	s := newStore(t)
	orphan := writeChunk(t, s, "orphaned chunk from a rotated-away snapshot")

	// Simulate a chunk written well before the grace window.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(s.ChunkPath(orphan), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	_, removed, err := s.GarbageCollect(nil, time.Now().Add(-chunkGracePeriod))
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if removed != 1 {
		t.Fatalf("chunks supprimés = %d, attendu 1", removed)
	}
	if s.HasChunk(orphan) {
		t.Fatal("le chunk orphelin aurait dû être supprimé")
	}
}

// Deduplication means an in-flight backup can reference a chunk it did not
// upload (another device already had it). If the snapshot that originally
// owned that chunk gets rotated away mid-backup, nothing in the DB
// references the chunk anymore - Touch is what keeps it alive until the
// borrowing backup submits its manifest.
func TestTouchProtectsDeduplicatedChunkFromGC(t *testing.T) {
	s := newStore(t)
	shared := writeChunk(t, s, "content both devices happen to have")

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(s.ChunkPath(shared), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// The second device's backup discovers the server already has this
	// chunk and so never re-uploads it; Touch records that it's in use.
	s.Touch(shared)

	if _, _, err := s.GarbageCollect(nil, time.Now().Add(-chunkGracePeriod)); err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if !s.HasChunk(shared) {
		t.Fatal("un chunk dédupliqué en cours d'utilisation a été supprimé")
	}
}

func TestWriteChunkRejectsMismatchedHash(t *testing.T) {
	s := newStore(t)
	wrong := hex.EncodeToString(sha256.New().Sum(nil))
	if _, err := s.WriteChunk(wrong, strings.NewReader("des octets qui ne correspondent pas")); err == nil {
		t.Fatal("WriteChunk aurait dû rejeter un contenu ne correspondant pas au hash annoncé")
	}
	if s.HasChunk(wrong) {
		t.Fatal("un chunk au contenu invalide a été conservé")
	}
}
