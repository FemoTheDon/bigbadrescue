// Package convert turns old voice-note audio (AMR, 3GPP, OGG) into MP3 with
// ffmpeg. It started life in BigBadConverter.
package convert

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Preset is how an MP3 is encoded.
type Preset struct {
	Name     string `json:"name"`
	Bitrate  string `json:"bitrate"`  // ffmpeg -b:a, e.g. "64k"
	Channels int    `json:"channels"` // 1 = mono
}

// Voice is the default: AMR and 3GPP voice notes are 8 kHz mono, so 64 kbps
// mono loses nothing and uploads half the bytes of 128 kbps.
var Voice = Preset{Name: "Voice", Bitrate: "64k", Channels: 1}

// Music is the BigBadConverter setting, for recordings worth the extra size.
var Music = Preset{Name: "Music", Bitrate: "128k", Channels: 2}

// PresetByName returns Voice unless name is "Music".
func PresetByName(name string) Preset {
	if strings.EqualFold(name, Music.Name) {
		return Music
	}
	return Voice
}

// Format describes one kind of attachment the app rescues.
type Format struct {
	Key   string   `json:"key"`   // "amr"
	Label string   `json:"label"` // "AMR"
	MIMEs []string `json:"mimes"` // Evernote MIME types that mean this format
}

// Formats are the attachment types that don't play in Evernote's mobile
// apps. WAV, MP3 and M4A play, so they are left alone.
var Formats = []Format{
	{"amr", "AMR", []string{"audio/amr", "audio/x-amr", "audio/amr-wb"}},
	{"3gpp", "3GPP", []string{"audio/3gpp", "audio/3gpp2", "video/3gpp"}},
	{"ogg", "OGG", []string{"audio/ogg", "audio/x-ogg", "application/ogg", "audio/opus", "audio/webm"}},
}

// FormatOfMIME returns the format a MIME type belongs to.
func FormatOfMIME(mime string) (Format, bool) {
	mime = strings.ToLower(strings.TrimSpace(mime))
	for _, f := range Formats {
		for _, m := range f.MIMEs {
			if m == mime {
				return f, true
			}
		}
	}
	return Format{}, false
}

// FormatByKey looks a format up by its key ("amr").
func FormatByKey(key string) (Format, bool) {
	for _, f := range Formats {
		if f.Key == strings.ToLower(key) {
			return f, true
		}
	}
	return Format{}, false
}

// OutputName is the MP3 file name for an input: the same name with .mp3.
func OutputName(input string) string {
	base := filepath.Base(input)
	return strings.TrimSuffix(base, filepath.Ext(base)) + ".mp3"
}

// FindFFmpeg looks for ffmpeg next to the app first (the copy bundled in
// BigBadRescue.app), then in the usual Homebrew and system locations.
func FindFFmpeg() (string, error) {
	if p := os.Getenv("BIGBAD_FFMPEG"); p != "" {
		if isExec(p) {
			return p, nil
		}
		return "", fmt.Errorf("BIGBAD_FFMPEG points at %s, which is not a program", p)
	}
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir := filepath.Dir(exe)
		dirs = append(dirs, dir, filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "..", "Resources", "bin"))
	}
	arch := map[string]string{"arm64": "arm64", "amd64": "x86_64"}[runtime.GOARCH]
	for _, d := range dirs {
		for _, name := range []string{"ffmpeg-" + arch, "ffmpeg"} {
			if p := filepath.Join(d, name); isExec(p) {
				return p, nil
			}
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	// Apps opened from Finder get a minimal PATH without Homebrew in it.
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/opt/local/bin/ffmpeg"} {
		if isExec(p) {
			return p, nil
		}
	}
	return "", errors.New("ffmpeg was not found")
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// Check runs ffmpeg once and returns its version line. It fails if the
// ffmpeg found cannot encode MP3.
func Check(ctx context.Context, ffmpeg string) (string, error) {
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return "", fmt.Errorf("ffmpeg at %s would not run: %w", ffmpeg, err)
	}
	if !strings.Contains(string(out), "libmp3lame") {
		return "", fmt.Errorf("ffmpeg at %s was built without MP3 support (libmp3lame)", ffmpeg)
	}
	v, _ := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-version").Output()
	line, _, _ := strings.Cut(string(v), "\n")
	line = strings.TrimPrefix(line, "ffmpeg version ")
	if i := strings.Index(line, " Copyright"); i > 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line), nil
}

