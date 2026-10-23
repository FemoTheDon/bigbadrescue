package ui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// openWindow shows url in a chromeless app window if a Chromium-based browser
// is installed, falling back to the default browser.
func openWindow(url string) error {
	for _, b := range chromiumCandidates() {
		path, err := exec.LookPath(b)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, "--app="+url, "--window-size=1280,860", "--new-window")
		if err := cmd.Start(); err == nil {
			go func() { _ = cmd.Wait() }()
			return nil
		}
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return errors.New("no browser found")
		}
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func chromiumCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		}
	case "windows":
		var out []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if base := os.Getenv(env); base != "" {
				out = append(out,
					filepath.Join(base, `Google\Chrome\Application\chrome.exe`),
					filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
				)
			}
		}
		return out
	default:
		return []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"}
	}
}

// openBrowser opens a page (Evernote's sign-in) in the normal browser,
// where the person is likely already signed in to Evernote.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return errors.New("no browser found")
		}
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
