// Package rescue runs BigBadRescue's jobs: converting a note's old audio to
// MP3 and, after review, removing the originals.
package rescue

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/FemoTheDon/bigbadrescue/internal/convert"
	"github.com/FemoTheDon/bigbadrescue/internal/evernote"
	"github.com/FemoTheDon/bigbadrescue/internal/store"
	"github.com/FemoTheDon/bigbadrescue/internal/verify"
)

// Tag names put on notes so progress is visible (and searchable) in Evernote.
const (
	TagConverted = "BigBadRescue: converted"
	TagCleaned   = "BigBadRescue: cleaned"
)

// Engine owns the job queue.
type Engine struct {
	ev     *evernote.Client
	st     *store.Store
	conv   *convert.Converter
	cache  string
	notify func(event string, v any)

	mu      sync.Mutex
	queue   []store.QueuedJob
	current *Running
	cancel  context.CancelFunc
	kick    chan struct{}
	tagIDs  map[string]string
	fileMu  sync.Mutex // guards preview conversions
}

// Running describes the job in progress.
type Running struct {
	Kind   string  `json:"kind"`
	NoteID string  `json:"noteId"`
	Title  string  `json:"title"`
	Step   string  `json:"step"`
	File   string  `json:"file,omitempty"`
	Frac   float64 `json:"frac"`
}

// New makes an engine. cache is a private folder for downloads and MP3s.
func New(ev *evernote.Client, st *store.Store, conv *convert.Converter, cache string, notify func(string, any)) *Engine {
	e := &Engine{ev: ev, st: st, conv: conv, cache: cache, notify: notify,
		kick: make(chan struct{}, 1), tagIDs: map[string]string{}, queue: st.Queue()}
	for _, d := range []string{"orig", "mp3", "preview", "check"} {
		_ = os.MkdirAll(filepath.Join(cache, d), 0o755)
	}
	return e
}

// Run processes the queue until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	for {
		e.mu.Lock()
		var job *store.QueuedJob
		if len(e.queue) > 0 {
			j := e.queue[0]
			job = &j
		}
		e.mu.Unlock()
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-e.kick:
				continue
			}
		}
		jctx, cancel := context.WithCancel(ctx)
		e.mu.Lock()
		e.current = &Running{Kind: job.Kind, NoteID: job.NoteID, Title: job.Title, Step: "Starting"}
		e.cancel = cancel
		e.mu.Unlock()
		e.sendQueue()
		var err error
		switch job.Kind {
		case "convert":
			err = e.convertNote(jctx, job.NoteID)
		case "cleanup":
			err = e.cleanupNote(jctx, job.NoteID)
		}
		cancel()
		if ctx.Err() != nil {
			return // quitting: the job stays queued and resumes next time
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("%s %q: %v", job.Kind, job.Title, err)
			e.notify("toast", map[string]string{"kind": "error", "text": fmt.Sprintf("%s: %v", job.Title, err)})
		}
		e.mu.Lock()
		if len(e.queue) > 0 && e.queue[0] == *job {
			e.queue = e.queue[1:]
		}
		e.current, e.cancel = nil, nil
		q := append([]store.QueuedJob(nil), e.queue...)
		e.mu.Unlock()
		e.st.SetQueue(q)
		e.sendQueue()
		e.sendNote(job.NoteID)
	}
}

// Enqueue adds jobs, skipping notes already queued for the same kind.
func (e *Engine) Enqueue(kind string, notes []store.QueuedJob) int {
	e.mu.Lock()
	n := 0
	for _, j := range notes {
		j.Kind = kind
		dup := false
		for _, q := range e.queue {
			if q.Kind == kind && q.NoteID == j.NoteID {
				dup = true
				break
			}
		}
		if !dup {
			e.queue = append(e.queue, j)
			n++
		}
	}
	q := append([]store.QueuedJob(nil), e.queue...)
	e.mu.Unlock()
	e.st.SetQueue(q)
	select {
	case e.kick <- struct{}{}:
	default:
	}
	e.sendQueue()
	return n
}

// Stop cancels the running job and empties the queue. Everything done so
// far is saved, so the same notes can simply be converted again later.
func (e *Engine) Stop() {
	e.mu.Lock()
	e.queue = nil
	if e.cancel != nil {
		e.cancel()
	}
	e.mu.Unlock()
	e.st.SetQueue(nil)
	e.sendQueue()
}

