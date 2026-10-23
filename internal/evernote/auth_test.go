package evernote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSignIn walks the whole OAuth 2.0 flow against a fake authorization
// server: discovery, dynamic client registration, the browser round trip,
// the token exchange, saving the token, and reusing it on the next start.
func TestSignIn(t *testing.T) {
	var registered, tokens atomic.Int32
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "search_notes", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"hits":[],"totalResultCount":893,"isLastPage":true}`}}}, nil
		})
	mcpH := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+ts.URL+`/.well-known/oauth-protected-resource/mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpH.ServeHTTP(w, r)
	})
	js := func(w http.ResponseWriter, v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"resource": ts.URL + "/mcp", "authorization_servers": []string{ts.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"issuer": ts.URL, "authorization_endpoint": ts.URL + "/authorize", "token_endpoint": ts.URL + "/token",
			"registration_endpoint": ts.URL + "/register", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		registered.Add(1)
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		if m["client_name"] != "BigBadRescue by Breadbaskets" {
			t.Errorf("client_name %v", m["client_name"])
		}
		w.WriteHeader(http.StatusCreated)
		js(w, map[string]any{"client_id": "bbr-client", "redirect_uris": m["redirect_uris"], "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge") == "" || q.Get("client_id") != "bbr-client" {
			t.Errorf("authorize query %v", q)
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=abc&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokens.Add(1)
		r.ParseForm()
		if r.Form.Get("code") != "abc" || r.Form.Get("code_verifier") == "" {
			t.Errorf("token form %v", r.Form)
		}
		js(w, map[string]any{"access_token": "good-token", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "r1"})
	})

	store := TokenStore{Service: "BigBadRescue-test", File: filepath.Join(t.TempDir(), "signin.json")}
	var c *Client
	// The "browser": follow the sign-in page's redirect and hand the code
	// to the app, as the /oauth/callback page does.
	browser := func(u string) error {
		go func() {
			hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := hc.Get(u)
			if err != nil {
				t.Error(err)
				return
			}
			loc, _ := url.Parse(resp.Header.Get("Location"))
			q := loc.Query()
			c.Callback(q.Get("code"), q.Get("state"), q.Get("iss"), q.Get("error"), "")
		}()
		return nil
	}
	newClient := func() *Client {
		cl, err := New(Config{Endpoint: ts.URL + "/mcp", RedirectURL: "http://127.0.0.1:47830/oauth/callback", OpenBrowser: browser, Tokens: store})
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}
	c = newClient()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := c.SearchNotes(ctx, "resource:audio/amr", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalResultCount != 893 || c.State().Status != "connected" {
		t.Fatalf("result %+v state %+v", r, c.State())
	}
	if registered.Load() != 1 || tokens.Load() != 1 {
		t.Fatalf("registered %d, tokens %d", registered.Load(), tokens.Load())
	}
	saved, _ := store.Load()
	if saved == nil || saved.ClientID != "bbr-client" || saved.Token.RefreshToken != "r1" || !strings.HasSuffix(saved.TokenURL, "/token") {
		t.Fatalf("saved %+v", saved)
	}

	// Next start: the saved sign-in is used with no browser and no new registration.
	c = newClient()
	if !c.HasSavedSignIn() {
		t.Fatal("no saved sign-in")
	}
	if _, err := c.SearchNotes(ctx, "x", 0, 1); err != nil {
		t.Fatal(err)
	}
	if registered.Load() != 1 || tokens.Load() != 1 {
		t.Fatalf("signed in again: registered %d, tokens %d", registered.Load(), tokens.Load())
	}
	if err := c.SignOut(); err != nil {
		t.Fatal(err)
	}
	if s, _ := store.Load(); s != nil {
		t.Fatal("sign-out kept the token")
	}
}
