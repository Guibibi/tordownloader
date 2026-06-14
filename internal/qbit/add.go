package qbit

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strings"

	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torrentmeta"
)

// maxMultipartMemory caps in-memory multipart parsing; .torrent files are small.
const maxMultipartMemory = 32 << 20 // 32 MiB

// maxTorrentFetch caps the size of a .torrent fetched from an http(s) URL.
const maxTorrentFetch = 16 << 20 // 16 MiB

// torrentsAdd accepts releases from Sonarr/Radarr: magnet/http links in the
// `urls` field and/or uploaded .torrent files in the `torrents` multipart field.
// Each is reduced to its v1 infohash, persisted as QUEUED, and picked up by the
// engine submitter. Idempotent on infohash. Returns "Ok." if at least one
// torrent was accepted (or already tracked), else "Fails." as qBittorrent does.
func (h *Handler) torrentsAdd(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	category := r.FormValue("category")
	savePath, err := h.savePathFor(r.Context(), category)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	var accepted, failed int

	// 1) urls: newline-separated magnet links or http(s) .torrent URLs.
	for _, line := range strings.Split(r.FormValue("urls"), "\n") {
		u := strings.TrimSpace(line)
		if u == "" {
			continue
		}
		meta, src, err := h.metaFromURL(r.Context(), u)
		if err != nil {
			h.log.Warn("add: bad url", "url", clip(u, 80), "err", err)
			failed++
			continue
		}
		if h.persist(r.Context(), meta, src, category, savePath) {
			accepted++
		} else {
			failed++
		}
	}

	// 2) multipart .torrent file uploads under "torrents".
	if r.MultipartForm != nil {
		for _, fh := range r.MultipartForm.File["torrents"] {
			data, err := readMultipart(fh)
			if err != nil {
				h.log.Warn("add: read upload", "name", fh.Filename, "err", err)
				failed++
				continue
			}
			meta, err := torrentmeta.FromTorrent(data)
			if err != nil {
				h.log.Warn("add: parse upload", "name", fh.Filename, "err", err)
				failed++
				continue
			}
			if h.persist(r.Context(), meta, source{blob: data}, category, savePath) {
				accepted++
			} else {
				failed++
			}
		}
	}

	if accepted == 0 && failed > 0 {
		h.writeText(w, "Fails.")
		return
	}
	h.writeText(w, "Ok.")
}

// source carries the original submission material for a torrent: a magnet URI or
// .torrent bytes (exactly one is set).
type source struct {
	magnet string
	blob   []byte
}

// metaFromURL extracts identity from a single `urls` entry: a magnet link is
// parsed directly; an http(s) link is fetched and parsed as a .torrent.
func (h *Handler) metaFromURL(ctx context.Context, u string) (torrentmeta.Meta, source, error) {
	if torrentmeta.IsMagnet(u) {
		m, err := torrentmeta.FromMagnet(u)
		return m, source{magnet: u}, err
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		data, err := h.fetchTorrent(ctx, u)
		if err != nil {
			return torrentmeta.Meta{}, source{}, err
		}
		m, err := torrentmeta.FromTorrent(data)
		return m, source{blob: data}, err
	}
	return torrentmeta.Meta{}, source{}, fmt.Errorf("unsupported url scheme")
}

// persist writes a QUEUED torrent row. Returns true on success or when the
// torrent is already tracked (idempotent re-grab).
func (h *Handler) persist(ctx context.Context, meta torrentmeta.Meta, src source, category, savePath string) bool {
	t, created, err := h.store.AddTorrent(ctx, store.AddTorrentParams{
		Infohash:   meta.InfoHash,
		Name:       meta.Name,
		Category:   category,
		SavePath:   savePath,
		Magnet:     src.magnet,
		SourceBlob: src.blob,
	})
	if err != nil {
		h.log.Error("add: persist", "infohash", meta.InfoHash, "err", err)
		return false
	}
	if created {
		h.log.Info("torrent accepted", "infohash", t.Infohash, "name", t.Name, "category", category)
	} else {
		h.log.Info("torrent already tracked", "infohash", t.Infohash)
	}
	return true
}