// Busy reports whether a job is running or waiting.
func (e *Engine) Busy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current != nil || len(e.queue) > 0
}

// QueueState is sent to the window.
type QueueState struct {
	Running *Running          `json:"running"`
	Waiting []store.QueuedJob `json:"waiting"`
}

// Queue returns the current queue.
func (e *Engine) Queue() QueueState {
	e.mu.Lock()
	defer e.mu.Unlock()
	qs := QueueState{Waiting: []store.QueuedJob{}}
	if e.current != nil {
		c := *e.current
		qs.Running = &c
	}
	for i, j := range e.queue {
		if i == 0 && e.current != nil && j.NoteID == e.current.NoteID && j.Kind == e.current.Kind {
			continue
		}
		qs.Waiting = append(qs.Waiting, j)
	}
	return qs
}

func (e *Engine) sendQueue() { e.notify("queue", e.Queue()) }

func (e *Engine) sendNote(noteID string) {
	e.notify("note", map[string]any{"noteId": noteID, "status": NoteStatus(e.st.ForNote(noteID)), "items": e.st.ForNote(noteID)})
}

func (e *Engine) step(step, file string, frac float64) {
	e.mu.Lock()
	if e.current != nil {
		e.current.Step, e.current.File, e.current.Frac = step, file, frac
	}
	e.mu.Unlock()
	e.sendQueue()
}

func (e *Engine) save(it *store.Item) {
	_ = e.st.Put(it)
	e.notify("item", it)
}

func (e *Engine) fail(it *store.Item, err error) error {
	if it.State != store.Failed {
		it.LastGood = it.State
	}
	it.State = store.Failed
	it.Error = err.Error()
	e.save(it)
	return err
}

// convertNote runs the safe pipeline for every old-format attachment on a
// note: back up, convert, check, upload, place under the original, download
// back and compare, tag. Originals are never touched here.
func (e *Engine) convertNote(ctx context.Context, noteID string) error {
	e.step("Reading the note", "", 0)
	n, err := e.ev.GetNote(ctx, noteID)
	if err != nil {
		return err
	}
	set := e.st.Settings()
	preset := convert.PresetByName(set.Preset)
	var todo []evernote.Resource
	for _, r := range n.Resources {
		if _, ok := convert.FormatOfMIME(r.Mime); ok {
			todo = append(todo, r)
		}
	}
	if len(todo) == 0 {
		e.notify("toast", map[string]string{"kind": "info", "text": n.Title + ": no AMR, 3GPP or OGG attachments left on this note."})
		return nil
	}
	var firstErr error
	converted := 0
	for i, r := range todo {
		ft, _ := convert.FormatOfMIME(r.Mime)
		key := store.Key(n.ID, r.Hash)
		it := e.st.Item(key)
		if it != nil && it.State != store.Failed && it.State != store.Rejected && store.AtLeast(it.State, store.Verified) {
			continue // done in an earlier run
		}
		if it != nil && it.State == store.Rejected {
			continue // you said this one sounded wrong; leave it alone
		}
		mp3Name := convert.OutputName(fileName(r, ft, n))
		if it == nil {
			it = &store.Item{Key: key, NoteID: n.ID, NoteTitle: n.Title, NoteCreated: n.Created,
				SrcHash: r.Hash, SrcName: fileName(r, ft, n), SrcMime: r.Mime, SrcSize: r.SizeBytes,
				Format: ft.Label, MP3Name: mp3Name, Preset: preset.Name}
			// A note fixed by hand or by another copy of the app already has
			// the MP3: record it rather than adding a second one.
			for _, o := range n.Resources {
				if o.Mime == "audio/mpeg" && strings.EqualFold(o.Name, mp3Name) {
					it.State, it.MP3Hash, it.MP3Size = store.Verified, o.Hash, o.SizeBytes
					it.Error = ""
					e.save(it)
					break
				}
			}
			if it.State == store.Verified {
				continue
			}
		} else if it.State == store.Failed {
			it.State, it.Error = it.LastGood, ""
		}
		label := fmt.Sprintf("%s (%d of %d)", it.SrcName, i+1, len(todo))
		if err := e.convertOne(ctx, n, it, preset, label); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		converted++
	}
	if converted > 0 && set.TagNotes {
		e.step("Tagging the note", "", 1)
		e.tag(ctx, n.ID, TagConverted)
	}
	return firstErr
}

