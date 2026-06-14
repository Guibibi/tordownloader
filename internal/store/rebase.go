package store

import (
	"context"
	"fmt"
	"path/filepath"
)

// RebaseToRoot makes the configured download root authoritative over save paths
// that were persisted on an earlier run. It recomputes every category's save
// path as <root>/<name>, and every *pending* torrent's save path as
// <root>/<category> (or <root> when uncategorized).
//
// Pending means nothing is on local disk yet — QUEUED, TORBOX_ACTIVE, and
// LOCAL_QUEUED. Torrents that have already written to disk are deliberately left
// alone: COMPLETE files live at their old save path (and content_path points
// there for Sonarr's import), an in-flight LOCAL_DOWNLOAD has staging under the
// old path, and ERROR is terminal. Rebasing those would orphan files.
//
// It returns the number of category and torrent rows actually changed, so the
// caller can log a one-line summary only when something moved.
func (s *Store) RebaseToRoot(ctx context.Context, root string) (cats, torrents int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("rebase: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Categories: collect first, then update (SQLite can't UPDATE while a query
	// on the same connection is still streaming rows).
	type catRow struct{ name, savePath string }
	var catRows []catRow
	rows, err := tx.QueryContext(ctx, `SELECT name, save_path FROM categories`)
	if err != nil {
		return 0, 0, fmt.Errorf("rebase: list categories: %w", err)
	}
	for rows.Next() {
		var c catRow
		if err := rows.Scan(&c.name, &c.savePath); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("rebase: scan category: %w", err)
		}
		catRows = append(catRows, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("rebase: iterate categories: %w", err)
	}
	rows.Close()

	for _, c := range catRows {
		want := filepath.Join(root, c.name)
		if want == c.savePath {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE categories SET save_path = ? WHERE name = ?`, want, c.name); err != nil {
			return 0, 0, fmt.Errorf("rebase: update category %s: %w", c.name, err)
		}
		cats++
	}

	// Pending torrents: nothing on local disk, safe to rebase.
	type torRow struct {
		id             int64
		category, path string
	}
	var torRows []torRow
	trows, err := tx.QueryContext(ctx,
		`SELECT id, category, save_path FROM torrents WHERE state IN (?, ?, ?)`,
		StateQueued, StateTorBoxActive, StateLocalQueued)
	if err != nil {
		return 0, 0, fmt.Errorf("rebase: list torrents: %w", err)
	}
	for trows.Next() {
		var t torRow
		if err := trows.Scan(&t.id, &t.category, &t.path); err != nil {
			trows.Close()
			return 0, 0, fmt.Errorf("rebase: scan torrent: %w", err)
		}
		torRows = append(torRows, t)
	}
	if err := trows.Err(); err != nil {
		trows.Close()
		return 0, 0, fmt.Errorf("rebase: iterate torrents: %w", err)
	}
	trows.Close()

	for _, t := range torRows {
		want := root
		if t.category != "" {
			want = filepath.Join(root, t.category)
		}
		if want == t.path {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE torrents SET save_path = ? WHERE id = ?`, want, t.id); err != nil {
			return 0, 0, fmt.Errorf("rebase: update torrent %d: %w", t.id, err)
		}
		torrents++
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("rebase: commit: %w", err)
	}
	return cats, torrents, nil
}
