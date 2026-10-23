// Package evernote talks to Evernote's official MCP server
// (https://mcp.evernote.com/mcp). It signs in with OAuth 2.0, so no API key
// is needed, and wraps the handful of tools BigBadRescue uses.
package evernote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/FemoTheDon/bigbadrescue/internal/pace"
	"github.com/FemoTheDon/bigbadrescue/internal/version"
)

// DefaultEndpoint is Evernote's MCP server.
const DefaultEndpoint = "https://mcp.evernote.com/mcp"

// Config sets up a Client.
type Config struct {
	Endpoint    string // MCP server URL
	NoAuth      bool   // the server needs no sign-in (the built-in demo server)
	RedirectURL string // where Evernote sends the browser back to, on our own server
	// OpenBrowser shows the Evernote sign-in page.
	OpenBrowser func(url string) error
	Tokens      TokenStore
	// Optional ways to identify the app, for when Evernote doesn't allow
	// apps to register themselves: a pre-registered client ID, or the URL
	// of a published client metadata document.
	ClientID          string
	ClientMetadataURL string
	Limiter           *pace.Limiter
	// OnState is told when the connection state changes.
	OnState func(State)
}

// State is the connection state shown in the window.
type State struct {
	Status    string `json:"status"` // "signed-out", "signing-in", "connecting", "connected", "error"
	Message   string `json:"message,omitempty"`
	SignInURL string `json:"signInUrl,omitempty"`
}

// Client is a connection to the Evernote MCP server.
type Client struct {
	cfg     Config
	httpc   *http.Client // for the MCP server (with auth)
	plain   *http.Client // for signed download and upload links
	rt      *watchRT
	handler *auth.AuthorizationCodeHandler

	mu      sync.Mutex
	session *mcp.ClientSession
	state   State
	codes   chan auth.AuthorizationResult
}

// New makes a client. It does not connect until Connect or the first call.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	rt := &watchRT{base: http.DefaultTransport}
	c := &Client{
		cfg:   cfg,
		rt:    rt,
		httpc: &http.Client{Transport: rt},
		plain: &http.Client{Timeout: 10 * time.Minute},
		codes: make(chan auth.AuthorizationResult, 1),
		state: State{Status: "signed-out"},
	}
	if cfg.NoAuth {
		return c, nil
	}
	saved, _ := cfg.Tokens.Load()
	hc := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL: cfg.RedirectURL,
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:              version.App + " by " + version.Company,
				RedirectURIs:            []string{cfg.RedirectURL},
				GrantTypes:              []string{"authorization_code", "refresh_token"},
				ResponseTypes:           []string{"code"},
				TokenEndpointAuthMethod: "none",
			},
		},
		AuthorizationCodeFetcher: c.fetchCode,
		RequestRefreshToken:      true,
		AcceptUnadvertisedIss:    true,
		Client:                   c.plain,
		NewTokenSource: func(ctx context.Context, oc *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			_ = cfg.Tokens.Save(savedFrom(oc, tok))
			return &savingSource{src: oc.TokenSource(context.WithValue(context.Background(), oauth2.HTTPClient, c.plain), tok), cfg: oc, ts: cfg.Tokens, last: tok.AccessToken}, nil
		},
	}
	if cfg.ClientMetadataURL != "" {
		hc.ClientIDMetadataDocumentConfig = &auth.ClientIDMetadataDocumentConfig{URL: cfg.ClientMetadataURL}
	}
	if cfg.ClientID != "" {
		hc.PreregisteredClient = &oauthex.ClientCredentials{ClientID: cfg.ClientID}
	}
	if saved != nil {
		oc := saved.config()
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, c.plain)
		hc.InitialTokenSource = &savingSource{src: oc.TokenSource(ctx, saved.Token), cfg: oc, ts: cfg.Tokens, last: saved.Token.AccessToken}
		c.state = State{Status: "connecting"}
	}
	h, err := auth.NewAuthorizationCodeHandler(hc)
	if err != nil {
		return nil, err
	}
	c.handler = h
	return c, nil
}

