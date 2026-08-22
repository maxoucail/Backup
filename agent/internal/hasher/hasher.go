// Package hasher computes the whole-file SHA-256 (used for the cheap
// unchanged-file check) and splits file content into fixed-size,
// content-addressed chunks for upload. Chunking is deliberately simple
// (sequential fixed-size slices, not a rolling content-defined boundary):
// it is fast, has zero edge cases, and still gives the two things that
// matter in practice - whole unchanged files cost nothing to re-back-up,
// and a changed large file only re-uploads the chunks that actually moved
// past the edit point onward.
package hasher

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

type Result struct {
	SHA256 string
	Chunks []string // chunk hashes, in order; empty for a zero-byte file
	Size   int64
}

func HashAndChunk(path string, chunkSize int64) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	whole := sha256.New()
	var chunks []string
	var total int64
	buf := make([]byte, 1<<20) // 1 MiB read buffer

	chunkHasher := sha256.New()
	var chunkWritten int64

	flushChunk := func() {
		if chunkWritten == 0 {
			return
		}
		chunks = append(chunks, hex.EncodeToString(chunkHasher.Sum(nil)))
		chunkHasher = sha256.New()
		chunkWritten = 0
	}

	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			data := buf[:n]
			whole.Write(data)
			total += int64(n)

			offset := 0
			for offset < n {
				remaining := chunkSize - chunkWritten
				take := int64(n - offset)
				if take > remaining {
					take = remaining
				}
				chunkHasher.Write(data[offset : int64(offset)+take])
				chunkWritten += take
				offset += int(take)
				if chunkWritten >= chunkSize {
					flushChunk()
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	flushChunk()

	return &Result{
		SHA256: hex.EncodeToString(whole.Sum(nil)),
		Chunks: chunks,
		Size:   total,
	}, nil
}

// ChunkReader streams the bytes of a single chunk (identified by its index
// and the file's chunk size) for upload, without loading the whole file.
func ChunkReader(path string, index int, chunkSize int64) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(int64(index)*chunkSize, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &limitedReadCloser{r: io.LimitReader(f, chunkSize), c: f}, nil
}

type limitedReadCloser struct {
	r io.Reader
	c io.Closer
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l *limitedReadCloser) Close() error               { return l.c.Close() }
