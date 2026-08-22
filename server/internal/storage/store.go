// Package storage implements the content-addressed chunk store: every
// unique piece of file content is written to disk exactly once, named by
// its SHA-256 hash. Backups become incremental for free - a snapshot's
// manifest just lists which existing chunks a file is made of, and only
// chunks the store has never seen before are actually written. Retention
// rotation deletes old snapshot manifests and garbage-collects any chunk no
// manifest references anymore.
package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

var ErrHashMismatch = errors.New("uploaded content does not match the announced hash")

type Store struct {
	Root string
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "chunks"), 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "manifests"), 0o750); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

func (s *Store) ChunkPath(hash string) string {
	if len(hash) < 4 {
		hash = hash + "0000"
	}
	return filepath.Join(s.Root, "chunks", hash[0:2], hash[2:4], hash)
}

func (s *Store) HasChunk(hash string) bool {
	_, err := os.Stat(s.ChunkPath(hash))
	return err == nil
}

// WriteChunk streams r to the chunk store under the given hash, verifying
// the content actually hashes to what the caller claims (never trust a
// client-declared hash for content that will be deduplicated across every
// other device). Returns bytesWritten=0 if the chunk already existed.
func (s *Store) WriteChunk(hash string, r io.Reader) (bytesWritten int64, err error) {
	if s.HasChunk(hash) {
		_, _ = io.Copy(io.Discard, r)
		s.Touch(hash) // in use by this backup, even though we kept the existing copy
		return 0, nil
	}
	path := s.ChunkPath(hash)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, "upload-*.tmp")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed away

	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	closeErr := tmp.Close()
	if err != nil {
		return 0, err
	}
	if closeErr != nil {
		return 0, closeErr
	}

	actual := hex.EncodeToString(hasher.Sum(nil))
	if actual != hash {
		return 0, fmt.Errorf("%w: expected %s got %s", ErrHashMismatch, hash, actual)
	}

	if err := os.Rename(tmpName, path); err != nil {
		// Another concurrent upload of the same chunk may have won the
		// race; that's fine, the content is identical by hash.
		if s.HasChunk(hash) {
			return n, nil
		}
		return 0, err
	}
	return n, nil
}

func (s *Store) ChunkReader(hash string) (io.ReadCloser, error) {
	return os.Open(s.ChunkPath(hash))
}

// chunkGracePeriod is how long a chunk is protected from garbage
// collection after it was last written or touched, regardless of whether
// any manifest references it yet.
//
// This exists because "referenced by a manifest" only becomes true at the
// *end* of a backup: an agent uploads chunks for minutes or hours, then
// submits the manifest listing them. Without a grace period, any GC
// triggered by another device finishing its own backup in that window
// would delete the in-flight chunks - failing that backup, or worse,
// racing the manifest write and leaving a snapshot marked successful with
// its data already gone. The window is deliberately generous: the only
// cost of a too-long grace period is delayed disk reclamation, while a
// too-short one costs data.
const chunkGracePeriod = 24 * time.Hour

// Touch marks a chunk as in use right now, protecting it for another
// grace period. Needed for deduplicated chunks: when a backup finds the
// server already has a chunk it never re-uploads it, so its mtime would
// otherwise still reflect whichever old snapshot first stored it - and
// that snapshot may be rotated away mid-backup.
func (s *Store) Touch(hash string) {
	now := time.Now()
	_ = os.Chtimes(s.ChunkPath(hash), now, now)
}

// GraceCutoff returns the timestamp to pass to GarbageCollect: chunks
// written or touched more recently than this are considered in use by an
// in-flight backup and left alone.
func GraceCutoff() time.Time {
	return time.Now().Add(-chunkGracePeriod)
}

// GarbageCollect deletes every chunk that is neither referenced by one of
// the given manifest files nor newer than protectNewerThan (see
// chunkGracePeriod), and returns bytes freed / chunks removed.
func (s *Store) GarbageCollect(manifestPaths []string, protectNewerThan time.Time) (freedBytes int64, removedChunks int, err error) {
	referenced := make(map[string]struct{}, 1<<16)
	for _, mp := range manifestPaths {
		m, err := ReadManifest(mp)
		if err != nil {
			continue // manifest may have raced with a concurrent delete; skip
		}
		for _, f := range m.Files {
			for _, c := range f.Chunks {
				referenced[c] = struct{}{}
			}
		}
	}

	chunksRoot := filepath.Join(s.Root, "chunks")
	walkErr := filepath.WalkDir(chunksRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		hash := d.Name()
		if _, ok := referenced[hash]; ok {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil // vanished under us; nothing to reclaim
		}
		if info.ModTime().After(protectNewerThan) {
			return nil // recently written or touched: an in-flight backup owns it
		}
		if rmErr := os.Remove(path); rmErr == nil {
			freedBytes += info.Size()
			removedChunks++
		}
		return nil
	})
	return freedBytes, removedChunks, walkErr
}

// UsedBytes returns the total size on disk of the chunk store (the real
// space consumed, after dedup - what the panel shows as storage used).
func (s *Store) UsedBytes() (int64, error) {
	var total int64
	err := filepath.WalkDir(filepath.Join(s.Root, "chunks"), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err == nil {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
