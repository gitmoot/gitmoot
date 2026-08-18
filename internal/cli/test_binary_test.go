package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	gitmootTestBinaryOnce sync.Once
	gitmootTestBinaryDir  string
	gitmootTestBinaryPath string
	gitmootTestBinaryErr  error
)

func sharedGitmootTestBinary(t *testing.T) string {
	t.Helper()
	gitmootTestBinaryOnce.Do(func() {
		gitmootTestBinaryDir, gitmootTestBinaryErr = os.MkdirTemp("", "gitmoot-test-binary-*")
		if gitmootTestBinaryErr != nil {
			return
		}
		gitmootTestBinaryPath = filepath.Join(gitmootTestBinaryDir, "gitmoot")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			gitmootTestBinaryErr = fmt.Errorf("resolve repository root: %w", err)
			return
		}
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", gitmootTestBinaryPath, "./cmd/gitmoot")
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			gitmootTestBinaryErr = fmt.Errorf("build gitmoot test binary: %w\n%s", err, output)
		}
	})
	if gitmootTestBinaryErr != nil {
		t.Fatal(gitmootTestBinaryErr)
	}
	return gitmootTestBinaryPath
}

func cleanupSharedGitmootTestBinary() error {
	if gitmootTestBinaryDir == "" {
		return nil
	}
	return os.RemoveAll(gitmootTestBinaryDir)
}
