package artifact

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestStorePutDedupeAndRead(t *testing.T) {
	store := NewStore(t.TempDir())
	content := []byte("hello artifact\n")

	first, err := store.Put(content)
	if err != nil {
		t.Fatalf("Put first returned error: %v", err)
	}
	second, err := store.Put(content)
	if err != nil {
		t.Fatalf("Put second returned error: %v", err)
	}
	if first.Hash != second.Hash || first.Path != second.Path {
		t.Fatalf("dedupe blob mismatch: first=%+v second=%+v", first, second)
	}
	if first.Size != int64(len(content)) {
		t.Fatalf("blob size = %d, want %d", first.Size, len(content))
	}
	read, err := store.Read(first.Hash)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if string(read) != string(content) {
		t.Fatalf("read content = %q, want %q", string(read), string(content))
	}
	if !strings.Contains(first.Path, strings.TrimPrefix(first.Hash, HashPrefix)[:2]) {
		t.Fatalf("blob path does not shard by hash: %s", first.Path)
	}
}

func TestStoreReadMissingBlob(t *testing.T) {
	store := NewStore(t.TempDir())
	hash := ContentHash([]byte("missing"))

	_, err := store.Read(hash)
	if err == nil {
		t.Fatal("Read missing blob returned nil error")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read returned raw os.ErrNotExist, want contextual error")
	}
	if !strings.Contains(err.Error(), hash) {
		t.Fatalf("Read error = %q, want hash", err.Error())
	}
}

func TestNormalizeHashRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{"", "abc", "sha256:not-hex"} {
		t.Run(input, func(t *testing.T) {
			if _, err := NormalizeHash(input); err == nil {
				t.Fatalf("NormalizeHash(%q) returned nil error", input)
			}
		})
	}
}
