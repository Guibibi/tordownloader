package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Category maps a category name to the save path used for its torrents.
type Category struct {
	Name     string
	SavePath string
}

// UpsertCategory creates or updates a category's save path.
func (s *Store) UpsertCategory(ctx context.Context, name, savePath string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO categories (name, save_path) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET save_path = excluded.save_path`, name, savePath)
	if err != nil {
		return fmt.Errorf("upsert category %s: %w", name, err)
	}
	return nil
}

// GetCategory returns the named category. The bool is false when absent.
func (s *Store) GetCategory(ctx context.Context, name string) (Category, bool, error) {
	var c Category
	err := s.db.QueryRowContext(ctx,
		`SELECT name, save_path FROM categories WHERE name = ?`, name).Scan(&c.Name, &c.SavePath)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Category{}, false, nil
	case err != nil:
		return Category{}, false, fmt.Errorf("get category %s: %w", name, err)
	}
	return c, true, nil
}

// RemoveCategories deletes the named categories. Torrents keep their category
// string; only the mapping is removed (matching qBittorrent behaviour).
func (s *Store) RemoveCategories(ctx context.Context, names []string) error {
	for _, n := range names {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM categories WHERE name = ?`, n); err != nil {
			return fmt.Errorf("remove category %s: %w", n, err)
		}
	}
	return nil
}

// SetTorrentCategory reassigns a torrent's category and save path.
func (s *Store) SetTorrentCategory(ctx context.Context, infohash, category, savePath string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE torrents SET category = ?, save_path = ?, updated_at = ? WHERE infohash = ?`,
		category, savePath, now, strings.ToLower(infohash))
	if err != nil {
		return fmt.Errorf("set category for %s: %w", infohash, err)
	}
	return nil
}

// ListCategories returns all categories ordered by name.
func (s *Store) ListCategories(ctx context.Context) ([]Category, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, save_path FROM categories ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	defer rows.Close()

	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.Name, &c.SavePath); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
