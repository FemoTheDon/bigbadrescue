package rescue

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FemoTheDon/bigbadrescue/internal/convert"
	"github.com/FemoTheDon/bigbadrescue/internal/evernote"
	"github.com/FemoTheDon/bigbadrescue/internal/mock"
	"github.com/FemoTheDon/bigbadrescue/internal/pace"
	"github.com/FemoTheDon/bigbadrescue/internal/store"
)

type rig struct {
	fake *mock.Server
	ev   *evernote.Client
	st   *store.Store
	e    *Engine
	lim  *pace.Limiter
	dir  string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	fake := mock.New()
	t.Cleanup(fake.Close)
	if err := fake.Seed(ff, dir, os.Getenv("BIGBAD_TEST_AMR")); err != nil {
		t.Fatal(err)
	}
	lim := pace.New(time.Millisecond, nil)
	ev, err := evernote.New(evernote.Config{Endpoint: fake.URL, NoAuth: true, Limiter: lim})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	set := st.Settings()
	set.BackupDir = filepath.Join(dir, "Backups")
	st.SetSettings(set)
	e := New(ev, st, &convert.Converter{FFmpeg: ff}, filepath.Join(dir, "cache"), func(string, any) {})
	return &rig{fake, ev, st, e, lim, dir}
}

func (r *rig) run(t *testing.T, kind string, rows []Row) {
	t.Helper()
	var jobs []store.QueuedJob
	for _, row := range rows {
		jobs = append(jobs, store.QueuedJob{NoteID: row.NoteID, Title: row.Title})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	done := make(chan struct{})
	go func() { r.e.Run(ctx); close(done) }()
	r.e.Enqueue(kind, jobs)
	for r.e.Busy() {
		if ctx.Err() != nil {
			t.Fatal("jobs didn't finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestSearchByDateAndType(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	rows, err := r.e.Search(ctx, Filter{From: "2014-01-01", To: "2014-12-31", Formats: []string{"amr", "ogg"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Title != "Choir rehearsal ideas" || rows[1].Title != "Bridge melody hum" {
		t.Fatalf("2014 rows = %+v", rows)
	}
	rows, _ = r.e.Search(ctx, Filter{Formats: []string{"3gpp"}})
	if len(rows) != 1 || rows[0].Title != "Sermon notes, Sunday" {
		t.Fatalf("3gpp rows = %+v", rows)
	}
	// The end date is inclusive.
	rows, _ = r.e.Search(ctx, Filter{From: "2015-05-18", To: "2015-05-18"})
	if len(rows) != 1 {
		t.Fatalf("one-day range = %+v", rows)
	}
	// WAV is left alone.
	rows, _ = r.e.Search(ctx, Filter{From: "2016-01-01"})
	if len(rows) != 0 {
		t.Fatalf("2016 rows = %+v", rows)
	}
	counts, err := r.e.Scan(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var seenWav bool
	for _, c := range counts {
		seenWav = seenWav || strings.HasPrefix(c.Format, "WAV")
	}
	if !seenWav || len(counts) < 3 {
		t.Fatalf("scan = %+v", counts)
	}
}

func TestConvertReviewCleanup(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	rows, err := r.e.Search(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d", len(rows))
	}
	r.run(t, "convert", rows)

	for _, row := range rows {
		items := r.st.ForNote(row.NoteID)
		if len(items) == 0 {
			t.Fatalf("%s: nothing recorded", row.Title)
		}
		for _, it := range items {
			if it.State != store.Verified {
				t.Fatalf("%s/%s: state %s (%s)", row.Title, it.SrcName, it.State, it.Error)
			}
			if _, err := os.Stat(it.Backup); err != nil {
				t.Fatalf("no backup: %v", err)
			}
			if !strings.Contains(it.Backup, filepath.Join("Backups", row.Created[:4])) {
				t.Fatalf("backup path %s", it.Backup)
			}
		}
		n, files := r.fake.Note(row.NoteID)
		var mp3s, originals int
		for _, f := range files {
			if f.Mime == "audio/mpeg" {
				mp3s++
			} else {
				originals++
			}
		}
		if mp3s != len(items) || originals != len(items) {
			t.Fatalf("%s: %d MP3s, %d originals after convert\n%s", row.Title, mp3s, originals, n.Content)
		}
		if len(n.Tags) != 1 {
			t.Fatalf("%s: tags %v", row.Title, n.Tags)
		}
		// The MP3 sits right after its original.
		it := items[0]
		src := strings.Index(n.Content, it.SrcHash)
		mp3 := strings.Index(n.Content, it.MP3Hash)
		if src < 0 || mp3 < src || mp3-src > 200 {
			t.Fatalf("%s: MP3 not placed after the original:\n%s", row.Title, n.Content)
		}
	}
	if NoteStatus(r.st.ForNote(rows[0].NoteID)) != "converted" {
		t.Fatal("status not converted")
	}

	// Converting again does nothing: no uploads, no edits.
	before := r.fake.Calls.Load()
	r.run(t, "convert", rows)
	if extra := r.fake.Calls.Load() - before; extra != int64(len(rows)) {
		t.Fatalf("second convert made %d calls, want %d (one read per note)", extra, len(rows))
	}

	// Cleanup only touches reviewed items.
	_, filesBefore := r.fake.Note(rows[0].NoteID)
	r.run(t, "cleanup", rows)
	if _, files := r.fake.Note(rows[0].NoteID); len(files) != len(filesBefore) {
		t.Fatalf("cleanup ran before review: %d files", len(files))
	}
	var keys []string
	for _, row := range rows {
		for _, it := range r.st.ForNote(row.NoteID) {
			keys = append(keys, it.Key)
		}
	}
	r.e.Review(keys[:len(keys)-1], true)
	r.e.Review(keys[len(keys)-1:], false)
	r.run(t, "cleanup", rows)
	for _, row := range rows {
		n, files := r.fake.Note(row.NoteID)
		for _, it := range r.st.ForNote(row.NoteID) {
			var hasSrc, hasMP3 bool
			for _, f := range files {
				hasSrc = hasSrc || f.Hash == it.SrcHash
				hasMP3 = hasMP3 || f.Hash == it.MP3Hash
			}
			if !hasMP3 {
				t.Fatalf("%s: MP3 missing after cleanup", row.Title)
			}
			if it.State == store.Rejected {
				if !hasSrc {
					t.Fatal("rejected original was removed")
				}
				continue
			}
			if it.State != store.Cleaned || hasSrc {
				t.Fatalf("%s: state %s, original still there %v (%s)\n%s", row.Title, it.State, hasSrc, it.Error, n.Content)
			}
			if _, err := os.Stat(it.Backup); err != nil {
				t.Fatal("backup deleted by cleanup")
			}
		}
	}
	if s := NoteStatus(r.st.ForNote(rows[0].NoteID)); s != "cleaned" {
		t.Fatalf("status %s", s)
	}
	if n, _ := r.fake.Note(rows[0].NoteID); len(n.Tags) != 2 {
		t.Fatalf("cleaned tag missing: %v", n.Tags)
	}
}

func TestPausesWhenRateLimited(t *testing.T) {
	r := newRig(t)
	r.fake.RateLimitEvery = 5
	r.fake.RetryAfter = 1
	rows, err := r.e.Search(context.Background(), Filter{Formats: []string{"3gpp"}})
	if err != nil {
		t.Fatal(err)
	}
	r.run(t, "convert", rows)
	for _, it := range r.st.ForNote(rows[0].NoteID) {
		if it.State != store.Verified {
			t.Fatalf("state %s: %s", it.State, it.Error)
		}
	}
	if p := r.lim.Status().Pauses; p == 0 {
		t.Fatal("never paused")
	}
}

// An old note whose format only changes when an edit really changes it:
// the MP3 must still end up in the note, next to the note's text.
func TestPlacesInOldNoteWithoutMigration(t *testing.T) {
	r := newRig(t)
	r.fake.NoopKeepsLegacy = true
	rows, err := r.e.Search(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	r.run(t, "convert", rows)
	for _, row := range rows {
		for _, it := range r.st.ForNote(row.NoteID) {
			if it.State != store.Verified {
				t.Fatalf("%s/%s: state %s (%s)", row.Title, it.SrcName, it.State, it.Error)
			}
			n, _ := r.fake.Note(row.NoteID)
			if !strings.Contains(n.Content, it.MP3Hash) {
				t.Fatalf("%s: MP3 not in the note\n%s", row.Title, n.Content)
			}
		}
	}
}
