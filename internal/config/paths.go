package config

import (
	"os"
	"path/filepath"
)

const (
	DirName    = ".gitmoot"
	ConfigName = "config.toml"
	DBName     = "gitmoot.db"
	LogsDir    = "logs"
	WorkDir    = "workspaces"
	EvalsDir   = "evals"
	BlobsDir   = "blobs"
)

type Paths struct {
	Home          string
	ConfigFile    string
	Database      string
	Logs          string
	Workspaces    string
	Evals         string
	ArtifactBlobs string
}

func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	return PathsForHome(home), nil
}

func PathsForHome(home string) Paths {
	root := filepath.Join(home, DirName)
	return Paths{
		Home:          root,
		ConfigFile:    filepath.Join(root, ConfigName),
		Database:      filepath.Join(root, DBName),
		Logs:          filepath.Join(root, LogsDir),
		Workspaces:    filepath.Join(root, WorkDir),
		Evals:         filepath.Join(root, EvalsDir),
		ArtifactBlobs: filepath.Join(root, EvalsDir, BlobsDir),
	}
}

// OMPRuntimeDir is where the Gitmoot OMP inbox add-on registers each running
// OMP runtime (registration JSON plus its private delivery socket). Mode 0700.
func (p Paths) OMPRuntimeDir() string {
	return filepath.Join(p.Home, "run", "omp")
}
