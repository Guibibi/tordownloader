package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The store must cap its connection pool at one. SQLite allows a single writer;
// database/sql's default pool opens connections on demand, so concurrent writers
// (the parallel per-file downloaders, the progress reporter, the reconcile/submit
// passes, and Sonarr's bulk /torrents/add) collide on the write lock and return
// SQLITE_BUSY under sustained load — even with WAL + busy_timeout, which only
// bounds the wait, not the contention. Pinning to one connection serializes all
// access in-process so SQLite never sees a conflict. This pins that invariant.
func TestWritePoolIsSingleConnection(t *testing.T) {
	st := newWritesStore(t)
	if got := st.DB().Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1 (single-writer serialization)", got)
	}
}

// Serializing every operation onto one connection introduces one real hazard: if
// a code path ever held the connection inside a transaction and then called
// another store method (which needs the same single connection), it would
// deadlock forever. This drives the real write paths — including the multi-
// statement MarkLocalQueued transaction — concurrently and fails loudly on a
// hang, so such a regression can't slip in unnoticed.
func TestConcurrentWritesDoNotDeadlock(t *testing.T) {
	ctx := context.Background()
	st := newWritesStore(t)

	// One torrent partway through a local download (many files to mark done).
	tr, _, err := st.AddTorrent(ctx, AddTorrentParams{Infohash: hash(1), Name: "pack"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkActive(ctx, tr.ID, 1000); err != nil {
		t.Fatal(err)
	}
	const nFiles = 64
	files := make([]FileInput, nFiles)
	for i := range files {
		files[i] = FileInput{TorBoxFileID: i, RelPath: hash(i) + ".mkv", Size: 1 << 20}
	}
	if err := st.MarkLocalQueued(ctx, tr.ID, "/dl/pack", int64(nFiles)<<20, files); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkLocalDownloading(ctx, tr.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListFiles(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}

	// A handful of TORBOX_ACTIVE torrents ready to be handed off via the multi-
	// statement MarkLocalQueued transaction, run concurrently with single writes.
	const nActive = 8
	active := make([]int64, nActive)
	for i := range active {
		at, _, err := st.AddTorrent(ctx, AddTorrentParams{Infohash: hash(500 + i), Name: "active"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.MarkActive(ctx, at.ID, 2000+i); err != nil {
			t.Fatal(err)
		}
		active[i] = at.ID
	}

	var (
		mu   sync.Mutex
		errs []error
	)
	record := func(err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup

	// Parallel per-file completion (the downloader's FileDone workers).
	for _, f := range rows {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			<-start
			record(st.MarkFileDone(ctx, id, 1<<20))
		}(f.ID)
	}
	// Progress reporter hammering the downloading torrent.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 50; i++ {
			record(st.UpdateLocalProgress(ctx, tr.ID, float64(i)/50, 1<<20))
		}
	}()
	// Multi-statement transactions (content hand-off) interleaved with the rest.
	for i, id := range active {
		wg.Add(1)
		go func(id int64, n int) {
			defer wg.Done()
			<-start
			record(st.MarkLocalQueued(ctx, id, "/dl/a", 1<<20,
				[]FileInput{{TorBoxFileID: n, RelPath: hash(900+n) + ".mkv", Size: 1 << 20}}))
		}(id, i)
	}
	// Sonarr bulk-adding new releases on other goroutines.
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			_, _, err := st.AddTorrent(ctx, AddTorrentParams{Infohash: hash(1000 + n), Name: "add"})
			record(err)
		}(i)
	}

	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("concurrent writes did not finish in 20s — likely a deadlock on the single connection")
	}

	for _, err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}
}
