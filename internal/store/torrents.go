package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Torrent is a row from the torrents table. It mirrors the schema in
// docs/ARCHITECTURE.md §5 and carries everything the qBittorrent layer needs to
// render a torrent object.
type Torrent struct {
	ID             int64
	Infohash       string
	TorBoxID       sql.NullInt64
	TorBoxQueuedID sql.NullInt64
	Name           string
	Category       string
	SavePath       string
	ContentPath    string
	Size           int64
	State          string
	TorBoxState    string
	TorBoxProgress float64
	LocalProgress  float64
	DLSpeed        int64
	Seeds          int
	Peers          int
	ETA            int64
	Cached         bool
	Error          string
	ArrNotified    bool
	AddedOn        int64
	ActiveSince    int64
	ProgressAt     int64
	// SpeedAt/SpeedBytes anchor the average-speed window: the time the window
	// opened and the byte count at that moment. The reconciler averages the
	// bytes gained since over the elapsed time to decide whether a fetch is
	// sustaining a useful speed, then re-anchors. Persisted so the window
	// survives a restart.
	SpeedAt     int64
	SpeedBytes  int64
	CompletedOn int64
	CreatedAt   int64
	UpdatedAt   int64
}

// File is a row from the files table.
type File struct {
	ID           int64
	TorrentID    int64
	TorBoxFileID sql.NullInt64
	RelPath      string
	ShortName    string
	Size         int64
	Downloaded   int64
	Done         bool
}

// TorrentFilter narrows ListTorrents. Zero values mean "no filter".
type TorrentFilter struct {
	// Category, if non-nil, restricts to torrents in that category (the empty
	// string is a valid category meaning "uncategorised").
	Category *string
	// Hashes, if non-empty, restricts to torrents whose infohash is in the set
	// (lowercase 40-hex). Unknown hashes are simply absent from the result.
	Hashes []string
}

const torrentColumns = `id, infohash, torbox_id, torbox_queued_id, name, category, save_path, content_path,
	size, state, torbox_state, torbox_progress, local_progress, dlspeed,
	seeds, peers, eta, cached, error, arr_notified,
	added_on, active_since, progress_at, speed_at, speed_bytes, completed_on, created_at, updated_at`

func scanTorrent(s interface{ Scan(...any) error }) (Torrent, error) {
	var t Torrent
	err := s.Scan(
		&t.ID, &t.Infohash, &t.TorBoxID, &t.TorBoxQueuedID, &t.Name, &t.Category, &t.SavePath, &t.ContentPath,
		&t.Size, &t.State, &t.TorBoxState, &t.TorBoxProgress, &t.LocalProgress, &t.DLSpeed,
		&t.Seeds, &t.Peers, &t.ETA, &t.Cached, &t.Error, &t.ArrNotified,
		&t.AddedOn, &t.ActiveSince, &t.ProgressAt, &t.SpeedAt, &t.SpeedBytes, &t.CompletedOn, &t.CreatedAt, &t.UpdatedAt,
	)
	return t, err
}

// ListTorrents returns torrents matching filter, newest first.
func (s *Store) ListTorrents(ctx context.Context, f TorrentFilter) ([]Torrent, error) {
	q := strings.Builder{}
	fmt.Fprintf(&q, "SELECT %s FROM torrents", torrentColumns)

	var (
		where []string
		args  []any
	)
	if f.Category != nil {
		where = append(where, "category = ?")
		args = append(args, *f.Category)
	}
	if len(f.Hashes) > 0 {
		placeholders := make([]string, len(f.Hashes))
		for i, h := range f.Hashes {
			placeholders[i] = "?"
			args = append(args, strings.ToLower(h))
		}
		where = append(where, "infohash IN ("+strings.Join(placeholders, ",")+")")
	}
	if len(where) > 0 {
		q.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	q.WriteString(" ORDER BY added_on DESC, id DESC")

	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list torrents: %w", err)
	}
	defer rows.Close()

	var out []Torrent
	for rows.Next() {
		t, err := scanTorrent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan torrent: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTorrent returns the torrent with the given infohash. The bool is false when
// no such torrent exists.
func (s *Store) GetTorrent(ctx context.Context, infohash string) (Torrent, bool, error) {
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT %s FROM torrents WHERE infohash = ?", torrentColumns),
		strings.ToLower(infohash))
	t, err := scanTorrent(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Torrent{}, false, nil
	case err != nil:
		return Torrent{}, false, fmt.Errorf("get torrent %s: %w", infohash, err)
	}
	return t, true, nil
}

// ListFiles returns the files belonging to a torrent, ordered by torbox file id.
func (s *Store) ListFiles(ctx context.Context, torrentID int64) ([]File, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, torrent_id, torbox_file_id, rel_path, short_name, size, downloaded, done
		 FROM files WHERE torrent_id = ? ORDER BY torbox_file_id, id`, torrentID)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()

	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.TorrentID, &f.TorBoxFileID, &f.RelPath, &f.ShortName,
			&f.Size, &f.Downloaded, &f.Done); err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
