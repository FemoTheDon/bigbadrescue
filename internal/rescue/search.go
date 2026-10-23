package rescue

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/FemoTheDon/bigbadrescue/internal/convert"
	"github.com/FemoTheDon/bigbadrescue/internal/evernote"
	"github.com/FemoTheDon/bigbadrescue/internal/store"
)

// Filter is the search bar.
type Filter struct {
	From       string   `json:"from"`  // YYYY-MM-DD, inclusive ("" = no start)
	To         string   `json:"to"`    // YYYY-MM-DD, inclusive ("" = no end)
	Field      string   `json:"field"` // "created" or "updated"
	Formats    []string `json:"formats"`
	NotebookID string   `json:"notebookId"`
}

// MaxResults caps one search so a huge account can't stall the app.
const MaxResults = 3000

// Row is one note in the list.
type Row struct {
	NoteID  string   `json:"noteId"`
	Title   string   `json:"title"`
	Snippet string   `json:"snippet"`
	Created string   `json:"created"`
	Updated string   `json:"updated"`
	Formats []string `json:"formats"`
	Status  string   `json:"status"`
}

func (f Filter) base() (string, error) {
	field := "created"
	if f.Field == "updated" {
		field = "updated"
	}
	var parts []string
	if f.From != "" {
		t, err := time.Parse("2006-01-02", f.From)
		if err != nil {
			return "", fmt.Errorf("the start date %q isn't a date", f.From)
		}
		parts = append(parts, field+":"+t.Format("20060102"))
	}
	if f.To != "" {
		t, err := time.Parse("2006-01-02", f.To)
		if err != nil {
			return "", fmt.Errorf("the end date %q isn't a date", f.To)
		}
		// "created:X" means on or after X, so the end is "not on or after
		// the day after".
		parts = append(parts, "-"+field+":"+t.AddDate(0, 0, 1).Format("20060102"))
	}
	if f.NotebookID != "" {
		parts = append(parts, `nbGuid:"`+f.NotebookID+`"`)
	}
	return strings.Join(parts, " "), nil
}

func (f Filter) formats() []convert.Format {
	var out []convert.Format
	for _, k := range f.Formats {
		if ft, ok := convert.FormatByKey(k); ok {
			out = append(out, ft)
		}
	}
	if len(out) == 0 {
		out = convert.Formats
	}
	return out
}

// Search finds notes in the date range with attachments of the chosen
// formats. Evernote can't OR resource types inside a date-filtered query,
// so it runs one search per MIME type and merges them.
func (e *Engine) Search(ctx context.Context, f Filter) ([]Row, error) {
	base, err := f.base()
	if err != nil {
		return nil, err
	}
	rows := map[string]*Row{}
	for _, ft := range f.formats() {
		for _, mime := range ft.MIMEs {
			q := strings.TrimSpace(base + " resource:" + mime)
			for start := 0; start < MaxResults; {
				page, err := e.ev.SearchNotes(ctx, q, start, 100)
				if err != nil {
					return nil, err
				}
				for _, h := range page.Hits {
					r := rows[h.NoteID]
					if r == nil {
						r = &Row{NoteID: h.NoteID, Title: h.Title, Snippet: h.Snippet, Created: h.CreatedAt, Updated: h.UpdatedAt}
						rows[h.NoteID] = r
					}
					if !contains(r.Formats, ft.Label) {
						r.Formats = append(r.Formats, ft.Label)
					}
				}
				start += len(page.Hits)
				if page.IsLastPage || len(page.Hits) == 0 || start >= page.TotalResultCount {
					break
				}
			}
		}
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		r.Status = NoteStatus(e.st.ForNote(r.NoteID))
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].NoteID < out[j].NoteID
	})
	return out, nil
}

// TypeCount is one line of the Scan types report.
type TypeCount struct {
	Format string `json:"format"`
	MIME   string `json:"mime"`
	Notes  int    `json:"notes"`
}