// Converter runs ffmpeg.
type Converter struct {
	FFmpeg string
}

// Progress receives the fraction done, from 0 to 1. A negative value means
// the length of the input is unknown, so only "working" can be shown.
type Progress func(frac float64)

var durationRE = regexp.MustCompile(`Duration: (\d+):(\d\d):(\d\d(?:\.\d+)?)`)

// Convert encodes in to an MP3 at out using preset p. It writes to a hidden
// temporary file in the same folder and renames it at the end, so out only
// ever appears complete. An existing file at out is replaced.
func (c *Converter) Convert(ctx context.Context, in, out string, p Preset, progress Progress) error {
	if progress == nil {
		progress = func(float64) {}
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".bigbadrescue-*.mp3.part")
	if err != nil {
		return fmt.Errorf("can't write to %s: %w", filepath.Dir(out), err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath) // a no-op once renamed

	cmd := exec.CommandContext(ctx, c.FFmpeg,
		"-hide_banner", "-nostdin", "-y",
		"-i", in,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-c:a", "libmp3lame", "-b:a", p.Bitrate, "-ac", strconv.Itoa(max(1, p.Channels)),
		// 44.1 kHz for everything: AMR is 8 or 16 kHz, which some phone
		// players handle badly in MP3, and 44.1 kHz is what every player expects.
		"-ar", "44100",
		"-map_metadata", "-1", "-id3v2_version", "3",
		"-f", "mp3",
		"-progress", "pipe:1", "-nostats",
		tmpPath,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't start ffmpeg: %w", err)
	}

	durCh := make(chan time.Duration, 1)
	errLines := make(chan []string, 1)
	go func() {
		var lines []string
		sent := false
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !sent {
				if m := durationRE.FindStringSubmatch(line); m != nil {
					durCh <- parseHMS(m[1], m[2], m[3])
					sent = true
				}
			}
			lines = append(lines, line)
			if len(lines) > 40 {
				lines = lines[1:]
			}
		}
		if !sent {
			durCh <- 0
		}
		errLines <- lines
	}()

	var dur time.Duration
	gotDur := false
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		if !gotDur {
			// ffmpeg prints Duration before it writes any progress.
			select {
			case dur = <-durCh:
				gotDur = true
			case <-time.After(2 * time.Second):
			}
		}
		switch k {
		case "out_time_us", "out_time_ms": // both are microseconds
			us, err := strconv.ParseInt(v, 10, 64)
			if err != nil || us < 0 {
				continue
			}
			if dur <= 0 {
				progress(-1)
				continue
			}
			f := float64(us) / float64(dur.Microseconds())
			progress(min(f, 0.99))
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	lines := <-errLines

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return friendlyError(lines, waitErr)
	}
	if fi, err := os.Stat(tmpPath); err != nil || fi.Size() == 0 {
		return errors.New("ffmpeg finished but wrote no audio")
	}
	_ = os.Chmod(tmpPath, 0o644) // CreateTemp makes it private
	if err := os.Rename(tmpPath, out); err != nil {
		return fmt.Errorf("couldn't save %s: %w", filepath.Base(out), err)
	}
	progress(1)
	return nil
}

func parseHMS(h, m, s string) time.Duration {
	hh, _ := strconv.Atoi(h)
	mm, _ := strconv.Atoi(m)
	ss, _ := strconv.ParseFloat(s, 64)
	return time.Duration((float64(hh*3600+mm*60) + ss) * float64(time.Second))
}

// friendlyError turns ffmpeg's last words into a short message.
func friendlyError(lines []string, err error) error {
	all := strings.Join(lines, "\n")
	switch {
	case strings.Contains(all, "Invalid data found when processing input"),
		strings.Contains(all, "could not find codec parameters"):
		return errors.New("this doesn't look like a readable audio file")
	case strings.Contains(all, "matches no streams"),
		strings.Contains(all, "does not contain any stream"):
		return errors.New("no audio found in this file")
	case strings.Contains(all, "No such file or directory"):
		return errors.New("the file has moved or been deleted")
	case strings.Contains(all, "Permission denied"):
		return errors.New("permission denied (check the file and the save folder)")
	case strings.Contains(all, "No space left on device"):
		return errors.New("the disk is full")
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "[out#") {
			return fmt.Errorf("ffmpeg: %s", l)
		}
	}
	return fmt.Errorf("ffmpeg failed: %w", err)
}
