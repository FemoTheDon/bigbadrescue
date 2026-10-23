package mock

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Tone makes a short test recording with ffmpeg: a voice-like warble in the
// given container and codec (for example "ogg"/"libvorbis").
func Tone(ffmpeg, dir, name string, seconds float64, freq int, format, codec string, extra ...string) ([]byte, error) {
	out := filepath.Join(dir, name)
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", fmt.Sprintf("aevalsrc=0.4*sin(2*PI*(%d+40*sin(2*PI*3*t))*t)*(0.6+0.4*sin(2*PI*0.7*t)):s=8000:d=%.2f", freq, seconds),
		"-ac", "1", "-c:a", codec}
	args = append(args, extra...)
	args = append(args, "-f", format, out)
	if b, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("making %s: %v %s", name, err, b)
	}
	return os.ReadFile(out)
}

// Seed fills the fake account with example notes. amrSample, if set, is a
// real AMR file to use (ffmpeg builds rarely include an AMR encoder).
func (s *Server) Seed(ffmpeg, dir, amrSample string) error {
	music := s.AddNotebook("Music ideas")
	church := s.AddNotebook("Church")
	day := func(y, m, d, h int) time.Time { return time.Date(y, time.Month(m), d, h, 0, 0, 0, time.UTC) }

	ogg1, err := Tone(ffmpeg, dir, "a.ogg", 6.5, 220, "ogg", "libvorbis")
	if err != nil {
		return err
	}
	ogg2, err := Tone(ffmpeg, dir, "b.ogg", 4.2, 300, "ogg", "libvorbis")
	if err != nil {
		return err
	}
	gp, err := Tone(ffmpeg, dir, "c.3gp", 5.0, 260, "3gp", "aac", "-ar", "8000")
	if err != nil {
		return err
	}
	wav, err := Tone(ffmpeg, dir, "d.wav", 3.0, 200, "wav", "pcm_s16le")
	if err != nil {
		return err
	}
	var amr []byte
	if amrSample != "" {
		if amr, err = os.ReadFile(amrSample); err != nil {
			return err
		}
	}
	voice := func(name string, data []byte) File {
		if amr != nil {
			return File{Name: name + ".amr", Mime: "audio/amr", Data: amr}
		}
		return File{Name: name + ".ogg", Mime: "audio/ogg", Data: data}
	}
	s.AddNote("Choir rehearsal ideas", music, day(2014, 3, 12, 19), "Alto line for verse 2\nTry it a third higher", voice("12 Mar 2014 19:04:11", ogg1), voice("12 Mar 2014 19:11:40", ogg2))
	s.AddNote("Bridge melody hum", music, day(2014, 4, 3, 8), "Hummed on the bus", voice("03 Apr 2014 08:15:02", ogg2))
	s.AddNote("Sermon notes, Sunday", church, day(2015, 5, 18, 11), "Key points\n1. Grace\n2. Patience", File{Name: "Recording 18-05-2015.3gp", Mime: "audio/3gpp", Data: gp})
	s.AddNote("Voice memo from the car", music, day(2015, 9, 2, 17), "", File{Name: "Memo.ogg", Mime: "audio/ogg", Data: ogg1})
	s.AddNote("Studio WAV bounce", music, day(2016, 2, 2, 21), "This one already plays", File{Name: "Bounce.wav", Mime: "audio/wav", Data: wav})
	s.AddNote("Shopping list", church, day(2016, 6, 6, 9), "Plain text, no audio")
	return nil
}
