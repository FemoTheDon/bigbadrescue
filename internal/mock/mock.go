// Package mock is a stand-in for Evernote's MCP server and its file
// storage, used by the tests and by `bigbadrescue -demo`. It copies the
// real server's answers and quirks as observed on 23 Oct 2026, including
// an old note being moved to the new editor format on its first edit.
package mock

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Note is a note in the fake account.
type Note struct {
	ID         string
	Title      string
	NotebookID string
	Created    time.Time
	Updated    time.Time
	Content    string
	Tags       []string // tag ids
	Legacy     bool     // not yet moved to the new editor format
	USN        int64
}

// File is an attachment's bytes and details.
type File struct {
	Hash string
	Mime string
	Name string
	Data []byte
}

// Server is the fake Evernote.
type Server struct {
	mu        sync.Mutex
	notes     map[string]*Note
	files     map[string]*File            // by hash: uploaded and seeded bytes
	resources map[string]map[string]*File // note id -> hash -> registered resource
	tags      map[string]string           // id -> name
	notebooks map[string]string
	uploads   map[string][]byte // finished uploads by hash
	nextID    int
	usn       int64

	// RateLimitEvery makes every Nth MCP request fail with HTTP 429.
	RateLimitEvery int64
	RetryAfter     int
	// NoopKeepsLegacy makes an edit that changes nothing leave an old note
	// in its old format (the real server may or may not save it).
	NoopKeepsLegacy bool
	reqs            atomic.Int64
	Calls           atomic.Int64 // tool calls served

	ts *httptest.Server
	// URL is the MCP endpoint; Base is the server root.
	URL, Base string
}

