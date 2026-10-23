// Package store is BigBadRescue's memory: which attachments have been
// backed up, converted, attached, reviewed and cleaned up, plus the
// settings. It is a small JSON file written atomically after every change.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/FemoTheDon/bigbadrescue/internal/verify"
)

// States an attachment moves through, in order.
const (
	BackedUp  = "backed-up" // original saved to the backups folder
	Converted = "converted" // MP3 made locally
	Checked   = "checked"   // MP3 passed the automatic check
	Uploaded  = "uploaded"  // MP3 bytes are in Evernote and registered on the note
	Attached  = "attached"  // MP3 tag placed in the note text
	Verified  = "verified"  // MP3 downloaded back from Evernote and matched
	Reviewed  = "reviewed"  // you listened and said it sounds right
	Cleaned   = "cleaned"   // original removed from the note
	Failed    = "failed"    // stopped; see Error
	Rejected  = "rejected"  // you said the MP3 doesn't sound right
)

var order = map[string]int{BackedUp: 1, Converted: 2, Checked: 3, Uploaded: 4, Attached: 5, Verified: 6, Reviewed: 7, Cleaned: 8}

// AtLeast reports whether state s has reached state want.
func AtLeast(s, want string) bool { return order[s] >= order[want] && order[s] > 0 }

// Item is one original attachment on one note.
type Item struct {
	Key         string         `json:"key"` // noteID/hash
	NoteID      string         `json:"noteId"`
	NoteTitle   string         `json:"noteTitle"`
	NoteCreated string         `json:"noteCreated"`
	SrcHash     string         `json:"srcHash"`
	SrcName     string         `json:"srcName"`
	SrcMime     string         `json:"srcMime"`
	SrcSize     int64          `json:"srcSize"`
	Format      string         `json:"format"`
	State       string         `json:"state"`
	LastGood    string         `json:"lastGood,omitempty"` // furthest state reached before a failure
	Error       string         `json:"error,omitempty"`
	Backup      string         `json:"backup,omitempty"`
	MP3Path     string         `json:"mp3Path,omitempty"`
	MP3Name     string         `json:"mp3Name,omitempty"`
	MP3Hash     string         `json:"mp3Hash,omitempty"`
	MP3Size     int64          `json:"mp3Size,omitempty"`
	MP3Tag      string         `json:"mp3Tag,omitempty"`
	Check       *verify.Result `json:"check,omitempty"`
	Preset      string         `json:"preset,omitempty"`
	Updated     time.Time      `json:"updated"`
	ReviewedAt  time.Time      `json:"reviewedAt,omitzero"`
	CleanedAt   time.Time      `json:"cleanedAt,omitzero"`
}

// Progress is how far the item got, ignoring a later failure.
func (it *Item) Progress() string {
	if it.State == Failed || it.State == Rejected {
		return it.LastGood
	}
	return it.State
}

// Settings are the choices in the Settings panel.
type Settings struct {
	BackupDir         string `json:"backupDir"`
	Preset            string `json:"preset"`     // "Voice" or "Music"
	IntervalMs        int    `json:"intervalMs"` // spacing between Evernote calls
	TrustAutoCheck    bool   `json:"trustAutoCheck"`
	TagNotes          bool   `json:"tagNotes"`
	ClientID          string `json:"clientId,omitempty"`
	ClientMetadataURL string `json:"clientMetadataUrl,omitempty"`
}

// Data is everything saved.
type Data struct {
	Items       map[string]*Item `json:"items"`
	PausedUntil time.Time        `json:"pausedUntil,omitzero"`
	PauseReason string           `json:"pauseReason,omitempty"`
	Settings    Settings         `json:"settings"`
	Queue       []QueuedJob      `json:"queue,omitempty"`
}

// QueuedJob is a job waiting to run, kept so a quit doesn't lose it.
type QueuedJob struct {
	Kind   string `json:"kind"` // "convert" or "cleanup"
	NoteID string `json:"noteId"`
	Title  string `json:"title"`
}

// Store guards Data and saves it.
type Store struct {
	mu   sync.Mutex
	path string
	d    Data
}

// DefaultSettings are used on first run.
func DefaultSettings() Settings {
	home, _ := os.UserHomeDir()
	return Settings{
		BackupDir:  filepath.Join(home, "Music", "BigBadRescue Backups"),
		Preset:     "Voice",
		IntervalMs: 1500,
		TagNotes:   true,
	}
}

// Open loads the store at path (created if missing).
func Open(path string) (*Store, error) {
	s := &Store{path: path, d: Data{Items: map[string]*Item{}, Settings: DefaultSettings()}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, os.MkdirAll(filepath.Dir(path), 0o755)
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.d); err != nil {
		// Keep the damaged file for inspection and start fresh.
		_ = os.Rename(path, path+".damaged-"+time.Now().Format("20060102-150405"))
		s.d = Data{Items: map[string]*Item{}, Settings: DefaultSettings()}
		return s, nil
	}
	if s.d.Items == nil {
		s.d.Items = map[string]*Item{}
	}
	if s.d.Settings.Preset == "" {
		s.d.Settings = DefaultSettings()
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(&s.d, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Key builds an item key.
func Key(noteID, hash string) string { return noteID + "/" + hash }

// Item returns a copy of an item, or nil.
func (s *Store) Item(key string) *Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	if it, ok := s.d.Items[key]; ok {
		c := *it
		return &c
	}
	return nil
}

// Put saves an item.
func (s *Store) Put(it *Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it.Updated = time.Now()
	c := *it
	s.d.Items[it.Key] = &c
	return s.saveLocked()
}

// Update changes an item in place and saves.
func (s *Store) Update(key string, f func(*Item)) (*Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.d.Items[key]
	if !ok {
		return nil, errors.New("unknown item " + key)
	}
	f(it)
	it.Updated = time.Now()
	c := *it
	return &c, s.saveLocked()
}

// ForNote returns copies of the items for one note.
func (s *Store) ForNote(noteID string) []*Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Item
	for _, it := range s.d.Items {
		if it.NoteID == noteID {
			c := *it
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SrcName < out[j].SrcName })
	return out
}

// All returns copies of every item, oldest note first.
func (s *Store) All() []*Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Item, 0, len(s.d.Items))
	for _, it := range s.d.Items {
		c := *it
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NoteCreated != out[j].NoteCreated {
			return out[i].NoteCreated < out[j].NoteCreated
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Settings returns the settings.
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Settings
}

// SetSettings saves new settings.
func (s *Store) SetSettings(st Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Settings = st
	return s.saveLocked()
}

// Pause returns the saved pause.
func (s *Store) Pause() (time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.PausedUntil, s.d.PauseReason
}

// SetPause saves the pause so a restart honours it.
func (s *Store) SetPause(until time.Time, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.PausedUntil.Equal(until) && s.d.PauseReason == reason {
		return
	}
	s.d.PausedUntil, s.d.PauseReason = until, reason
	_ = s.saveLocked()
}

// Queue returns the saved queue.
func (s *Store) Queue() []QueuedJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]QueuedJob(nil), s.d.Queue...)
}

// SetQueue saves the queue.
func (s *Store) SetQueue(q []QueuedJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Queue = append([]QueuedJob(nil), q...)
	_ = s.saveLocked()
}
