package evernote

import (
	"strings"
	"testing"
)

// Real ENML from a note after Evernote's editor migrated it.
const migrated = `<!DOCTYPE en-note SYSTEM "http://xml.evernote.com/pub/enml2.dtd"><en-note style="word-wrap: break-word;"><en-media style="--en-transcription:&quot;{\&quot;boxHeight\&quot;:null}&quot;;--en-requiredFeatures:&quot;[\&quot;enhancedAudio\&quot;]&quot;;" hash="c4456db0b0b39c8feeb50503237c77a2" type="audio/amr" /><en-media hash="ae0b2caaf5fce5195ed4ce45b234ad8d" type="audio/mpeg" /><div>Like a bridge idea</div><div><br/></div></en-note>`

func TestMediaTag(t *testing.T) {
	tag, err := MediaTag(migrated, "C4456DB0B0B39C8FEEB50503237C77A2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tag, `<en-media style="--en-transcription`) || !strings.HasSuffix(tag, `type="audio/amr" />`) {
		t.Fatalf("tag = %s", tag)
	}
	if strings.Count(migrated, tag) != 1 {
		t.Fatal("tag is not an exact substring")
	}
	if _, err := MediaTag(migrated, strings.Repeat("0", 32)); err != ErrNoTag {
		t.Fatalf("missing hash: %v", err)
	}
	old := `<en-note><div><en-media type="audio/amr" hash="c4456db0b0b39c8feeb50503237c77a2"></en-media></div></en-note>`
	tag, err = MediaTag(old, "c4456db0b0b39c8feeb50503237c77a2")
	if err != nil || tag != `<en-media type="audio/amr" hash="c4456db0b0b39c8feeb50503237c77a2"></en-media>` {
		t.Fatalf("old-style tag = %q, %v", tag, err)
	}
	dup := `<en-note><div><en-media hash="c4456db0b0b39c8feeb50503237c77a2" type="audio/amr"/></div><div><en-media hash="c4456db0b0b39c8feeb50503237c77a2" type="audio/amr"/></div></en-note>`
	as, err := MediaAnchors(dup, "c4456db0b0b39c8feeb50503237c77a2")
	if err != nil || len(as) != 2 {
		t.Fatalf("duplicates: %v %v", as, err)
	}
	for _, a := range as {
		if strings.Count(dup, a.Find) != 1 || a.Before+a.Tag+a.After != a.Find {
			t.Fatalf("anchor not unique or inconsistent: %+v", a)
		}
	}
}

func TestPlainText(t *testing.T) {
	got := PlainText(migrated)
	if got != "Like a bridge idea" {
		t.Fatalf("got %q", got)
	}
	got = PlainText(`<en-note><div>Verse A</div><div>You are God &amp; great</div><div><br/></div><div><br/></div><div>Chorus</div></en-note>`)
	if got != "Verse A\nYou are God & great\n\nChorus" {
		t.Fatalf("got %q", got)
	}
}

func TestTextAnchors(t *testing.T) {
	// An old-format note like the ones that failed in v1.0.
	enml := `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE en-note SYSTEM "http://xml.evernote.com/pub/enml2.dtd"><en-note style="word-wrap: break-word; -webkit-nbsp-mode: space; -webkit-line-break: after-white-space;"><div><en-media type="audio/amr" hash="0a1b2c3d4e5f60718293a4b5c6d7e8f9"/></div><div><br/>Sing it again<br/></div></en-note>`
	after, before := TextAnchors(enml, "0a1b2c3d4e5f60718293a4b5c6d7e8f9")
	if after != "Sing it again" || before != "" {
		t.Fatalf("after %q before %q", after, before)
	}
	enml = `<en-note><div>Intro &amp; idea</div><div><en-media type="audio/amr" hash="0a1b2c3d4e5f60718293a4b5c6d7e8f9"/></div></en-note>`
	after, before = TextAnchors(enml, "0a1b2c3d4e5f60718293a4b5c6d7e8f9")
	if after != "" || before != "idea" {
		t.Fatalf("after %q before %q", after, before)
	}
}
