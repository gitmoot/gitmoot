package cli

import (
	"bytes"
	"context"
	"fmt"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"testing"
	"time"
)

func TestPipelineSuccessTriggerArmsAtAddAndReenable(t *testing.T) {
	home := t.TempDir()
	upstreamFile := writeSpec(t, "name: upstream\nstages: [{id: run, cmd: echo}]\n")
	if code := Run([]string{"pipeline", "add", upstreamFile, "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("add upstream exit=%d", code)
	}
	base := time.Now().UTC().Add(-time.Hour)
	if err := withStore(home, func(store *db.Store) error {
		seedPipelineRunState(t, store, "prun-before-add", "upstream", pipeline.RunSucceeded, base)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	downstreamFile := writeSpec(t, "name: downstream\nrepo: owner/downstream\ntrigger: {kind: pipeline, pipeline: upstream}\nstages: [{id: run, cmd: echo}]\n")
	var stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", downstreamFile, "--enable", "--home", home}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("add downstream exit=%d stderr=%s", code, stderr.String())
	}
	if err := withStore(home, func(store *db.Store) error {
		if err := pipeline.TriggerPipelineRuns(context.Background(), store, time.Now().UTC()); err != nil {
			return err
		}
		if runs, err := store.ListPipelineRuns(context.Background(), "downstream"); err != nil || len(runs) != 0 {
			return fmt.Errorf("add-time arm runs=%v err=%v", runs, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"pipeline", "disable", "downstream", "--home", home}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("disable exit=%d stderr=%s", code, stderr.String())
	}
	if err := withStore(home, func(store *db.Store) error {
		seedPipelineRunState(t, store, "prun-while-disabled", "upstream", pipeline.RunSucceeded, time.Now().UTC())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"pipeline", "enable", "downstream", "--home", home}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("enable exit=%d stderr=%s", code, stderr.String())
	}
	if err := withStore(home, func(store *db.Store) error {
		if err := pipeline.TriggerPipelineRuns(context.Background(), store, time.Now().UTC().Add(time.Minute)); err != nil {
			return err
		}
		runs, err := store.ListPipelineRuns(context.Background(), "downstream")
		if err != nil || len(runs) != 0 {
			return fmt.Errorf("re-enable arm runs=%v err=%v", runs, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