func (e *Engine) convertOne(ctx context.Context, n *evernote.Note, it *store.Item, preset convert.Preset, label string) error {
	set := e.st.Settings()
	// 1. Back up the original (download it if it isn't cached).
	if !store.AtLeast(it.State, store.BackedUp) || !exists(it.Backup) {
		e.step("Downloading the original", label, 0.05)
		orig, err := e.original(ctx, n.ID, it.SrcHash, it.SrcMime)
		if err != nil {
			return e.fail(it, err)
		}
		dest := backupPath(set.BackupDir, n, it.SrcName)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return e.fail(it, fmt.Errorf("couldn't make the backup folder: %w", err))
		}
		if got, _, _ := verify.MD5File(dest); got != it.SrcHash {
			dest = uniquePath(dest, it.SrcHash)
			if err := copyFile(orig, dest); err != nil {
				return e.fail(it, fmt.Errorf("couldn't back up the original: %w", err))
			}
		}
		if got, _, _ := verify.MD5File(dest); got != it.SrcHash {
			return e.fail(it, errors.New("the backup copy doesn't match the original"))
		}
		it.Backup, it.State = dest, store.BackedUp
		e.save(it)
	}
	// 2 and 3. Convert and check. Never redone once uploaded, because the
	// uploaded bytes are what the later steps refer to.
	if !store.AtLeast(it.State, store.Checked) {
		e.step("Converting", label, 0.2)
		mp3 := filepath.Join(e.cache, "mp3", it.SrcHash+".mp3")
		err := e.conv.Convert(ctx, it.Backup, mp3, preset, func(f float64) {
			if f >= 0 {
				e.step("Converting", label, 0.2+0.2*f)
			}
		})
		if err != nil {
			return e.fail(it, fmt.Errorf("conversion failed: %w", err))
		}
		it.MP3Path, it.Preset, it.State = mp3, preset.Name, store.Converted
		e.save(it)
		e.step("Checking the MP3 plays", label, 0.45)
		res, err := verify.Compare(ctx, e.conv.FFmpeg, it.Backup, mp3)
		if err != nil {
			return e.fail(it, err)
		}
		it.Check = &res
		if !res.OK() {
			return e.fail(it, errors.New("check failed: "+res.Problem))
		}
		hash, size, err := verify.MD5File(mp3)
		if err != nil {
			return e.fail(it, err)
		}
		it.MP3Hash, it.MP3Size, it.State = hash, size, store.Checked
		e.save(it)
	}
	// 4. Upload and register the MP3 on the note.
	if !store.AtLeast(it.State, store.Uploaded) {
		if !exists(it.MP3Path) {
			return e.fail(it, errors.New("the converted MP3 is missing from the cache; press Retry to convert again"))
		}
		e.step("Uploading the MP3", label, 0.55)
		tk, err := e.ev.StartUpload(ctx, n.ID, it.MP3Hash, it.MP3Size)
		if err != nil {
			return e.fail(it, err)
		}
		if !tk.AlreadyUploaded {
			if err := e.ev.Upload(ctx, tk.UploadURL, it.MP3Path, it.MP3Size); err != nil {
				return e.fail(it, err)
			}
		}
		e.step("Attaching the MP3", label, 0.7)
		fin, err := e.ev.Finalize(ctx, n.ID, it.MP3Hash, it.MP3Size, "audio/mpeg", it.MP3Name)
		if err != nil {
			return e.fail(it, err)
		}
		it.MP3Tag = fin.EnMediaTag
		if it.MP3Tag == "" {
			it.MP3Tag = `<en-media hash="` + it.MP3Hash + `" type="audio/mpeg" />`
		}
		it.State = store.Uploaded
		e.save(it)
	}
	// 5. Place the MP3 right under the original in the note text.
	if !store.AtLeast(it.State, store.Attached) {
		e.step("Placing the MP3 in the note", label, 0.8)
		if err := e.place(ctx, n.ID, it); err != nil {
			return e.fail(it, err)
		}
		it.State = store.Attached
		e.save(it)
	}
	// 6. Download the MP3 back from Evernote and compare.
	e.step("Checking Evernote's copy", label, 0.9)
	a, err := e.ev.GetAttachment(ctx, n.ID, it.MP3Hash)
	if err != nil {
		return e.fail(it, err)
	}
	back := filepath.Join(e.cache, "check", it.MP3Hash+".mp3")
	if err := e.ev.Download(ctx, a.DownloadURL, back, it.MP3Hash); err != nil {
		return e.fail(it, fmt.Errorf("Evernote's copy of the MP3 doesn't match: %w", err))
	}
	_ = os.Remove(back)
	it.State, it.Error = store.Verified, ""
	if set.TrustAutoCheck {
		it.State, it.ReviewedAt = store.Reviewed, time.Now()
	}
	e.save(it)
	return nil
}