// HasSavedSignIn reports whether a sign-in from an earlier run exists.
func (c *Client) HasSavedSignIn() bool {
	s, _ := c.cfg.Tokens.Load()
	return s != nil || c.cfg.NoAuth
}

// State returns the connection state.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Client) setState(s State) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
	if c.cfg.OnState != nil {
		c.cfg.OnState(s)
	}
}

// fetchCode opens the Evernote sign-in page and waits for the browser to
// come back to our /oauth/callback page.
func (c *Client) fetchCode(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	// Drop a stale code from an earlier attempt.
	select {
	case <-c.codes:
	default:
	}
	c.setState(State{Status: "signing-in", Message: "Finish signing in to Evernote in your browser.", SignInURL: args.URL})
	if c.cfg.OpenBrowser != nil {
		if err := c.cfg.OpenBrowser(args.URL); err != nil {
			log.Printf("open the sign-in page yourself: %s", args.URL)
		}
	}
	t := time.NewTimer(10 * time.Minute)
	defer t.Stop()
	select {
	case r := <-c.codes:
		if r.Code == "" {
			return nil, errors.New("Evernote didn't complete the sign-in")
		}
		return &r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, errors.New("the sign-in timed out; press Connect to try again")
	}
}

// Callback is called by the app's /oauth/callback page with the query
// string Evernote sent the browser back with. It returns an error message
// to show in the browser, or "".
func (c *Client) Callback(code, state, iss, oauthErr, desc string) string {
	if oauthErr != "" {
		msg := oauthErr
		if desc != "" {
			msg += ": " + desc
		}
		select {
		case c.codes <- auth.AuthorizationResult{}:
		default:
		}
		return msg
	}
	select {
	case c.codes <- auth.AuthorizationResult{Code: code, State: state, Iss: iss}:
	default:
	}
	return ""
}

// Connect opens the MCP session, signing in first if needed.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.session != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	c.setState(State{Status: "connecting"})
	t := &mcp.StreamableClientTransport{
		Endpoint:             c.cfg.Endpoint,
		HTTPClient:           c.httpc,
		DisableStandaloneSSE: true,
		MaxRetries:           2,
	}
	if c.handler != nil {
		t.OAuthHandler = c.handler
	}
	client := mcp.NewClient(&mcp.Implementation{Name: version.App, Version: version.Version}, nil)
	cs, err := client.Connect(ctx, t, nil)
	if err != nil {
		msg := friendlyConnectError(err)
		c.setState(State{Status: "error", Message: msg})
		return errors.New(msg)
	}
	c.mu.Lock()
	c.session = cs
	c.mu.Unlock()
	c.setState(State{Status: "connected"})
	return nil
}

// SignOut forgets the sign-in and closes the session.
func (c *Client) SignOut() error {
	c.drop()
	c.setState(State{Status: "signed-out"})
	return c.cfg.Tokens.Clear()
}

func (c *Client) drop() {
	c.mu.Lock()
	s := c.session
	c.session = nil
	c.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

func friendlyConnectError(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "registration"):
		return "Evernote didn't accept BigBadRescue's sign-in request (" + s + "). See \"If sign-in fails\" in the README."
	case strings.Contains(s, "no such host"), strings.Contains(s, "dial tcp"):
		return "Couldn't reach Evernote. Check your internet connection."
	}
	return "Couldn't connect to Evernote: " + s
}

// RateLimitError means Evernote asked the app to slow down.
type RateLimitError struct {
	Wait time.Duration // 0 if Evernote didn't say
	Msg  string
}

func (e *RateLimitError) Error() string { return "Evernote asked BigBadRescue to slow down: " + e.Msg }

// ToolError is an error a tool reported (bad input, note not found…).
type ToolError struct {
	Tool string
	Msg  string
}

