package ui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The folder picker and Finder helpers come from BigBadConverter.

var errCancelled = errors.New("cancelled")
var errNoPicker = errors.New("no folder picker is available here; type the folder path instead")

// appleString quotes s for AppleScript.
func appleString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// pickFolder shows the system folder picker.
func pickFolder(prompt, start string) (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		body := `return POSIX path of (choose folder with prompt ` + appleString(prompt) + startLoc(start) + `)`
		paths, err := runAppleScript(start, body)
		if err != nil {
			return "", err
		}
		return paths[0], nil
	default:
		if p, err := exec.LookPath("zenity"); err == nil {
			cmd = exec.Command(p, "--file-selection", "--directory", "--title="+prompt, "--filename="+start+"/")
		} else {
			return "", errNoPicker
		}
	}
	paths, err := runPicker(cmd, "\n")
	if err != nil {
		return "", err
	}
	return paths[0], nil
}

// startLoc is the "default location" clause for a picker, using the startLoc
// variable that runAppleScript sets, or nothing if start isn't a folder.
func startLoc(start string) string {
	if fi, err := os.Stat(start); err == nil && fi.IsDir() {
		return ` default location startLoc`
	}
	return ""
}

// runAppleScript runs a picker. It is shown by the frontmost app (the
// BigBadRescue window) so it comes up in front of the window. If macOS
// refuses that (Automation permission turned off), it runs again on its own
// so the picker still appears.
func runAppleScript(start, body string) ([]string, error) {
	head := "set startLoc to path to home folder\n"
	if start != "" {
		head = `set startLoc to POSIX file ` + appleString(start) + "\n"
	}
	var stderr strings.Builder
	cmd := exec.Command("osascript", "-e", head+"tell application (path to frontmost application as text)\n"+body+"\nend tell")
	cmd.Stderr = &stderr
	paths, err := runPicker(cmd, "\n")
	if !errors.Is(err, errCancelled) || strings.Contains(stderr.String(), "-128") {
		return paths, err // picked, or the user pressed Cancel
	}
	return runPicker(exec.Command("osascript", "-e", head+body), "\n")
}

func runPicker(cmd *exec.Cmd, sep string) ([]string, error) {
	out, err := cmd.Output()
	if err != nil {
		// Every picker exits non-zero when the user cancels.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, errCancelled
		}
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(out), sep) {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, errCancelled
	}
	return paths, nil
}

// reveal selects a file in Finder, or opens a folder.
func reveal(path string, selectIt bool) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		if selectIt {
			cmd = exec.Command("open", "-R", path)
		} else {
			cmd = exec.Command("open", path)
		}
	case "windows":
		if selectIt {
			cmd = exec.Command("explorer", "/select,"+path)
		} else {
			cmd = exec.Command("explorer", path)
		}
	default:
		if selectIt {
			path = filepath.Dir(path)
		}
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func hasZenity() bool {
	_, err := exec.LookPath("zenity")
	return err == nil
}
