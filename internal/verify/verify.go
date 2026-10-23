// Package verify checks that a converted MP3 really plays: it decodes the
// whole file, compares its length with the original and makes sure it is not
// silent.
package verify

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// Tolerance is how far the MP3's length may drift from the original's.
const Tolerance = 500 * time.Millisecond

// SilenceDB is the loudest peak (in dB) below which a file counts as silent.
const SilenceDB = -50.0

// Audio is what decoding a file found.
type Audio struct {
	Duration time.Duration `json:"duration"`
	PeakDB   float64       `json:"peakDb"`
}

var (
	timeRE = regexp.MustCompile(`time=(\d+):(\d\d):(\d\d(?:\.\d+)?)`)
	peakRE = regexp.MustCompile(`max_volume: (-?(?:\d+(?:\.\d+)?|inf)) dB`)
)

// Decode runs the whole file through ffmpeg and reports its true length and
// peak level. Header durations are not trusted: for AMR they are estimates
// (one test file claimed 81 s and really ran 78 s).
func Decode(ctx context.Context, ffmpeg, path string) (Audio, error) {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-v", "info",
		"-i", path, "-map", "0:a:0", "-af", "volumedetect", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stderr.String()
	if err != nil {
		return Audio{}, fmt.Errorf("the file would not decode: %s", lastLine(out))
	}
	if bytes.Contains(stderr.Bytes(), []byte("Error while decoding")) {
		return Audio{}, fmt.Errorf("the file has damaged audio frames")
	}
	ms := timeRE.FindAllStringSubmatch(out, -1)
	if len(ms) == 0 {
		return Audio{}, fmt.Errorf("couldn't measure the file's length")
	}
	m := ms[len(ms)-1]
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	s, _ := strconv.ParseFloat(m[3], 64)
	a := Audio{Duration: time.Duration((float64(h*3600+mi*60) + s) * float64(time.Second)), PeakDB: -200}
	if pm := peakRE.FindStringSubmatch(out); pm != nil && pm[1] != "-inf" {
		a.PeakDB, _ = strconv.ParseFloat(pm[1], 64)
	}
	return a, nil
}

// Result is the outcome of checking a converted file against its original.
type Result struct {
	Original Audio  `json:"original"`
	MP3      Audio  `json:"mp3"`
	Problem  string `json:"problem,omitempty"`
}

// OK reports whether the check passed.
func (r Result) OK() bool { return r.Problem == "" }

// Compare decodes both files and checks the MP3 plays to the end, runs as
// long as the original, and is not silent (unless the original is too).
func Compare(ctx context.Context, ffmpeg, original, mp3 string) (Result, error) {
	var r Result
	var err error
	if r.Original, err = Decode(ctx, ffmpeg, original); err != nil {
		return r, fmt.Errorf("original: %w", err)
	}
	if r.MP3, err = Decode(ctx, ffmpeg, mp3); err != nil {
		r.Problem = "the MP3 does not play: " + err.Error()
		return r, nil
	}
	if d := r.MP3.Duration - r.Original.Duration; d > Tolerance || d < -Tolerance {
		r.Problem = fmt.Sprintf("the MP3 runs %.1f s but the original runs %.1f s",
			r.MP3.Duration.Seconds(), r.Original.Duration.Seconds())
		return r, nil
	}
	if r.MP3.PeakDB < SilenceDB && r.Original.PeakDB >= SilenceDB {
		r.Problem = "the MP3 is silent"
	}
	return r, nil
}

// MD5File returns the lowercase hex MD5 of a file and its size.
func MD5File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := md5.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func lastLine(s string) string {
	lines := bytes.Split(bytes.TrimSpace([]byte(s)), []byte("\n"))
	if len(lines) == 0 {
		return "unknown error"
	}
	return string(bytes.TrimSpace(lines[len(lines)-1]))
}
