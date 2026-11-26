package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	// ErrNotFound indicates the requested blob key does not exist in the store.
	ErrNotFound = errors.New("blob: not found")
	// ErrDigestMismatch indicates a SHA-256 format or content digest mismatch.
	ErrDigestMismatch = errors.New("blob: digest mismatch")
	// ErrInvalidKey indicates a malformed or unsafe object key or prefix.
	ErrInvalidKey = errors.New("blob: invalid key")
)

// ObjectMeta describes a stored blob object and its verified SHA-256 digest.
type ObjectMeta struct {
	Key       string `json:"key"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Store defines the streaming content-verified blob storage boundary.
type Store interface {
	// Put streams r into key. If expectedSHA256 is non-empty (64-char lowercase hex),
	// it verifies the stream digest before committing and returns ErrDigestMismatch on mismatch.
	Put(ctx context.Context, key string, r io.Reader, expectedSHA256 string) (ObjectMeta, error)
	// Get opens a verified stream for key. If expectedSHA256 is non-empty, the returned
	// ReadCloser verifies the full stream against expectedSHA256 (and stored metadata) at EOF.
	Get(ctx context.Context, key string, expectedSHA256 string) (io.ReadCloser, ObjectMeta, error)
	// Stat returns stored metadata for key without reading object bytes.
	Stat(ctx context.Context, key string) (ObjectMeta, error)
	// Delete removes key idempotently, returning nil if the object does not exist.
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes all objects whose keys begin with prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}

// ValidateKey validates that key is a relative slash-separated object path with segments
// containing only [A-Za-z0-9._-] and rejecting empty segments, '.', '..', '\', and control chars.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: key must not be empty", ErrInvalidKey)
	}
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("%w: key %q must not have leading or trailing '/'", ErrInvalidKey, key)
	}
	segments := strings.Split(key, "/")
	for _, seg := range segments {
		if err := validateSegment(seg, key); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePrefix validates a key prefix for DeletePrefix. An empty prefix "" matches all objects;
// otherwise it must consist of valid slash-separated key segments with an optional trailing '/'.
func ValidatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(prefix, "/")
	if trimmed == "" {
		return fmt.Errorf("%w: prefix %q must not be '/'", ErrInvalidKey, prefix)
	}
	return ValidateKey(trimmed)
}

func validateSegment(seg, full string) error {
	if seg == "" || seg == "." || seg == ".." {
		return fmt.Errorf("%w: invalid path segment %q in %q", ErrInvalidKey, seg, full)
	}
	for i := 0; i < len(seg); i++ {
		if !isAllowedKeyByte(seg[i]) {
			return fmt.Errorf("%w: invalid character %q in %q", ErrInvalidKey, seg[i], full)
		}
	}
	return nil
}

func isAllowedKeyByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		b == '.' || b == '_' || b == '-'
}

// IsValidSHA256Hex reports whether digest is a 64-character lowercase hexadecimal SHA-256 string.
func IsValidSHA256Hex(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
