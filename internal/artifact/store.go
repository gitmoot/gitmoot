package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	HashPrefix = "sha256:"
)

type Blob struct {
	Hash string
	Size int64
	Path string
}

type Store struct {
	Root string
}

func NewStore(root string) Store {
	return Store{Root: root}
}

func (s Store) Put(content []byte) (Blob, error) {
	root := strings.TrimSpace(s.Root)
	if root == "" {
		return Blob{}, errors.New("artifact blob root is required")
	}
	hash := ContentHash(content)
	path, err := s.Path(hash)
	if err != nil {
		return Blob{}, err
	}
	if info, err := os.Stat(path); err == nil {
		if info.Size() != int64(len(content)) {
			return Blob{}, fmt.Errorf("artifact blob %s has size %d, want %d", hash, info.Size(), len(content))
		}
		return Blob{Hash: hash, Size: info.Size(), Path: path}, nil
	} else if !os.IsNotExist(err) {
		return Blob{}, fmt.Errorf("stat artifact blob: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Blob{}, fmt.Errorf("create artifact blob directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".blob-*")
	if err != nil {
		return Blob{}, fmt.Errorf("create temporary artifact blob: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return Blob{}, fmt.Errorf("write artifact blob: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return Blob{}, fmt.Errorf("chmod artifact blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Blob{}, fmt.Errorf("close artifact blob: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			committed = true
			return Blob{Hash: hash, Size: int64(len(content)), Path: path}, nil
		}
		return Blob{}, fmt.Errorf("commit artifact blob: %w", err)
	}
	committed = true
	return Blob{Hash: hash, Size: int64(len(content)), Path: path}, nil
}

func (s Store) Read(hash string) ([]byte, error) {
	path, err := s.Path(hash)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("artifact blob %s not found", NormalizeHashForMessage(hash))
		}
		return nil, fmt.Errorf("read artifact blob: %w", err)
	}
	return content, nil
}

func (s Store) Path(hash string) (string, error) {
	normalized, err := NormalizeHash(hash)
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(s.Root)
	if root == "" {
		return "", errors.New("artifact blob root is required")
	}
	hexHash := strings.TrimPrefix(normalized, HashPrefix)
	return filepath.Join(root, "sha256", hexHash[:2], hexHash), nil
}

func ContentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return HashPrefix + hex.EncodeToString(sum[:])
}

func NormalizeHash(hash string) (string, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	hash = strings.TrimPrefix(hash, HashPrefix)
	if len(hash) != sha256.Size*2 {
		return "", fmt.Errorf("artifact hash must be %d hex characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", fmt.Errorf("artifact hash is not valid hex: %w", err)
	}
	return HashPrefix + hash, nil
}

func NormalizeHashForMessage(hash string) string {
	normalized, err := NormalizeHash(hash)
	if err != nil {
		return strings.TrimSpace(hash)
	}
	return normalized
}
