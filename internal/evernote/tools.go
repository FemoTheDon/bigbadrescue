package evernote

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// SearchNotes runs one page of an Evernote search-grammar query, oldest
// first.
func (c *Client) SearchNotes(ctx context.Context, query string, start, max int) (*SearchResult, error) {
	var r SearchResult
	err := c.call(ctx, "search_notes", map[string]any{
		"query": query, "startIndex": start, "maxResults": max,
		"sortBy": "created", "ascending": true,
	}, &r)
	return &r, err
}

// GetNote fetches a note with its text and attachment list.
func (c *Client) GetNote(ctx context.Context, id string) (*Note, error) {
	var n Note
	err := c.call(ctx, "get_note", map[string]any{"noteId": id}, &n)
	return &n, err
}

// GetAttachment returns a short-lived download link for an attachment.
func (c *Client) GetAttachment(ctx context.Context, noteID, hash string) (*Attachment, error) {
	var a Attachment
	err := c.call(ctx, "get_attachment", map[string]any{"noteId": noteID, "hash": hash}, &a)
	return &a, err
}

// StartUpload asks for an upload link for a file with this MD5 and size.
func (c *Client) StartUpload(ctx context.Context, noteID, hash string, size int64) (*UploadTicket, error) {
	var t UploadTicket
	err := c.call(ctx, "start_attachment_upload", map[string]any{"noteId": noteID, "hash": hash, "sizeBytes": size}, &t)
	return &t, err
}

// Finalize registers uploaded bytes as an attachment on the note.
func (c *Client) Finalize(ctx context.Context, noteID, hash string, size int64, mime, filename string) (*Finalized, error) {
	var f Finalized
	err := c.call(ctx, "finalize_attachment", map[string]any{
		"noteId": noteID, "hash": hash, "sizeBytes": size, "mimeType": mime, "filename": filename,
	}, &f)
	return &f, err
}

// ErrNotFound means an edit_note "find" anchor wasn't in the note.
var ErrNotFound = errors.New("the text to change was not found in the note")

// Replace swaps the single occurrence of find in the note body for content
// ("" deletes it).
func (c *Client) Replace(ctx context.Context, noteID, find, content string) error {
	err := c.call(ctx, "edit_note", map[string]any{"noteId": noteID, "mode": "replace", "find": find, "content": content}, nil)
	var te *ToolError
	if errors.As(err, &te) && strings.Contains(strings.ToLower(te.Msg), "did not match") {
		return ErrNotFound
	}
	return err
}

// Append adds content to the end of the note body.
func (c *Client) Append(ctx context.Context, noteID, content string) error {
	return c.call(ctx, "edit_note", map[string]any{"noteId": noteID, "mode": "append", "content": content}, nil)
}

// FindTag returns the id of the tag with exactly this name, or "".
func (c *Client) FindTag(ctx context.Context, name string) (string, error) {
	var r struct {
		Hits []Tag `json:"hits"`
	}
	if err := c.call(ctx, "search_tags", map[string]any{"query": name, "maxResults": 10}, &r); err != nil {
		return "", err
	}
	for _, t := range r.Hits {
		if strings.EqualFold(t.Title(), name) {
			return t.GUID(), nil
		}
	}
	return "", nil
}

// EnsureTag returns the id of the tag with this name, creating it if needed.
func (c *Client) EnsureTag(ctx context.Context, name string) (string, error) {
	id, err := c.FindTag(ctx, name)
	if err != nil || id != "" {
		return id, err
	}
	var r map[string]any
	if err := c.call(ctx, "create_tag", map[string]any{"name": name}, &r); err != nil {
		return "", err
	}
	for _, k := range []string{"tagId", "id", "guid"} {
		if s, ok := r[k].(string); ok && s != "" {
			return s, nil
		}
	}
	if t, ok := r["tag"].(map[string]any); ok {
		for _, k := range []string{"tagId", "id", "guid"} {
			if s, ok := t[k].(string); ok && s != "" {
				return s, nil
			}
		}
	}
	return c.FindTag(ctx, name)
}

// AddTags puts tags on notes (1–50 notes per call).
func (c *Client) AddTags(ctx context.Context, noteIDs, tagIDs []string) error {
	return c.call(ctx, "update_note_tags", map[string]any{"noteIds": noteIDs, "tagIdsToAdd": tagIDs}, nil)
}

// Notebooks lists the account's notebooks.
func (c *Client) Notebooks(ctx context.Context) ([]Notebook, error) {
	var r struct {
		Hits []Notebook `json:"hits"`
	}
	err := c.call(ctx, "search_notebooks", map[string]any{"query": "", "maxResults": 100}, &r)
	return r.Hits, err
}

// Download saves a signed link to dest and checks its MD5. It writes to a
// temporary file first so dest only ever appears complete.
func (c *Client) Download(ctx context.Context, url, dest, wantMD5 string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.plain.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := md5.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("download failed: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); wantMD5 != "" && !strings.EqualFold(got, wantMD5) {
		os.Remove(tmp)
		return fmt.Errorf("the downloaded file is damaged (checksum %s, expected %s)", got, wantMD5)
	}
	return os.Rename(tmp, dest)
}

// Upload sends a file to a start_attachment_upload link. The link is a
// Google Cloud Storage resumable-upload link: one POST opens a session,
// then one PUT sends the bytes.
func (c *Client) Upload(ctx context.Context, uploadURL, path string, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("x-goog-resumable", "start")
	req.Header.Set("x-upload-content-length", strconv.FormatInt(size, 10))
	req.ContentLength = 0
	resp, err := c.plain.Do(req)
	if err != nil {
		return fmt.Errorf("upload failed to start: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	session := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK || session == "" {
		return fmt.Errorf("upload failed to start: HTTP %d", resp.StatusCode)
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, session, f)
	if err != nil {
		return err
	}
	put.ContentLength = size
	put.Header.Set("Content-Type", "application/octet-stream")
	resp, err = c.plain.Do(put)
	if err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("upload failed: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
