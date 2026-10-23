package evernote

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var mediaRE = regexp.MustCompile(`<en-media\b[^>]*>`)
var hashAttrRE = regexp.MustCompile(`\bhash\s*=\s*["']([0-9a-fA-F]{32})["']`)

// ErrNoTag means the note body has no <en-media> tag for an attachment
// (the file is in the note's list but not shown in its text).
var ErrNoTag = errors.New("the note text has no tag for this attachment")

// Anchor is a piece of a note's ENML that occurs exactly once and contains
// one <en-media> tag, so it can be an edit_note "find" string. Before and
// After are the text around the tag inside Find (usually empty).
type Anchor struct {
	Find, Before, Tag, After string
}

// MediaAnchors returns an anchor for every <en-media> tag of the
// attachment with this hash, copied byte for byte from the ENML. A tag that
// appears more than once (the same file shown twice) is widened with the
// text around it until it is unique.
func MediaAnchors(enml, hash string) ([]Anchor, error) {
	hash = strings.ToLower(hash)
	var out []Anchor
	for _, loc := range mediaRE.FindAllStringIndex(enml, -1) {
		start, end := loc[0], loc[1]
		m := hashAttrRE.FindStringSubmatch(enml[start:end])
		if m == nil || strings.ToLower(m[1]) != hash {
			continue
		}
		if !strings.HasSuffix(enml[start:end], "/>") && strings.HasPrefix(enml[end:], "</en-media>") {
			end += len("</en-media>")
		}
		a, b := start, end
		for strings.Count(enml, enml[a:b]) > 1 {
			if b < len(enml) {
				b = min(len(enml), b+24)
			} else if a > 0 {
				a = max(0, a-24)
			} else {
				return nil, fmt.Errorf("couldn't find a unique place for the attachment in the note")
			}
		}
		out = append(out, Anchor{Find: enml[a:b], Before: enml[a:start], Tag: enml[start:end], After: enml[end:b]})
	}
	if len(out) == 0 {
		return nil, ErrNoTag
	}
	return out, nil
}

// MediaTag returns the exact <en-media …> tag for an attachment (the first
// one, if it appears more than once).
func MediaTag(enml, hash string) (string, error) {
	as, err := MediaAnchors(enml, hash)
	if err != nil {
		return "", err
	}
	return as[0].Tag, nil
}

// PlainText turns ENML into readable text for the note viewer.
func PlainText(enml string) string {
	s := enml
	if i := strings.Index(s, "<en-note"); i >= 0 {
		s = s[i:]
	}
	s = openRE.ReplaceAllString(s, "")
	s = blockRE.ReplaceAllString(s, "\n")
	s = mediaRE.ReplaceAllString(s, "\n")
	s = tagRE.ReplaceAllString(s, "")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&apos;", "'").Replace(s)
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 || len(out) == 0 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

var (
	openRE  = regexp.MustCompile(`(?i)<(div|p|li|h[1-6]|tr|blockquote|ul|ol|table)\b[^>]*>`)
	blockRE = regexp.MustCompile(`(?i)<br\s*/?>|</(div|p|li|h[1-6]|tr|blockquote|ul|ol|table)>`)
	tagRE   = regexp.MustCompile(`<[^>]+>`)
)

var textRunRE = regexp.MustCompile(`>([^<]+)<`)
var entityRE = regexp.MustCompile(`&[#0-9A-Za-z]+;`)

// TextAnchors returns plain text near the attachment's first tag that
// occurs exactly once in the ENML: the nearest piece after the tag and the
// nearest piece before it ("" when there is none). Evernote matches
// edit_note's "find" against the note in its new editor format, so for an
// old note a "find" holding tags can miss even though it was copied from
// get_note; plain text survives that change.
func TextAnchors(enml, hash string) (after, before string) {
	as, err := MediaAnchors(enml, hash)
	if err != nil {
		return "", ""
	}
	start := strings.Index(enml, as[0].Find) + len(as[0].Before)
	end := start + len(as[0].Tag)
	body := strings.Index(enml, "<en-note")
	pick := func(lo, hi int, last bool) string {
		var found []string
		for _, m := range textRunRE.FindAllStringSubmatchIndex(enml[lo:hi], -1) {
			run := enml[lo+m[2] : lo+m[3]]
			for _, part := range entityRE.Split(run, -1) {
				part = strings.TrimSpace(part)
				if len([]rune(part)) >= 3 && strings.Count(enml, part) == 1 {
					found = append(found, part)
				}
			}
		}
		if len(found) == 0 {
			return ""
		}
		if last {
			return found[len(found)-1]
		}
		return found[0]
	}
	if end <= len(enml) {
		after = pick(end-1, len(enml), false)
	}
	if body >= 0 && body < start {
		before = pick(body, start+1, true)
	}
	return after, before
}
