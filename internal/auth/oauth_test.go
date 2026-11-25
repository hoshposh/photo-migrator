package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"golang.org/x/oauth2"
)

// fakeGoogle is a stand-in token endpoint that records what it received.
type fakeGoogle struct {
	mu       sync.Mutex
	calls    int
	verifier string
}

func (f *fakeGoogle) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("token endpoint: parse form: %v", err)
		}
		f.mu.Lock()
		f.calls++
		f.verifier = r.PostForm.Get("code_verifier")
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "refresh_token": "rt", "expires_in": 3600,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runFlow runs runLoopbackFlow with a fake browser. The browser receives the
// authorization URL and must return the query to send to the redirect URI.
// It returns the token, the parsed authorization URL, and the flow error.
func runFlow(t *testing.T, tokenURL string, browser func(auth url.Values) url.Values) (*oauth2.Token, url.Values, error) {
	t.Helper()
	cfg := &oauth2.Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		Endpoint:     oauth2.Endpoint{AuthURL: "https://accounts.example/auth", TokenURL: tokenURL},
	}

	var authQuery url.Values
	orig := openBrowser
	openBrowser = func(rawURL string) {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Errorf("parse auth URL: %v", err)
			return
		}
		authQuery = u.Query()
		cb, err := url.Parse(authQuery.Get("redirect_uri"))
		if err != nil {
			t.Errorf("parse redirect_uri: %v", err)
			return
		}
		cb.RawQuery = browser(authQuery).Encode()
		go func() {
			resp, err := http.Get(cb.String())
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	t.Cleanup(func() { openBrowser = orig })

	tok, err := runLoopbackFlow(context.Background(), cfg)
	return tok, authQuery, err
}

// honestBrowser echoes back the state Google would return, plus a code.
func honestBrowser(auth url.Values) url.Values {
	return url.Values{"code": {"auth-code"}, "state": {auth.Get("state")}}
}

// Break caught: authorization code exchanged without PKCE, or with a
// verifier that does not match the challenge sent to Google.
func TestLoopbackFlow_UsesPKCE(t *testing.T) {
	g := &fakeGoogle{}
	srv := g.server(t)

	tok, auth, err := runFlow(t, srv.URL, honestBrowser)
	if err != nil {
		t.Fatalf("flow: %v", err)
	}
	if tok.AccessToken != "at" {
		t.Fatalf("access token = %q, want \"at\"", tok.AccessToken)
	}

	if got := auth.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want \"S256\"", got)
	}
	g.mu.Lock()
	verifier := g.verifier
	g.mu.Unlock()
	if verifier == "" {
		t.Fatal("token exchange sent no code_verifier")
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); auth.Get("code_challenge") != want {
		t.Errorf("code_challenge %q does not match S256(code_verifier) %q", auth.Get("code_challenge"), want)
	}
}

// Break caught: the callback accepting a redirect whose state does not match
// the one this flow issued (login CSRF).
func TestLoopbackFlow_RejectsMismatchedState(t *testing.T) {
	g := &fakeGoogle{}
	srv := g.server(t)

	_, _, err := runFlow(t, srv.URL, func(url.Values) url.Values {
		return url.Values{"code": {"attacker-code"}, "state": {"forged"}}
	})
	if err == nil {
		t.Fatal("flow accepted a callback with a forged state")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.calls != 0 {
		t.Errorf("token endpoint called %d times; code must not be exchanged on state mismatch", g.calls)
	}
}

// Break caught: a fixed or predictable state value, which an attacker could
// copy into a forged callback.
func TestLoopbackFlow_StateIsUnpredictable(t *testing.T) {
	srv := (&fakeGoogle{}).server(t)

	seen := map[string]bool{}
	for range 2 {
		_, auth, err := runFlow(t, srv.URL, honestBrowser)
		if err != nil {
			t.Fatalf("flow: %v", err)
		}
		s := auth.Get("state")
		if len(s) < 16 {
			t.Errorf("state %q is too short to be unguessable", s)
		}
		if seen[s] {
			t.Errorf("state %q repeated across flows", s)
		}
		seen[s] = true
	}
}
