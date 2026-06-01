package google

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestNewClientFromReader(t *testing.T) {
	tests := []struct {
		name     string
		config   *Config
		validate func(*testing.T, *Client, error)
		wantErr  bool
	}{
		{
			name:   "Nil",
			config: nil,
			validate: func(t *testing.T, c *Client, err error) {
				if err == nil {
					t.Error("expected error from nil config")
				}
			},
		},
		{
			name:   "Empty",
			config: &Config{},
			validate: func(t *testing.T, c *Client, err error) {
				if err == nil {
					t.Error("expected error from empty config")
				}
			},
		},
		{
			name: "Incomplete",
			config: &Config{
				ClientID: "test-id",
			},
			validate: func(t *testing.T, c *Client, err error) {
				if err == nil {
					t.Error("expected error from incomplete config")
				}
			},
		},
		{
			name: "Minimal",
			config: &Config{
				ClientID:     "test-id",
				ClientSecret: "test-secret",
				RedirectURIs: []string{"http://redirect"},
				AuthURI:      "http://auth",
				TokenURI:     "http://token",
			},
			validate: func(t *testing.T, c *Client, err error) {
				if err != nil {
					t.Error("expected no error from minimal config, got:", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var config bytes.Buffer

			// google's creds are nested under installed or web
			webConfig := struct {
				Web *Config `json:"web"`
			}{Web: tt.config}

			if err := json.NewEncoder(&config).Encode(webConfig); err != nil {
				t.Error(err)
			}
			var clog bytes.Buffer
			got, err := NewClientFromReader(log.New(&clog, "", 0), &config)
			tt.validate(t, got, err)
		})
	}
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// TestDefaultScopesExcludeMeet verifies Meet is not requested by default, so
// Drive/Zoom-only users aren't prompted for Meet permissions.
func TestDefaultScopesExcludeMeet(t *testing.T) {
	cfg := []byte(`{"web":{"client_id":"id","client_secret":"secret","redirect_uris":["http://r"],"auth_uri":"http://a","token_uri":"http://t"}}`)
	var clog bytes.Buffer
	c, err := NewClientFromReader(log.New(&clog, "", 0), bytes.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if hasScope(c.config.Scopes, MeetScope) {
		t.Errorf("did not expect meet scope by default, got %v", c.config.Scopes)
	}
}

// TestWithMeetScope verifies the opt-in option adds the Meet scope.
func TestWithMeetScope(t *testing.T) {
	cfg := []byte(`{"web":{"client_id":"id","client_secret":"secret","redirect_uris":["http://r"],"auth_uri":"http://a","token_uri":"http://t"}}`)
	var clog bytes.Buffer
	c, err := NewClientFromReader(log.New(&clog, "", 0), bytes.NewReader(cfg), WithMeetScope())
	if err != nil {
		t.Fatal(err)
	}
	if !hasScope(c.config.Scopes, MeetScope) {
		t.Errorf("expected meet scope with WithMeetScope(), got %v", c.config.Scopes)
	}
}

func TestTokenSource(t *testing.T) {
	cfg := []byte(`{"web":{"client_id":"id","client_secret":"secret","redirect_uris":["http://r"],"auth_uri":"http://a","token_uri":"http://t"}}`)
	var clog bytes.Buffer
	c, err := NewClientFromReader(log.New(&clog, "", 0), bytes.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}
	c.credentials = &oauth2.Token{AccessToken: "x"}
	if c.TokenSource(context.Background()) == nil {
		t.Error("expected non-nil token source")
	}
}

// TestTokenSourceLazy verifies a source handed out before login (no creds)
// errors cleanly rather than panicking, and then resolves credentials that
// arrive afterward - the web-login-first flow.
func TestTokenSourceLazy(t *testing.T) {
	cfg := []byte(`{"web":{"client_id":"id","client_secret":"secret","redirect_uris":["http://r"],"auth_uri":"http://a","token_uri":"http://t"}}`)
	var clog bytes.Buffer
	c, err := NewClientFromReader(log.New(&clog, "", 0), bytes.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}

	ts := c.TokenSource(context.Background()) // built before any creds exist
	if _, err := ts.Token(); err == nil {
		t.Error("expected error from token source before credentials exist")
	}

	// credentials arrive later (e.g. via the OAuth web callback)
	c.credentials = &oauth2.Token{AccessToken: "live-token", Expiry: time.Now().Add(time.Hour)}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("expected token after creds arrive, got error: %v", err)
	}
	if tok.AccessToken != "live-token" {
		t.Errorf("expected live-token, got %q", tok.AccessToken)
	}
}
