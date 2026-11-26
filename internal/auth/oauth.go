// Package auth implements OAuth2 authentication for the Google Photos API
// using a loopback redirect flow suitable for desktop CLI applications.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Scopes requested from the Google Photos Library API.
// The legacy "photoslibrary" scope is deprecated; the API now requires
// granular scopes:
//   - appendonly: upload new media items
//   - edit.appcreateddata: create albums and add app-uploaded items to them
var Scopes = []string{
	"https://www.googleapis.com/auth/photoslibrary.appendonly",
	"https://www.googleapis.com/auth/photoslibrary.edit.appcreateddata",
}

// LoadConfig reads an OAuth2 credentials.json file downloaded from the
// Google Cloud Console and returns a configured *oauth2.Config.
func LoadConfig(credentialsPath string) (*oauth2.Config, error) {
	data, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", credentialsPath, err)
	}
	cfg, err := google.ConfigFromJSON(data, Scopes...)
	if err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	return cfg, nil
}

// GetClient returns an authenticated *http.Client for the Google Photos API.
//
// It uses the token cached at ~/.config/photo-migrator/token.json, refreshing
// it automatically when it expires. If there is no cached token it signs in
// first (see Login).
func GetClient(ctx context.Context, cfg *oauth2.Config) (*http.Client, error) {
	tokenPath, err := tokenCachePath()
	if err != nil {
		return nil, err
	}
	if tok, err := loadToken(tokenPath); err == nil {
		return cfg.Client(ctx, tok), nil
	}
	return Login(ctx, cfg)
}

// Login always runs the browser sign-in, ignoring any cached token, and
// replaces token.json with the result. This is what `photo-migrator auth`
// does, so re-running it fixes a token Google has rejected (for example the
// 7-day expiry for apps in Testing mode).
func Login(ctx context.Context, cfg *oauth2.Config) (*http.Client, error) {
	tokenPath, err := tokenCachePath()
	if err != nil {
		return nil, err
	}
	tok, err := runLoopbackFlow(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := saveToken(tokenPath, tok); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not cache token: %v\n", err)
	} else {
		fmt.Printf("Token cached to %s\n", tokenPath)
	}
	return cfg.Client(ctx, tok), nil
}

// runLoopbackFlow starts an ephemeral local HTTP server, opens the system
// browser to the Google authorization page, and waits (up to 3 min) for the
// redirect containing the authorization code.
func runLoopbackFlow(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	// state binds the callback to this flow (prevents login CSRF); the PKCE
	// verifier binds the code exchange to this process, so an intercepted
	// authorization code is useless on its own.
	state := rand.Text()
	verifier := oauth2.GenerateVerifier()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
				select {
				case errCh <- fmt.Errorf("authorization failed: state mismatch (possible forged redirect)"):
				default:
				}
				http.Error(w, "authorization failed: state mismatch", http.StatusBadRequest)
				return
			}
			code := q.Get("code")
			if code == "" {
				select {
				case errCh <- fmt.Errorf("authorization failed: %s", r.URL.RawQuery):
				default:
				}
				http.Error(w, "authorization failed", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintln(w, `<html><body style="font-family:sans-serif;padding:2em">
				<h2>✓ Authorization complete</h2>
				<p>You may close this tab and return to the terminal.</p>
			</body></html>`)
			select {
			case codeCh <- code:
			default: // a code was already received; ignore repeats
			}
		}),
	}

	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			select {
			case errCh <- err:
			default:
			}
		}
	}()
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier))
	fmt.Println("\nOpening browser for Google Photos authorization (Account B)...")
	fmt.Println("If the browser does not open automatically, visit this URL:")
	fmt.Println(" ", authURL)
	openBrowser(authURL)

	flowCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	select {
	case code := <-codeCh:
		tok, err := cfg.Exchange(flowCtx, code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("exchange code: %w", err)
		}
		return tok, nil
	case err := <-errCh:
		return nil, err
	case <-flowCtx.Done():
		return nil, fmt.Errorf("authorization timed out after 3 minutes")
	}
}

// openBrowser opens url in the default system browser. It is a variable so
// tests can stand in for the browser.
var openBrowser = func(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "cmd", []string{"/c", "start", url}
	default: // Linux / WSL
		cmd, args = "xdg-open", []string{url}
	}
	exec.Command(cmd, args...).Start() //nolint:errcheck
}

// tokenCachePath returns the path to the cached OAuth2 token file.
func tokenCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "photo-migrator")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create token dir: %w", err)
	}
	return filepath.Join(dir, "token.json"), nil
}

func loadToken(path string) (_ *oauth2.Token, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	var tok oauth2.Token
	if err := json.NewDecoder(f).Decode(&tok); err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}
	return &tok, nil
}

func saveToken(path string, tok *oauth2.Token) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	return json.NewEncoder(f).Encode(tok)
}
