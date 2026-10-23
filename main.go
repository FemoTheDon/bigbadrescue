// BigBadRescue by Breadbaskets finds Evernote notes whose old voice
// recordings (AMR, 3GPP, OGG) no longer play in Evernote's mobile apps,
// converts them to MP3, puts the MP3 on the same note and, once you have
// listened to it, removes the original. Originals are backed up first.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/FemoTheDon/bigbadrescue/internal/convert"
	"github.com/FemoTheDon/bigbadrescue/internal/evernote"
	"github.com/FemoTheDon/bigbadrescue/internal/mock"
	"github.com/FemoTheDon/bigbadrescue/internal/ui"
	"github.com/FemoTheDon/bigbadrescue/internal/version"
)

func main() {
	port := flag.Int("port", 47830, "port for the window; Evernote's sign-in returns to it")
	noOpen := flag.Bool("no-open", false, "don't open the window; print its address instead")
	stay := flag.Bool("stay", false, "keep running after the window is closed")
	demo := flag.Bool("demo", false, "try the app on a pretend Evernote account with example notes")
	demoAMR := flag.String("demo-amr", "", "with -demo: a real .amr file to use in the example notes")
	endpoint := flag.String("mcp", evernote.DefaultEndpoint, "Evernote MCP server address")
	dataDir := flag.String("data", "", "folder for settings, memory and cache (default: Application Support/BigBadRescue)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Title())
		return
	}
	log.SetFlags(log.LstdFlags)
	log.SetPrefix(version.App + ": ")

	dir := *dataDir
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			log.Fatal(err)
		}
		dir = filepath.Join(base, version.App)
	}
	opts := ui.Options{Port: *port, Open: !*noOpen, Stay: *stay || *noOpen, Endpoint: *endpoint, DataDir: dir}
	ff, err := convert.FindFFmpeg()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		opts.FFmpegOK, err = convert.Check(ctx, ff)
		cancel()
	}
	opts.FFmpeg, opts.FFmpegEr = ff, err
	if err != nil {
		log.Printf("warning: %v (see README: install it with `brew install ffmpeg`)", err)
	} else {
		log.Printf("using ffmpeg %s (%s)", opts.FFmpegOK, ff)
	}

	if *demo {
		if opts.FFmpegEr != nil {
			log.Fatal("the demo needs ffmpeg to make its example recordings")
		}
		opts.DataDir = filepath.Join(dir, "demo")
		_ = os.RemoveAll(opts.DataDir) // a fresh pretend account every time
		samples := filepath.Join(opts.DataDir, "samples")
		if err := os.MkdirAll(samples, 0o755); err != nil {
			log.Fatal(err)
		}
		fake := mock.New()
		defer fake.Close()
		if err := fake.Seed(ff, samples, *demoAMR); err != nil {
			log.Fatal(err)
		}
		opts.Endpoint, opts.NoAuth, opts.Demo = fake.URL, true, true
		log.Printf("demo: pretend Evernote at %s", fake.URL)
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := ui.Run(opts); err != nil {
		log.Fatal(err)
	}
}
