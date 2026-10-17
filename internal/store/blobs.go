package store

import (
	"context"
	"fmt"
)

// BlobInput is file content to store.
type BlobInput struct {
	Hash    string
	Content string
	Size    int64
	Binary  bool
}

// EncodedBlob is a blob compressed and ready to insert. Encode outside a
// transaction: compression is the expensive part of a write.
type EncodedBlob struct {
	Hash        string
	Data        []byte
	Compression string
	Size        int32
	Binary      bool
}

// EncodeBlobs compresses blobs for [Queries.InsertBlobs].
func EncodeBlobs(in []BlobInput) []EncodedBlob {
	out := make([]EncodedBlob, len(in))
	for i, b := range in {
		data, codec := []byte{}, compressionNone
		if !b.Binary {
			data, codec = compress(b.Content)
		}
		out[i] = EncodedBlob{Hash: b.Hash, Data: data, Compression: codec, Size: int32(b.Size), Binary: b.Binary}
	}
	return out
}

// insertChunkBytes bounds one INSERT statement's payload.
const insertChunkBytes = 16 << 20

// InsertBlobs stores blobs not already stored for the session and returns
// how many bytes the new ones occupy.
func (q *Queries) InsertBlobs(ctx context.Context, sessionID string, blobs []EncodedBlob) (int64, error) {
	var stored int64
	for start := 0; start < len(blobs); {
		end, size := start, 0
		for end < len(blobs) && (end == start || size+len(blobs[end].Data) <= insertChunkBytes) {
			size += len(blobs[end].Data)
			end++
		}
		n, err := q.insertBlobChunk(ctx, sessionID, blobs[start:end])
		if err != nil {
			return 0, err
		}
		stored += n
		start = end
	}
	return stored, nil
}

func (q *Queries) insertBlobChunk(ctx context.Context, sessionID string, blobs []EncodedBlob) (int64, error) {
	hashes := make([]string, len(blobs))
	data := make([][]byte, len(blobs))
	codecs := make([]string, len(blobs))
	sizes := make([]int32, len(blobs))
	binary := make([]bool, len(blobs))
	for i, b := range blobs {
		hashes[i], data[i], codecs[i], sizes[i], binary[i] = b.Hash, b.Data, b.Compression, b.Size, b.Binary
	}
	rows, err := q.db.Query(ctx, `
		INSERT INTO blobs (session_id, hash, content, compression, size, "binary")
		SELECT $1, t.hash, t.content, t.codec, t.size, t.bin
		FROM unnest($2::text[], $3::bytea[], $4::text[], $5::int[], $6::bool[]) AS t(hash, content, codec, size, bin)
		ON CONFLICT (session_id, hash) DO NOTHING
		RETURNING octet_length(content)`,
		sessionID, hashes, data, codecs, sizes, binary)
	if err != nil {
		return 0, fmt.Errorf("insert blobs: %w", err)
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, rows.Err()
}

// Blob is stored file content.
type Blob struct {
	Content string
	Binary  bool
}

// Blobs reads the listed blobs of a session; hashes without a blob are
// absent from the result.
func (q *Queries) Blobs(ctx context.Context, sessionID string, hashes []string) (map[string]Blob, error) {
	out := make(map[string]Blob, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx,
		`SELECT hash, content, compression, "binary" FROM blobs WHERE session_id = $1 AND hash = ANY($2)`,
		sessionID, hashes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			hash, codec string
			data        []byte
			b           Blob
		)
		if err := rows.Scan(&hash, &data, &codec, &b.Binary); err != nil {
			return nil, err
		}
		if b.Content, err = decompress(data, codec); err != nil {
			return nil, err
		}
		out[hash] = b
	}
	return out, rows.Err()
}

// HeadContents maps hash to content for every current text file.
func (q *Queries) HeadContents(ctx context.Context, sessionID string) (map[string]string, error) {
	rows, err := q.db.Query(ctx, `
		SELECT DISTINCT b.hash, b.content, b.compression
		FROM files f
		JOIN blobs b ON b.session_id = f.session_id AND b.hash = f.hash
		WHERE f.session_id = $1 AND NOT b."binary"`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var (
			hash, codec string
			data        []byte
		)
		if err := rows.Scan(&hash, &data, &codec); err != nil {
			return nil, err
		}
		if out[hash], err = decompress(data, codec); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}
