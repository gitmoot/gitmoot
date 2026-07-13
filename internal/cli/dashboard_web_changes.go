package cli

import (
	"context"
	"fmt"

	"github.com/jerryfane/gitmoot/internal/db"
)

// ChangeCursor returns the dashboard's opaque liveness cursor. The dashboard
// uses it only as an invalidation signal; page data still comes from the normal
// read-only endpoints.
func (d *webDataSource) ChangeCursor(ctx context.Context) (string, error) {
	cursor := "0.0.0"
	err := withStore(d.home, func(store *db.Store) error {
		jobEventID, workflowNoteID, taskEventID, err := store.DashboardChangeCursor(ctx)
		if err != nil {
			return err
		}
		cursor = fmt.Sprintf("%d.%d.%d", jobEventID, workflowNoteID, taskEventID)
		return nil
	})
	return cursor, err
}
