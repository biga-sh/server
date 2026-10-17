package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
)

// Blob encodings. hash and size always describe the raw content.
const (
	compressionNone   = "none"
	compressionBrotli = "br"
)

// brotliQuality trades a little ratio for much faster compression than the
// maximum of 11, which matters on a request path. Source code still shrinks
// four to six times.
const brotliQuality = 5

// maxDecodedBlob bounds decompression, so a corrupt row cannot expand
// without limit.
const maxDecodedBlob = 64 << 20

// compress encodes content for storage, keeping it raw when brotli would
// not make it smaller.
func compress(content string) ([]byte, string) {
	raw := []byte(content)
	if len(raw) < 64 {
		return raw, compressionNone
	}
	var buf bytes.Buffer
	buf.Grow(len(raw) / 3)
	w := brotli.NewWriterLevel(&buf, brotliQuality)
	if _, err := w.Write(raw); err != nil {
		return raw, compressionNone
	}
	if err := w.Close(); err != nil || buf.Len() >= len(raw) {
		return raw, compressionNone
	}
	return buf.Bytes(), compressionBrotli
}

// decompress decodes stored blob bytes back to text.
func decompress(data []byte, compression string) (string, error) {
	switch compression {
	case compressionNone, "":
		return string(data), nil
	case compressionBrotli:
		out, err := io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(data)), maxDecodedBlob+1))
		if err != nil {
			return "", fmt.Errorf("decode blob: %w", err)
		}
		if len(out) > maxDecodedBlob {
			return "", errors.New("decode blob: exceeds the size limit")
		}
		return string(out), nil
	default:
		return "", fmt.Errorf("decode blob: unknown compression %q", compression)
	}
}