// New starts a fake Evernote on a local port.
func New() *Server {
	s := &Server{notes: map[string]*Note{}, files: map[string]*File{}, resources: map[string]map[string]*File{},
		tags: map[string]string{}, notebooks: map[string]string{}, uploads: map[string][]byte{}, usn: 1000, RetryAfter: 1}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-evernote", Version: "1"}, nil)
	s.register(srv)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && s.RateLimitEvery > 0 && s.reqs.Add(1)%s.RateLimitEvery == 0 {
			w.Header().Set("Retry-After", strconv.Itoa(s.RetryAfter))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		h.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /files/{hash}", s.serveFile)
	mux.HandleFunc("POST /upload/{hash}", s.startSession)
	mux.HandleFunc("PUT /session/{hash}", s.putBytes)
	s.ts = httptest.NewServer(mux)
	s.Base = s.ts.URL
	s.URL = s.ts.URL + "/mcp"
	return s
}

// Close stops the server.
func (s *Server) Close() { s.ts.Close() }

func (s *Server) id() string {
	s.nextID++
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", s.nextID, s.nextID)
}

// AddNotebook adds a notebook and returns its id.
func (s *Server) AddNotebook(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.id()
	s.notebooks[id] = name
	return id
}

// AddNote adds a note in the old (legacy) format with the given text and
// attachments, each shown as <div><en-media …/></div> before the text.
func (s *Server) AddNote(title, notebookID string, created time.Time, text string, files ...File) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.id()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE en-note SYSTEM "http://xml.evernote.com/pub/enml2.dtd"><en-note style="word-wrap: break-word;">`)
	s.resources[id] = map[string]*File{}
	for _, f := range files {
		f := f
		sum := md5.Sum(f.Data)
		f.Hash = hex.EncodeToString(sum[:])
		s.files[f.Hash] = &f
		s.resources[id][f.Hash] = &f
		fmt.Fprintf(&b, `<div><en-media type="%s" hash="%s"/></div>`, f.Mime, f.Hash)
	}
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(&b, "<div>%s</div>", escape(line))
	}
	b.WriteString("</en-note>")
	s.usn++
	s.notes[id] = &Note{ID: id, Title: title, NotebookID: notebookID, Created: created, Updated: created, Content: b.String(), Legacy: true, USN: s.usn}
	return id
}

// Note returns a copy of a note and its visible resources.
func (s *Server) Note(id string) (Note, []File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := *s.notes[id]
	return n, s.visible(id)
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// visible lists a note's resources that its text refers to, as the real
// server does: a finalized attachment appears once its tag is in the note.
func (s *Server) visible(id string) []File {
	n := s.notes[id]
	var out []File
	for h, f := range s.resources[id] {
		if strings.Contains(n.Content, `hash="`+h+`"`) {
			out = append(out, *f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var wrappedMedia = regexp.MustCompile(`<div>(<en-media [^>]*?)\s*/></div>`)

// migrate copies what Evernote's editor does to an old note on first edit.
func migrate(n *Note) {
	n.Content = strings.Replace(n.Content, `<?xml version="1.0" encoding="UTF-8"?>`, "", 1)
	n.Content = wrappedMedia.ReplaceAllStringFunc(n.Content, func(m string) string {
		inner := wrappedMedia.FindStringSubmatch(m)[1]
		return `<en-media style="--en-transcription:&quot;{\&quot;state\&quot;:\&quot;ready\&quot;}&quot;;"` + strings.TrimPrefix(inner, "<en-media") + ` />`
	})
	n.Legacy = false
}

type args map[string]any

func (a args) str(k string) string {
	v, _ := a[k].(string)
	return v
}

func (a args) num(k string, def int) int {
	if v, ok := a[k].(float64); ok {
		return int(v)
	}
	return def
}

func (s *Server) register(srv *mcp.Server) {
	add := func(name string, h func(a args) (any, error)) {
		srv.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				s.Calls.Add(1)
				var a args
				_ = json.Unmarshal(req.Params.Arguments, &a)
				s.mu.Lock()
				out, err := h(a)
				s.mu.Unlock()
				if err != nil {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
				}
				b, _ := json.Marshal(out)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
			})
	}
	add("search_notes", s.searchNotes)
	add("get_note", s.getNote)
	add("get_attachment", s.getAttachment)
	add("start_attachment_upload", s.startUpload)
	add("finalize_attachment", s.finalize)
	add("edit_note", s.editNote)
	add("search_tags", s.searchTags)
	add("create_tag", s.createTag)
	add("update_note_tags", s.updateTags)
	add("search_notebooks", s.searchNotebooks)
}

var termRE = regexp.MustCompile(`(-?)(created|updated|resource|nbGuid):("[^"]*"|\S+)`)

func (s *Server) searchNotes(a args) (any, error) {
	q := a.str("query")
	start, max := a.num("startIndex", 0), a.num("maxResults", 10)
	var hits []*Note
	for _, n := range s.notes {
		ok := true
		for _, m := range termRE.FindAllStringSubmatch(q, -1) {
			neg, field, val := m[1] == "-", m[2], strings.Trim(m[3], `"`)
			var match bool
			switch field {
			case "created", "updated":
				t, err := time.Parse("20060102", val)
				if err != nil {
					return nil, fmt.Errorf("bad date %q", val)
				}
				when := n.Created
				if field == "updated" {
					when = n.Updated
				}
				match = !when.Before(t)
			case "resource":
				for _, f := range s.visible(n.ID) {
					if f.Mime == val || (strings.HasSuffix(val, "/*") && strings.HasPrefix(f.Mime, strings.TrimSuffix(val, "*"))) {
						match = true
					}
				}
			case "nbGuid":
				match = n.NotebookID == val
			}
			if match == neg {
				ok = false
			}
		}
		if ok {
			hits = append(hits, n)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Created.Before(hits[j].Created) })
	total := len(hits)
	if start > total {
		start = total
	}
	end := min(total, start+max)
	out := []map[string]any{}
	for _, n := range hits[start:end] {
		out = append(out, map[string]any{"noteId": n.ID, "title": n.Title, "snippet": snippet(n.Content),
			"createdAt": n.Created.UTC().Format("2006-01-02T15:04:05.000Z"), "updatedAt": n.Updated.UTC().Format("2006-01-02T15:04:05.000Z"), "score": 0})
	}
	return map[string]any{"hits": out, "totalResultCount": total, "startIndex": start, "isLastPage": end >= total}, nil
}

var tagStrip = regexp.MustCompile(`<[^>]+>`)

func snippet(enml string) string {
	t := strings.Join(strings.Fields(tagStrip.ReplaceAllString(enml, " ")), " ")
	if len(t) > 200 {
		t = t[:200]
	}
	return t
}

func (s *Server) note(a args) (*Note, error) {
	n, ok := s.notes[a.str("noteId")]
	if !ok {
		return nil, fmt.Errorf("note %s not found", a.str("noteId"))
	}
	return n, nil
}

func (s *Server) getNote(a args) (any, error) {
	n, err := s.note(a)
	if err != nil {
		return nil, err
	}
	res := []map[string]any{}
	for _, f := range s.visible(n.ID) {
		res = append(res, map[string]any{"id": "r-" + f.Hash[:8], "hash": f.Hash, "mime": f.Mime, "name": f.Name, "sizeBytes": len(f.Data)})
	}
	tags := []map[string]any{}
	for _, t := range n.Tags {
		tags = append(tags, map[string]any{"id": t, "name": s.tags[t]})
	}
	return map[string]any{"id": n.ID, "title": n.Title, "version": n.USN, "updateSequenceNumber": n.USN, "notebookId": n.NotebookID,
		"content": n.Content, "created": n.Created.UTC().Format(time.RFC3339), "updated": n.Updated.UTC().Format(time.RFC3339),
		"active": true, "tags": tags, "resources": res}, nil
}

func (s *Server) getAttachment(a args) (any, error) {
	n, err := s.note(a)
	if err != nil {
		return nil, err
	}
	f, ok := s.resources[n.ID][a.str("hash")]
	if !ok {
		return nil, fmt.Errorf("attachment not found")
	}
	return map[string]any{"downloadUrl": s.Base + "/files/" + f.Hash, "expiresAt": time.Now().Add(5 * time.Minute).UnixMilli(),
		"noteId": n.ID, "hash": f.Hash, "mimeType": f.Mime, "filename": f.Name, "sizeBytes": len(f.Data)}, nil
}

func (s *Server) startUpload(a args) (any, error) {
	if _, err := s.note(a); err != nil {
		return nil, err
	}
	h := a.str("hash")
	_, done := s.uploads[h]
	return map[string]any{"noteId": a.str("noteId"), "hash": h, "sizeBytes": a.num("sizeBytes", 0), "alreadyUploaded": done,
		"uploadUrl": fmt.Sprintf("%s/upload/%s?size=%d", s.Base, h, a.num("sizeBytes", 0))}, nil
}

func (s *Server) finalize(a args) (any, error) {
	n, err := s.note(a)
	if err != nil {
		return nil, err
	}
	h := a.str("hash")
	data, ok := s.uploads[h]
	if !ok {
		return nil, fmt.Errorf("no uploaded bytes for %s", h)
	}
	if len(data) != a.num("sizeBytes", -1) {
		return nil, fmt.Errorf("size mismatch")
	}
	f := &File{Hash: h, Mime: a.str("mimeType"), Name: a.str("filename"), Data: data}
	s.files[h] = f
	s.resources[n.ID][h] = f
	tag := fmt.Sprintf(`<en-media hash="%s" type="%s" />`, h, f.Mime)
	return map[string]any{"noteId": n.ID, "resourceGuid": "r-" + h[:8], "hash": h, "sizeBytes": len(data), "mimeType": f.Mime, "enMediaTag": tag, "filename": f.Name}, nil
}

func (s *Server) editNote(a args) (any, error) {
	n, err := s.note(a)
	if err != nil {
		return nil, err
	}
	// Evernote matches against the note in its new editor format, and only
	// saves that format when the edit succeeds.
	work := *n
	if work.Legacy {
		migrate(&work)
	}
	content := a.str("content")
	switch a.str("mode") {
	case "append":
		work.Content = strings.Replace(work.Content, "</en-note>", content+"</en-note>", 1)
	case "replace":
		find := a.str("find")
		switch c := strings.Count(work.Content, find); {
		case c == 0:
			return nil, fmt.Errorf("`find` did not match the note body. Copy the exact ENML substring from get_note byte-for-byte")
		case c > 1:
			return nil, fmt.Errorf("`find` matched %d places; make it unique", c)
		}
		if find == content && s.NoopKeepsLegacy {
			s.touch(n)
			return map[string]any{"noteId": n.ID, "updated": []string{}, "mode": "replace", "didMutate": false}, nil
		}
		work.Content = strings.Replace(work.Content, find, content, 1)
	default:
		return nil, fmt.Errorf("unknown mode")
	}
	*n = work
	s.touch(n)
	return map[string]any{"noteId": n.ID, "updated": []string{"body"}, "mode": a.str("mode"), "didMutate": true}, nil
}

func (s *Server) touch(n *Note) {
	s.usn++
	n.USN = s.usn
	n.Updated = time.Now()
}

func (s *Server) searchTags(a args) (any, error) {
	q := strings.ToLower(a.str("query"))
	hits := []map[string]any{}
	for id, name := range s.tags {
		if strings.Contains(strings.ToLower(name), q) {
			hits = append(hits, map[string]any{"tagId": id, "label": name, "searchFilter": `tagGuid:"` + id + `"`})
		}
	}
	return map[string]any{"hits": hits, "totalResultCount": len(hits), "isLastPage": true}, nil
}

func (s *Server) createTag(a args) (any, error) {
	id := s.id()
	s.tags[id] = a.str("name")
	return map[string]any{"tagId": id, "name": a.str("name")}, nil
}

func (s *Server) updateTags(a args) (any, error) {
	ids, _ := a["noteIds"].([]any)
	add, _ := a["tagIdsToAdd"].([]any)
	for _, id := range ids {
		n, ok := s.notes[id.(string)]
		if !ok {
			continue
		}
		for _, t := range add {
			ts := t.(string)
			has := false
			for _, x := range n.Tags {
				has = has || x == ts
			}
			if !has {
				n.Tags = append(n.Tags, ts)
			}
		}
	}
	return map[string]any{"results": []any{}}, nil
}

func (s *Server) searchNotebooks(a args) (any, error) {
	hits := []map[string]any{}
	for id, name := range s.notebooks {
		hits = append(hits, map[string]any{"notebookId": id, "label": name, "searchFilter": `nbGuid:"` + id + `"`})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i]["label"].(string) < hits[j]["label"].(string) })
	return map[string]any{"hits": hits, "totalResultCount": len(hits), "isLastPage": true}, nil
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	f, ok := s.files[r.PathValue("hash")]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", f.Mime)
	w.Write(f.Data)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-goog-resumable") != "start" || r.Header.Get("x-upload-content-length") != r.URL.Query().Get("size") {
		http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("%s/session/%s?size=%s", s.Base, r.PathValue("hash"), r.URL.Query().Get("size")))
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) putBytes(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	if strconv.Itoa(len(b)) != r.URL.Query().Get("size") {
		http.Error(w, "wrong length", http.StatusBadRequest)
		return
	}
	sum := md5.Sum(b)
	if hex.EncodeToString(sum[:]) != r.PathValue("hash") {
		// Real storage doesn't check, which is why the app must hash right.
		http.Error(w, "hash mismatch (test server only)", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.uploads[r.PathValue("hash")] = b
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}
