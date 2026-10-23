package evernote

// These mirror the JSON the Evernote MCP server returns (checked against a
// real account on 23 Oct 2026).

// Hit is one note in a search result.
type Hit struct {
	NoteID    string `json:"noteId"`
	Title     string `json:"title"`
	Snippet   string `json:"snippet"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// SearchResult is one page of search_notes.
type SearchResult struct {
	Hits             []Hit `json:"hits"`
	TotalResultCount int   `json:"totalResultCount"`
	StartIndex       int   `json:"startIndex"`
	IsLastPage       bool  `json:"isLastPage"`
}

// Tag is a tag on a note, or one found by search_tags.
type Tag struct {
	ID    string `json:"id"`
	TagID string `json:"tagId"`
	Name  string `json:"name"`
	Label string `json:"label"`
}

// GUID returns whichever id field the server filled in.
func (t Tag) GUID() string {
	if t.TagID != "" {
		return t.TagID
	}
	return t.ID
}

// Title returns whichever name field the server filled in.
func (t Tag) Title() string {
	if t.Label != "" {
		return t.Label
	}
	return t.Name
}

// Resource is one attachment in a note's manifest.
type Resource struct {
	ID        string `json:"id"`
	Hash      string `json:"hash"`
	Mime      string `json:"mime"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Note is what get_note returns.
type Note struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Version    int64      `json:"version"`
	USN        int64      `json:"updateSequenceNumber"`
	NotebookID string     `json:"notebookId"`
	Content    string     `json:"content"`
	Created    string     `json:"created"`
	Updated    string     `json:"updated"`
	Active     bool       `json:"active"`
	Tags       []Tag      `json:"tags"`
	Resources  []Resource `json:"resources"`
}

// Resource returns the attachment with the given hash.
func (n *Note) Resource(hash string) (Resource, bool) {
	for _, r := range n.Resources {
		if r.Hash == hash {
			return r, true
		}
	}
	return Resource{}, false
}

// Attachment is a signed, short-lived download link from get_attachment.
type Attachment struct {
	DownloadURL  string `json:"downloadUrl"`
	ExpiresAt    int64  `json:"expiresAt"`
	ResourceGUID string `json:"resourceGuid"`
	Hash         string `json:"hash"`
	MimeType     string `json:"mimeType"`
	Filename     string `json:"filename"`
	SizeBytes    int64  `json:"sizeBytes"`
}

// UploadTicket is what start_attachment_upload returns.
type UploadTicket struct {
	AlreadyUploaded bool   `json:"alreadyUploaded"`
	UploadURL       string `json:"uploadUrl"`
}

// Finalized is what finalize_attachment returns.
type Finalized struct {
	ResourceGUID string `json:"resourceGuid"`
	Hash         string `json:"hash"`
	MimeType     string `json:"mimeType"`
	EnMediaTag   string `json:"enMediaTag"`
	Filename     string `json:"filename"`
}

// Notebook is one hit from search_notebooks.
type Notebook struct {
	NotebookID string `json:"notebookId"`
	Label      string `json:"label"`
	Stack      string `json:"stack"`
}