// Scan counts notes per attachment type in the date range (any audio, so
// it also reports types BigBadRescue leaves alone, such as WAV and MP3).
func (e *Engine) Scan(ctx context.Context, f Filter) ([]TypeCount, error) {
	base, err := f.base()
	if err != nil {
		return nil, err
	}
	var out []TypeCount
	for _, ft := range convert.Formats {
		for _, mime := range ft.MIMEs {
			page, err := e.ev.SearchNotes(ctx, strings.TrimSpace(base+" resource:"+mime), 0, 1)
			if err != nil {
				return nil, err
			}
			if page.TotalResultCount > 0 {
				out = append(out, TypeCount{ft.Label, mime, page.TotalResultCount})
			}
		}
	}
	for _, m := range []struct{ label, mime string }{
		{"WAV (plays already)", "audio/wav"}, {"WAV (plays already)", "audio/x-wav"},
		{"MP3 (plays already)", "audio/mpeg"}, {"M4A (plays already)", "audio/mp4"}, {"M4A (plays already)", "audio/x-m4a"},
	} {
		page, err := e.ev.SearchNotes(ctx, strings.TrimSpace(base+" resource:"+m.mime), 0, 1)
		if err != nil {
			return nil, err
		}
		if page.TotalResultCount > 0 {
			out = append(out, TypeCount{m.label, m.mime, page.TotalResultCount})
		}
	}
	return out, nil
}

// NoteStatus sums up a note's items for its badge in the list.
func NoteStatus(items []*store.Item) string {
	if len(items) == 0 {
		return "todo"
	}
	all := func(want string) bool {
		for _, it := range items {
			if it.State == store.Failed || it.State == store.Rejected || !store.AtLeast(it.State, want) {
				return false
			}
		}
		return true
	}
	for _, it := range items {
		if it.State == store.Failed {
			return "failed"
		}
		if it.State == store.Rejected {
			return "rejected"
		}
	}
	switch {
	case all(store.Cleaned):
		return "cleaned"
	case all(store.Reviewed):
		return "reviewed"
	case all(store.Verified):
		return "converted"
	}
	return "partial"
}

// Detail is the note viewer's content.
type Detail struct {
	NoteID      string       `json:"noteId"`
	Title       string       `json:"title"`
	Created     string       `json:"created"`
	Updated     string       `json:"updated"`
	Text        string       `json:"text"`
	Tags        []string     `json:"tags"`
	Attachments []Attachment `json:"attachments"`
	Status      string       `json:"status"`
}

// Attachment is one file in the viewer.
type Attachment struct {
	Hash      string      `json:"hash"`
	Name      string      `json:"name"`
	Mime      string      `json:"mime"`
	Size      int64       `json:"size"`
	Format    string      `json:"format,omitempty"` // set when BigBadRescue converts this type
	Playable  bool        `json:"playable"`         // plays on phones as it is
	Audio     bool        `json:"audio"`
	Item      *store.Item `json:"item,omitempty"`
	Converted bool        `json:"converted"` // an MP3 made by BigBadRescue
}

// NoteDetail fetches one note for the viewer.
func (e *Engine) NoteDetail(ctx context.Context, id string) (*Detail, error) {
	n, err := e.ev.GetNote(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &Detail{NoteID: n.ID, Title: n.Title, Created: n.Created, Updated: n.Updated, Text: evernote.PlainText(n.Content)}
	for _, t := range n.Tags {
		d.Tags = append(d.Tags, t.Title())
	}
	items := e.st.ForNote(n.ID)
	mp3s := map[string]bool{}
	for _, it := range items {
		if it.MP3Hash != "" {
			mp3s[it.MP3Hash] = true
		}
	}
	for _, r := range n.Resources {
		a := Attachment{Hash: r.Hash, Name: r.Name, Mime: r.Mime, Size: r.SizeBytes,
			Audio:    strings.HasPrefix(r.Mime, "audio/") || strings.Contains(r.Mime, "ogg") || r.Mime == "video/3gpp",
			Playable: playable(r.Mime), Converted: mp3s[r.Hash]}
		if ft, ok := convert.FormatOfMIME(r.Mime); ok {
			a.Format = ft.Label
			a.Item = e.st.Item(store.Key(n.ID, r.Hash))
		}
		d.Attachments = append(d.Attachments, a)
	}
	d.Status = NoteStatus(items)
	return d, nil
}

func playable(mime string) bool {
	switch strings.ToLower(mime) {
	case "audio/mpeg", "audio/mp3", "audio/mp4", "audio/x-m4a", "audio/m4a", "audio/aac", "audio/wav", "audio/x-wav", "audio/wave":
		return true
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