// place puts the MP3's tag right after the original's tag, or at the end
// of the note if the original has no tag in the text.
//
// Evernote matches edit_note's "find" against a note in its new editor
// format. An old note is only moved to that format when an edit succeeds,
// so get_note still returns the old format and a "find" holding the
// original's tag misses. place then anchors on plain text next to the
// original, and as a last resort adds the MP3 at the end of the note.
func (e *Engine) place(ctx context.Context, noteID string, it *store.Item) error {
	n, err := e.ev.GetNote(ctx, noteID)
	if err != nil {
		return err
	}
	if _, err := evernote.MediaTag(n.Content, it.MP3Hash); err == nil {
		return nil // already placed (an earlier run got this far)
	}
	as, err := evernote.MediaAnchors(n.Content, it.SrcHash)
	if errors.Is(err, evernote.ErrNoTag) {
		return e.placed(ctx, noteID, it, e.ev.Append(ctx, noteID, it.MP3Tag))
	} else if err != nil {
		return err
	}
	a := as[0]
	err = e.ev.Replace(ctx, noteID, a.Find, a.Before+a.Tag+it.MP3Tag+a.After)
	if !errors.Is(err, evernote.ErrNotFound) {
		return e.placed(ctx, noteID, it, err)
	}
	after, before := evernote.TextAnchors(n.Content, it.SrcHash)
	// An edit that changes nothing still moves the note to the new format.
	// Then the original's tag can be found as get_note now shows it.
	if text := cmp.Or(after, before); text != "" {
		if err := e.ev.Replace(ctx, noteID, text, text); err == nil {
			if n2, err := e.ev.GetNote(ctx, noteID); err == nil && n2.Content != n.Content {
				if as, err := evernote.MediaAnchors(n2.Content, it.SrcHash); err == nil {
					a := as[0]
					err = e.ev.Replace(ctx, noteID, a.Find, a.Before+a.Tag+it.MP3Tag+a.After)
					if !errors.Is(err, evernote.ErrNotFound) {
						return e.placed(ctx, noteID, it, err)
					}
				}
				n = n2
				after, before = evernote.TextAnchors(n.Content, it.SrcHash)
			}
		}
	}
	if after != "" {
		err = e.ev.Replace(ctx, noteID, after, it.MP3Tag+after)
		if !errors.Is(err, evernote.ErrNotFound) {
			return e.placed(ctx, noteID, it, err)
		}
	}
	if before != "" {
		err = e.ev.Replace(ctx, noteID, before, before+it.MP3Tag)
		if !errors.Is(err, evernote.ErrNotFound) {
			return e.placed(ctx, noteID, it, err)
		}
	}
	return e.placed(ctx, noteID, it, e.ev.Append(ctx, noteID, it.MP3Tag))
}

// placed checks that an edit which reported success really put the MP3's
// tag in the note.
func (e *Engine) placed(ctx context.Context, noteID string, it *store.Item, err error) error {
	if err != nil {
		return err
	}
	n, err := e.ev.GetNote(ctx, noteID)
	if err != nil {
		return err
	}
	if _, err := evernote.MediaTag(n.Content, it.MP3Hash); err != nil {
		return errors.New("Evernote accepted the edit but the MP3 isn't in the note text; press Retry")
	}
	return nil
}