// fetchTorrent downloads a .torrent file from an http(s) URL.
func (h *Handler) fetchTorrent(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch torrent: http %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentFetch))
	if err != nil {
		return nil, fmt.Errorf("fetch torrent: %w", err)
	}
	return data, nil
}

// savePathFor returns the save path for a category, creating the category with a
// default path (<root>/<category>) the first time it is seen. An empty category
// maps to the download root and is not persisted.
func (h *Handler) savePathFor(ctx context.Context, category string) (string, error) {
	if category == "" {
		return h.root, nil
	}
	if c, ok, err := h.store.GetCategory(ctx, category); err != nil {
		return "", err
	} else if ok {
		return c.SavePath, nil
	}
	savePath := path.Join(h.root, category)
	if err := h.store.UpsertCategory(ctx, category, savePath); err != nil {
		return "", err
	}
	return savePath, nil
}

// torrentsDelete removes torrents by `hashes` (pipe-separated, or "all").
// When deleteFiles is true, local content is also removed. Each torrent is
// deleted end-to-end via the engine: cancel any in-flight download, tell
// TorBox to delete, remove local files, and drop the DB row.
func (h *Handler) torrentsDelete(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	hashes := r.FormValue("hashes")
	deleteFiles := r.FormValue("deleteFiles") == "true"

	// Resolve actual hashes to delete.
	var toDelete []string
	if hashes == "all" {
		// Enumerate every tracked torrent.
		all, err := h.store.ListTorrents(r.Context(), store.TorrentFilter{})
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		for _, t := range all {
			toDelete = append(toDelete, t.Infohash)
		}
	} else {
		toDelete = splitHashes(hashes)
	}

	if h.deleteFn != nil {
		// Engine-driven: full lifecycle cleanup (TorBox + local files + DB).
		var failed int
		for _, hh := range toDelete {
			if err := h.deleteFn(r.Context(), hh, deleteFiles); err != nil {
				h.log.Error("delete torrent", "infohash", hh, "err", err)
				failed++
			}
		}
		if failed > 0 && failed == len(toDelete) && len(toDelete) > 0 {
			h.serverError(w, r, fmt.Errorf("all %d deletes failed", failed))
			return
		}
	} else {
		// Fallback: DB-only removal (used in tests that don't wire the engine).
		var (
			n   int64
			err error
		)
		if hashes == "all" {
			n, err = h.store.DeleteAll(r.Context())
		} else {
			n, err = h.store.DeleteByHashes(r.Context(), toDelete)
		}
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		h.log.Info("torrents deleted", "count", n)
	}
	w.WriteHeader(http.StatusOK)
}

// createCategory persists a category mapping. savePath defaults to <root>/<name>.
func (h *Handler) createCategory(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := r.FormValue("category")
	if name == "" {
		http.Error(w, "category name required", http.StatusBadRequest)
		return
	}
	savePath := r.FormValue("savePath")
	if savePath == "" {
		savePath = path.Join(h.root, name)
	}
	if err := h.store.UpsertCategory(r.Context(), name, savePath); err != nil {
		h.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// setCategory reassigns the given torrents to a category, ensuring it exists.
func (h *Handler) setCategory(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	category := r.FormValue("category")
	savePath, err := h.savePathFor(r.Context(), category)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	for _, hash := range splitHashes(r.FormValue("hashes")) {
		if err := h.store.SetTorrentCategory(r.Context(), hash, category, savePath); err != nil {
			h.serverError(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// removeCategories deletes category mappings named in `categories` (newline-sep).
func (h *Handler) removeCategories(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var names []string
	for _, n := range strings.Split(r.FormValue("categories"), "\n") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if err := h.store.RemoveCategories(r.Context(), names); err != nil {
		h.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// clip shortens s for log lines (magnet URIs are long).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// parseForm parses either a multipart or urlencoded body so FormValue and
// MultipartForm are populated.
func parseForm(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(maxMultipartMemory)
	}
	return r.ParseForm()
}

// readMultipart reads an uploaded file's bytes.
func readMultipart(fh *multipart.FileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxTorrentFetch))
}
