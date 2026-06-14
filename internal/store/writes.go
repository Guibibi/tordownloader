package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AddTorrentParams describes a torrent being accepted from Sonarr/Radarr. Either
// Magnet or SourceBlob carries the source we later submit to TorBox.
type AddTorrentParams struct {
	Infohash   string
	Name       string
	Category   string
	SavePath   string
	Magnet     string
	SourceBlob []byte
	Size       int64
}

// AddTorrent inserts a new QUEUED torrent, or returns the existing row if one
// with the same infohash is already tracked (idempotent on re-grab). The bool is
// true when a new row was created.
func (s *Store) AddTorrent(ctx context.Context, p AddTorrentParams) (Torrent, bool, error) {
	infohash := strings.ToLower(p.Infohash)
	now := time.Now().Unix()

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO torrents (infohash, name, category, save_path, magnet, source_blob,
			size, state, added_on, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(infohash) DO NOTHING`,
		infohash, p.Name, p.Category, p.SavePath, p.Magnet, blobOrNil(p.SourceBlob),
		p.Size, StateQueued, now, now, now)
	if err != nil {
		return Torrent{}, false, fmt.Errorf("add torrent: %w", err)
	}
	affected, _ := res.RowsAffected()

	t, ok, err := s.GetTorrent(ctx, infohash)
	if err != nil {
		return Torrent{}, false, err
	}
	if !ok {
		return Torrent{}, false, fmt.Errorf("add torrent: row vanished after insert: %s", infohash)
	}
	return t, affected > 0, nil
}

// Source returns the submission source for a torrent: its magnet (preferred) and
// the .torrent bytes, if any. Used by the engine submitter.
func (s *Store) Source(ctx context.Context, id int64) (magnet string, blob []byte, err error) {
	row := s.db.QueryRowContext(ctx, `SELECT magnet, source_blob FROM torrents WHERE id = ?`, id)
	if err := row.Scan(&magnet, &blob); err != nil {
		return "", nil, fmt.Errorf("source %d: %w", id, err)
	}
	return magnet, blob, nil
}

// CountByState returns how many torrents are in the given internal state.
func (s *Store) CountByState(ctx context.Context, state string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM torrents WHERE state = ?`, state).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count state %s: %w", state, err)
	}
	return n, nil
}

// ListByState returns torrents in the given state, oldest first (FIFO for the
// submission queue).
func (s *Store) ListByState(ctx context.Context, state string) ([]Torrent, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf("SELECT %s FROM torrents WHERE state = ? ORDER BY added_on, id", torrentColumns), state)
	if err != nil {
		return nil, fmt.Errorf("list state %s: %w", state, err)
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

// MarkActive records a successful createtorrent: it stores the TorBox id and
// transitions the torrent to TORBOX_ACTIVE, starting the fail-fast clock.
func (s *Store) MarkActive(ctx context.Context, id int64, torboxID int) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE torrents SET torbox_id = ?, state = ?, active_since = ?, updated_at = ?
		WHERE id = ?`, torboxID, StateTorBoxActive, now, now, id)
	if err != nil {
		return fmt.Errorf("mark active %d: %w", id, err)
	}
	return nil
}

// TorBoxStatus carries the reconciler's view of a TORBOX_ACTIVE torrent's
// TorBox-side state, applied by UpdateTorBoxStatus.
type TorBoxStatus struct {
	State    string  // TorBox download_state (e.g. downloading, cached)
	Progress float64 // 0..1
	DLSpeed  int64   // bytes/s
	Size     int64   // 0 = leave the stored size unchanged
	Name     string  // "" = leave the stored name unchanged
}

// UpdateTorBoxStatus refreshes the TorBox-side fields of a torrent while it is
// fetching. It only touches rows still in TORBOX_ACTIVE, so a concurrent delete
// or transition is never clobbered. Size and Name are only overwritten when set,
// so a later, richer mylist reading can't blank out earlier data.
func (s *Store) UpdateTorBoxStatus(ctx context.Context, id int64, st TorBoxStatus) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE torrents SET
			torbox_state    = ?,
			torbox_progress = ?,
			dlspeed         = ?,
			size            = CASE WHEN ? > 0  THEN ? ELSE size END,
			name            = CASE WHEN ? <> '' THEN ? ELSE name END,
			updated_at      = ?
		WHERE id = ? AND state = ?`,
		st.State, st.Progress, st.DLSpeed,
		st.Size, st.Size,
		st.Name, st.Name,
		now, id, StateTorBoxActive)
	if err != nil {
		return fmt.Errorf("update torbox status %d: %w", id, err)
	}
	return nil
}

// FileInput is one file to record when a torrent's content becomes available on
// TorBox (see MarkLocalQueued). RelPath is the full path within the torrent
// (TorBox file `name`), which preserves folder structure on disk.
type FileInput struct {
	TorBoxFileID int
	RelPath      string
	ShortName    string
	Size         int64
}

// MarkLocalQueued records that TorBox has the content and moves the torrent from
// TORBOX_ACTIVE to LOCAL_QUEUED: it replaces the file rows, sets content_path,
// marks torbox_progress complete, and refreshes size when known. The whole
// transition is one transaction and is a no-op if the torrent already left
// TORBOX_ACTIVE (e.g. deleted concurrently), so no orphan file rows are left.
func (s *Store) MarkLocalQueued(ctx context.Context, id int64, contentPath string, size int64, files []FileInput) error {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mark local queued %d: begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE torrents SET
			state           = ?,
			torbox_progress = 1,
			content_path    = ?,
			size            = CASE WHEN ? > 0 THEN ? ELSE size END,
			updated_at      = ?
		WHERE id = ? AND state = ?`,
		StateLocalQueued, contentPath, size, size, now, id, StateTorBoxActive)
	if err != nil {
		return fmt.Errorf("mark local queued %d: update torrent: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Torrent is no longer TORBOX_ACTIVE; leave it (and its files) untouched.
		return nil
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE torrent_id = ?`, id); err != nil {
		return fmt.Errorf("mark local queued %d: clear files: %w", id, err)
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO files (torrent_id, torbox_file_id, rel_path, short_name, size)
			VALUES (?,?,?,?,?)`,
			id, f.TorBoxFileID, f.RelPath, f.ShortName, f.Size); err != nil {
			return fmt.Errorf("mark local queued %d: insert file: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark local queued %d: commit: %w", id, err)
	}
	return nil
}

// MarkError moves a torrent to ERROR with a human-readable reason.
func (s *Store) MarkError(ctx context.Context, id int64, reason string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE torrents SET state = ?, error = ?, updated_at = ? WHERE id = ?`,
		StateError, reason, now, id)
	if err != nil {
		return fmt.Errorf("mark error %d: %w", id, err)
	}
	return nil
}

// DeleteByHashes removes the torrents with the given infohashes (files cascade)
// and returns how many rows were deleted. M3 only drops local rows; TorBox-side
// deletion and local file removal are added in M6.
func (s *Store) DeleteByHashes(ctx context.Context, hashes []string) (int64, error) {
	if len(hashes) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(hashes))
	args := make([]any, len(hashes))
	for i, h := range hashes {
		placeholders[i] = "?"
		args[i] = strings.ToLower(h)
	}
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM torrents WHERE infohash IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return 0, fmt.Errorf("delete torrents: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteAll removes every torrent (and its files). Returns the count removed.
func (s *Store) DeleteAll(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM torrents")
	if err != nil {
		return 0, fmt.Errorf("delete all torrents: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func blobOrNil(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