// cleanupNote removes originals whose MP3 has been reviewed (or passed the
// automatic check, if you chose to trust it).
func (e *Engine) cleanupNote(ctx context.Context, noteID string) error {
	set := e.st.Settings()
	var todo []*store.Item
	for _, it := range e.st.ForNote(noteID) {
		if it.State == store.Reviewed || (set.TrustAutoCheck && it.State == store.Verified) {
			todo = append(todo, it)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	e.step("Reading the note", "", 0)
	var firstErr error
	cleaned := 0
	for i, it := range todo {
		label := fmt.Sprintf("%s (%d of %d)", it.SrcName, i+1, len(todo))
		e.step("Removing the original", label, float64(i)/float64(len(todo)))
		if err := e.removeOriginal(ctx, it); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			it.Error = err.Error()
			e.save(it)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		cleaned++
	}
	if cleaned > 0 && set.TagNotes {
		all := true
		for _, it := range e.st.ForNote(noteID) {
			if it.State != store.Cleaned {
				all = false
			}
		}
		if all {
			e.tag(ctx, noteID, TagCleaned)
		}
	}
	return firstErr
}

func (e *Engine) removeOriginal(ctx context.Context, it *store.Item) error {
	if !exists(it.Backup) {
		return errors.New("the backup copy of the original is missing, so it was kept on the note")
	}
	// Each pass removes one tag of the original (a file can be shown more
	// than once). A miss is retried after re-reading the note.
	misses := 0
	for pass := 0; ; pass++ {
		n, err := e.ev.GetNote(ctx, it.NoteID)
		if err != nil {
			return err
		}
		if _, ok := n.Resource(it.MP3Hash); !ok {
			return errors.New("the MP3 is no longer on the note, so the original was kept")
		}
		as, err := evernote.MediaAnchors(n.Content, it.SrcHash)
		if errors.Is(err, evernote.ErrNoTag) {
			if _, ok := n.Resource(it.SrcHash); ok {
				if pass == 0 {
					return errors.New("the original isn't shown in the note text, so BigBadRescue can't remove it; delete it in Evernote")
				}
				return errors.New("the original was removed from the note text but Evernote still lists it")
			}
			break // gone
		} else if err != nil {
			return err
		}
		if pass >= 10 {
			return errors.New("the original kept reappearing in the note")
		}
		a := as[0]
		err = e.ev.Replace(ctx, it.NoteID, a.Find, a.Before+a.After)
		if errors.Is(err, evernote.ErrNotFound) && misses == 0 {
			misses++
			continue
		}
		if err != nil {
			return err
		}
	}
	it.State, it.Error, it.CleanedAt = store.Cleaned, "", time.Now()
	e.save(it)
	return nil
}

func (e *Engine) tag(ctx context.Context, noteID, name string) {
	e.mu.Lock()
	id := e.tagIDs[name]
	e.mu.Unlock()
	var err error
	if id == "" {
		if id, err = e.ev.EnsureTag(ctx, name); err != nil || id == "" {
			log.Printf("couldn't make the tag %q: %v", name, err)
			return
		}
		e.mu.Lock()
		e.tagIDs[name] = id
		e.mu.Unlock()
	}
	if err := e.ev.AddTags(ctx, []string{noteID}, []string{id}); err != nil {
		log.Printf("couldn't tag a note %q: %v", name, err)
	}
}

// Review marks items as sounding right (ok) or wrong.
func (e *Engine) Review(keys []string, ok bool) []*store.Item {
	var out []*store.Item
	for _, k := range keys {
		it, err := e.st.Update(k, func(it *store.Item) {
			switch {
			case ok && (it.State == store.Verified || it.State == store.Rejected):
				it.State, it.ReviewedAt, it.Error = store.Reviewed, time.Now(), ""
			case !ok && (it.State == store.Verified || it.State == store.Reviewed):
				it.LastGood, it.State, it.ReviewedAt = it.State, store.Rejected, time.Time{}
			}
		})
		if err == nil {
			out = append(out, it)
			e.notify("item", it)
		}
	}
	return out
}

// original returns a cached copy of an attachment, downloading it if needed.
func (e *Engine) original(ctx context.Context, noteID, hash, mime string) (string, error) {
	p := filepath.Join(e.cache, "orig", hash+extFor(mime))
	if got, _, _ := verify.MD5File(p); got == hash {
		return p, nil
	}
	a, err := e.ev.GetAttachment(ctx, noteID, hash)
	if err != nil {
		return "", err
	}
	if err := e.ev.Download(ctx, a.DownloadURL, p, hash); err != nil {
		return "", err
	}
	return p, nil
}

// Preview returns a file the browser can play for an attachment: the file
// itself if it already plays, otherwise a quick MP3 of it.
func (e *Engine) Preview(ctx context.Context, noteID, hash, mime string) (string, error) {
	var src string
	if it := e.st.Item(store.Key(noteID, hash)); it != nil && exists(it.Backup) {
		src = it.Backup
	} else if it := e.itemByMP3(noteID, hash); it != nil && exists(it.MP3Path) {
		return it.MP3Path, nil
	} else {
		var err error
		if src, err = e.original(ctx, noteID, hash, mime); err != nil {
			return "", err
		}
	}
	if playable(mime) {
		return src, nil
	}
	return e.previewOf(ctx, src, hash)
}

// previewOf converts a local file for playback.
func (e *Engine) previewOf(ctx context.Context, src, hash string) (string, error) {
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	out := filepath.Join(e.cache, "preview", hash+".mp3")
	if exists(out) {
		return out, nil
	}
	if err := e.conv.Convert(ctx, src, out, convert.Voice, nil); err != nil {
		return "", err
	}
	return out, nil
}

func (e *Engine) itemByMP3(noteID, hash string) *store.Item {
	for _, it := range e.st.ForNote(noteID) {
		if it.MP3Hash == hash {
			return it
		}
	}
	return nil
}

// ReviewFile returns a playable file for an item: the original (as a
// preview MP3) or the new MP3.
func (e *Engine) ReviewFile(ctx context.Context, key, which string) (string, error) {
	it := e.st.Item(key)
	if it == nil {
		return "", errors.New("unknown attachment")
	}
	if which == "mp3" {
		if exists(it.MP3Path) {
			return it.MP3Path, nil
		}
		return e.Preview(ctx, it.NoteID, it.MP3Hash, "audio/mpeg")
	}
	if !exists(it.Backup) {
		return "", errors.New("the backup copy is missing")
	}
	return e.previewOf(ctx, it.Backup, it.SrcHash)
}

func fileName(r evernote.Resource, ft convert.Format, n *evernote.Note) string {
	if r.Name != "" {
		return r.Name
	}
	return "Audio " + r.Hash[:8] + extFor(r.Mime)
}

func extFor(mime string) string {
	switch {
	case strings.Contains(mime, "amr"):
		return ".amr"
	case strings.Contains(mime, "3gpp2"):
		return ".3g2"
	case strings.Contains(mime, "3gpp"):
		return ".3gp"
	case strings.Contains(mime, "ogg"), strings.Contains(mime, "opus"):
		return ".ogg"
	case strings.Contains(mime, "webm"):
		return ".webm"
	case strings.Contains(mime, "mpeg"), strings.Contains(mime, "mp3"):
		return ".mp3"
	case strings.Contains(mime, "mp4"), strings.Contains(mime, "m4a"):
		return ".m4a"
	case strings.Contains(mime, "wav"):
		return ".wav"
	}
	return ".bin"
}

var unsafeRE = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]+`)

func safeName(s string) string {
	s = strings.TrimSpace(unsafeRE.ReplaceAllString(s, " "))
	s = strings.Trim(s, ". ")
	if len([]rune(s)) > 80 {
		s = string([]rune(s)[:80])
	}
	if s == "" {
		s = "Untitled"
	}
	return s
}

// backupPath is <backups>/<year>/<YYYY-MM-DD> <title>/<file>.
func backupPath(dir string, n *evernote.Note, name string) string {
	year, day := "Unknown date", "Unknown date"
	if t, err := time.Parse(time.RFC3339, n.Created); err == nil {
		year, day = t.Format("2006"), t.Format("2006-01-02")
	}
	return filepath.Join(dir, year, day+" "+safeName(n.Title), safeName(name))
}

// uniquePath avoids overwriting a different file with the same name.
func uniquePath(p, hash string) string {
	if !exists(p) {
		return p
	}
	ext := filepath.Ext(p)
	return strings.TrimSuffix(p, ext) + " (" + hash[:8] + ")" + ext
}

func exists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
