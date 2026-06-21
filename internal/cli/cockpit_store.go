package cli

import (
	"context"

	"github.com/jerryfane/gitmoot/internal/cockpit"
	"github.com/jerryfane/gitmoot/internal/db"
)

// cockpitPaneStore adapts a *db.Store to the cockpit.PaneStore interface. The
// cockpit package keeps its own cockpit.Pane record (so it builds in isolation)
// while the store persists field-identical db.CockpitPane rows; this shim
// converts cockpit.Pane <-> db.CockpitPane field-for-field and passes the
// identical-signature methods straight through.
type cockpitPaneStore struct {
	store *db.Store
}

func (s cockpitPaneStore) InsertCockpitPane(ctx context.Context, pane cockpit.Pane) error {
	return s.store.InsertCockpitPane(ctx, toDBCockpitPane(pane))
}

func (s cockpitPaneStore) GetCockpitPaneByJob(ctx context.Context, jobID string) (cockpit.Pane, error) {
	pane, err := s.store.GetCockpitPaneByJob(ctx, jobID)
	if err != nil {
		return cockpit.Pane{}, err
	}
	return fromDBCockpitPane(pane), nil
}

func (s cockpitPaneStore) GetCockpitPaneByKey(ctx context.Context, workspaceID, paneKey string) (cockpit.Pane, bool, error) {
	pane, found, err := s.store.GetCockpitPaneByKey(ctx, workspaceID, paneKey)
	if err != nil || !found {
		return cockpit.Pane{}, found, err
	}
	return fromDBCockpitPane(pane), true, nil
}

func (s cockpitPaneStore) ListCockpitPanesByRoot(ctx context.Context, rootJobID string) ([]cockpit.Pane, error) {
	panes, err := s.store.ListCockpitPanesByRoot(ctx, rootJobID)
	if err != nil {
		return nil, err
	}
	out := make([]cockpit.Pane, 0, len(panes))
	for _, pane := range panes {
		out = append(out, fromDBCockpitPane(pane))
	}
	return out, nil
}

func (s cockpitPaneStore) ListAllCockpitPanes(ctx context.Context) ([]cockpit.Pane, error) {
	panes, err := s.store.ListAllCockpitPanes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]cockpit.Pane, 0, len(panes))
	for _, pane := range panes {
		out = append(out, fromDBCockpitPane(pane))
	}
	return out, nil
}

func (s cockpitPaneStore) DeleteCockpitPane(ctx context.Context, id string) error {
	return s.store.DeleteCockpitPane(ctx, id)
}

func (s cockpitPaneStore) DeleteCockpitPaneByJob(ctx context.Context, jobID string) error {
	return s.store.DeleteCockpitPaneByJob(ctx, jobID)
}

func (s cockpitPaneStore) GetOrCreateWorkspaceForRoot(ctx context.Context, rootJobID string, create func() (workspaceID string, err error)) (string, error) {
	return s.store.GetOrCreateWorkspaceForRoot(ctx, rootJobID, create)
}

func toDBCockpitPane(p cockpit.Pane) db.CockpitPane {
	return db.CockpitPane{
		ID:          p.ID,
		JobID:       p.JobID,
		PaneKey:     p.PaneKey,
		RootJobID:   p.RootJobID,
		PaneID:      p.PaneID,
		WorkspaceID: p.WorkspaceID,
		Source:      p.Source,
		CreatedAt:   p.CreatedAt,
	}
}

func fromDBCockpitPane(p db.CockpitPane) cockpit.Pane {
	return cockpit.Pane{
		ID:          p.ID,
		JobID:       p.JobID,
		PaneKey:     p.PaneKey,
		RootJobID:   p.RootJobID,
		PaneID:      p.PaneID,
		WorkspaceID: p.WorkspaceID,
		Source:      p.Source,
		CreatedAt:   p.CreatedAt,
	}
}
