package evernote

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Saved is the sign-in kept between runs: the OAuth client Evernote
// registered for this app and the current token.
type Saved struct {
	ClientID     string        `json:"clientId"`
	ClientSecret string        `json:"clientSecret,omitempty"`
	AuthURL      string        `json:"authUrl"`
	TokenURL     string        `json:"tokenUrl"`
	AuthStyle    int           `json:"authStyle"`
	Scopes       []string      `json:"scopes,omitempty"`
	RedirectURL  string        `json:"redirectUrl"`
	Token        *oauth2.Token `json:"token"`
	SavedAt      time.Time     `json:"savedAt"`
}

func (s *Saved) config() *oauth2.Config {
	return &oauth2.Config{
		ClientID: s.ClientID, ClientSecret: s.ClientSecret,
		Endpoint:    oauth2.Endpoint{AuthURL: s.AuthURL, TokenURL: s.TokenURL, AuthStyle: oauth2.AuthStyle(s.AuthStyle)},
		Scopes:      s.Scopes,
		RedirectURL: s.RedirectURL,
	}
}

func savedFrom(cfg *oauth2.Config, tok *oauth2.Token) *Saved {
	return &Saved{
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		AuthURL: cfg.Endpoint.AuthURL, TokenURL: cfg.Endpoint.TokenURL, AuthStyle: int(cfg.Endpoint.AuthStyle),
		Scopes: cfg.Scopes, RedirectURL: cfg.RedirectURL, Token: tok, SavedAt: time.Now(),
	}
}

// TokenStore keeps the sign-in: in the macOS Keychain on a Mac, otherwise
// in a private file in the settings folder.
type TokenStore struct {
	Service string // Keychain service name
	File    string // fallback file
}

const keychainAccount = "evernote"

// Load returns the saved sign-in, or nil if there is none.
func (ts TokenStore) Load() (*Saved, error) {
	var raw []byte
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("/usr/bin/security", "find-generic-password", "-s", ts.Service, "-a", keychainAccount, "-w").Output()
		if err != nil {
			return nil, nil // not found
		}
		raw = bytes.TrimSpace(out)
	} else {
		b, err := os.ReadFile(ts.File)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		raw = b
	}
	var s Saved
	if err := json.Unmarshal(raw, &s); err != nil || s.Token == nil {
		return nil, nil
	}
	return &s, nil
}

// Save stores the sign-in.
func (ts TokenStore) Save(s *Saved) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		// -U updates an existing item. The token goes in as an argument
		// because `security` has no way to read it from stdin.
		cmd := exec.Command("/usr/bin/security", "add-generic-password", "-U", "-s", ts.Service, "-a", keychainAccount,
			"-l", ts.Service+" (Evernote sign-in)", "-w", string(b))
		if out, err := cmd.CombinedOutput(); err != nil {
			return errors.New("couldn't save the sign-in to the Keychain: " + strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(ts.File), 0o700); err != nil {
		return err
	}
	return os.WriteFile(ts.File, b, 0o600)
}

// Clear forgets the sign-in.
func (ts TokenStore) Clear() error {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("/usr/bin/security", "delete-generic-password", "-s", ts.Service, "-a", keychainAccount).Run()
		return nil
	}
	err := os.Remove(ts.File)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// savingSource saves the token every time it is refreshed.
type savingSource struct {
	src  oauth2.TokenSource
	cfg  *oauth2.Config
	ts   TokenStore
	last string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		_ = s.ts.Save(savedFrom(s.cfg, t))
	}
	return t, nil
}
