package db

import "context"

// ListDashboardMessages is the local operator view, not a role-scoped inbox.
// The dashboard bounds each thread while retaining its pending obligations.
func (s *Store) ListDashboardMessages(ctx context.Context) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, messageSelect+` ORDER BY m.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}