func (e *ToolError) Error() string { return e.Msg }

var (
	rateRE  = regexp.MustCompile(`(?i)rate.?limit|too many requests|\b429\b|throttl`)
	retryRE = regexp.MustCompile(`(?i)(?:retry|try again|wait)[^0-9]{0,30}(\d+)\s*(s|sec|second|m|min|minute)`)
	durRE   = regexp.MustCompile(`(?i)rateLimitDuration\D{0,5}(\d+)`)
)

// call runs one tool. It waits for the limiter, pauses and retries when
// Evernote says to slow down, and reconnects once if the session dropped.
func (c *Client) call(ctx context.Context, tool string, args map[string]any, out any) error {
	reconnected := false
	for {
		if c.cfg.Limiter != nil {
			if err := c.cfg.Limiter.Wait(ctx); err != nil {
				return err
			}
		}
		if err := c.Connect(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		cs := c.session
		c.mu.Unlock()
		mark := c.rt.mark()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if hit, wait := c.rt.limitedSince(mark); hit && err != nil {
			if !c.pause(wait, "Evernote returned “too many requests”.") {
				return ctx.Err()
			}
			if err != nil && !reconnected {
				c.drop()
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !reconnected {
				reconnected = true
				c.drop()
				continue
			}
			return fmt.Errorf("Evernote didn't answer (%s): %w", tool, err)
		}
		if res.IsError {
			msg := resultText(res)
			if rateRE.MatchString(msg) {
				if !c.pause(waitFrom(msg), msg) {
					return ctx.Err()
				}
				continue
			}
			return &ToolError{Tool: tool, Msg: msg}
		}
		if c.cfg.Limiter != nil {
			c.cfg.Limiter.Succeeded()
		}
		if out == nil {
			return nil
		}
		return decodeResult(res, out)
	}
}

// pause holds every call; it returns false if there is no limiter (tests).
func (c *Client) pause(wait time.Duration, reason string) bool {
	if c.cfg.Limiter == nil {
		return false
	}
	c.cfg.Limiter.PauseFor(wait, reason)
	return true
}

func waitFrom(msg string) time.Duration {
	if m := durRE.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		return time.Duration(n) * time.Second
	}
	if m := retryRE.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		if strings.HasPrefix(strings.ToLower(m[2]), "m") {
			return time.Duration(n) * time.Minute
		}
		return time.Duration(n) * time.Second
	}
	return 0
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	if len(parts) == 0 {
		return "Evernote reported an error"
	}
	return strings.Join(parts, "\n")
}

func decodeResult(res *mcp.CallToolResult, out any) error {
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err == nil {
			if err := json.Unmarshal(b, out); err == nil {
				return nil
			}
		}
	}
	text := resultText(res)
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("unexpected answer from Evernote: %.200s", text)
	}
	return nil
}

// watchRT notices HTTP 429 answers (and their Retry-After) from the MCP
// server, which the MCP library otherwise reports as a plain error.
type watchRT struct {
	base http.RoundTripper
	mu   sync.Mutex
	seq  int
	hits []rtHit
}

type rtHit struct {
	seq  int
	wait time.Duration
}

func (w *watchRT) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := w.base.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		var wait time.Duration
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if n, err := strconv.Atoi(ra); err == nil {
				wait = time.Duration(n) * time.Second
			} else if t, err := http.ParseTime(ra); err == nil {
				wait = time.Until(t)
			}
		}
		w.mu.Lock()
		w.seq++
		w.hits = append(w.hits, rtHit{w.seq, wait})
		if len(w.hits) > 20 {
			w.hits = w.hits[1:]
		}
		w.mu.Unlock()
	}
	return resp, err
}

func (w *watchRT) mark() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

func (w *watchRT) limitedSince(mark int) (bool, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.hits) - 1; i >= 0; i-- {
		if w.hits[i].seq > mark {
			return true, w.hits[i].wait
		}
	}
	return false, 0
}
