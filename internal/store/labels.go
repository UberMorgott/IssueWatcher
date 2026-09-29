package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// ProjectLabels returns project id's stored labels (in the platform's order)
// and when they were fetched (zero = never).
func (s *Store) ProjectLabels(ctx context.Context, id int64) ([]provider.Label, time.Time, error) {
	var at string
	err := s.rd.QueryRowContext(ctx, `SELECT labels_at FROM projects WHERE id = ?`, id).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: project labels: %w", err)
	}
	fetched, _ := time.Parse(timeFormat, at)
	rows, err := s.rd.QueryContext(ctx, `SELECT name, color, description FROM project_labels WHERE project_id = ? ORDER BY rowid`, id)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: project labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []provider.Label{}
	for rows.Next() {
		var l provider.Label
		if err := rows.Scan(&l.Name, &l.Color, &l.Description); err != nil {
			return nil, time.Time{}, fmt.Errorf("store: scan label: %w", err)
		}
		out = append(out, l)
	}
	return out, fetched, rows.Err()
}

// SetProjectLabels replaces project id's stored labels, fetched at at.
func (s *Store) SetProjectLabels(ctx context.Context, id int64, labels []provider.Label, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE projects SET labels_at = ? WHERE id = ?`, ts(at), id)
	if err != nil {
		return fmt.Errorf("store: set labels: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_labels WHERE project_id = ?`, id); err != nil {
		return fmt.Errorf("store: set labels: %w", err)
	}
	for _, l := range labels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_labels (project_id, name, color, description) VALUES (?, ?, ?, ?)
			ON CONFLICT (project_id, name) DO NOTHING`, id, l.Name, l.Color, l.Description); err != nil {
			return fmt.Errorf("store: set labels: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit labels: %w", err)
	}
	return nil
}
