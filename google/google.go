package google

import (
	"context"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

const (
	MimeTypeFolder = "application/vnd.google-apps.folder"
)

type Client struct {
	logger     *log.Logger
	httpClient *http.Client

	config *oauth2.Config
	// credsMu guards credentials, which is read by background archival
	// (via TokenSource/Service) while the OAuth handler writes it.
	credsMu     sync.RWMutex
	credentials *oauth2.Token
	cm          *credentialsManager
}

// getCreds returns the current credentials under a read lock.
func (c *Client) getCreds() *oauth2.Token {
	c.credsMu.RLock()
	defer c.credsMu.RUnlock()
	return c.credentials
}

type ClientOption func(*Client)

func CustomHTTPClientOption(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = httpClient
	}
}

// MeetScope is the read-only Google Meet scope needed to discover conference
// recordings and transcripts.
const MeetScope = "https://www.googleapis.com/auth/meetings.space.readonly"

// WithMeetScope requests the Google Meet read scope in addition to Drive. It is
// opt-in so Drive/Zoom-only users aren't prompted for Meet permissions.
func WithMeetScope() ClientOption {
	return func(c *Client) {
		c.config.Scopes = append(c.config.Scopes, MeetScope)
	}
}

// Config is a google-defined client_credentials.json format.
// Duplicated from golang.org/x/oauth2/google since it is not exported.
type Config struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	RedirectURIs []string `json:"redirect_uris"`
	AuthURI      string   `json:"auth_uri"`
	TokenURI     string   `json:"token_uri"`
}

func NewClientFromFile(logger *log.Logger, path string, options ...ClientOption) (*Client, error) {
	// If modifying these scopes, delete your previously saved token.json.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return NewClientFromReader(logger, f, options...)
}

func NewClientFromReader(logger *log.Logger, r io.Reader, options ...ClientOption) (*Client, error) {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	// https://developers.google.com/identity/protocols/googlescopes#drivev3
	// Drive scope for archival; the Meet scope is opt-in via WithMeetScope.
	config, err := google.ConfigFromJSON(b, drive.DriveScope)
	if err != nil {
		return nil, err
	}
	return NewClient(logger, config, options...)
}

func NewClient(logger *log.Logger, config *oauth2.Config, options ...ClientOption) (*Client, error) {
	c := &Client{
		logger:     logger,
		httpClient: http.DefaultClient,
		config:     config,
	}

	for _, o := range options {
		o(c)
	}
	return c, nil
}

func (c *Client) updateCreds(token *oauth2.Token) {
	c.credsMu.Lock()
	defer c.credsMu.Unlock()
	c.credentials = token
	if token != nil && c.cm != nil {
		// saveCreds reads c.credentials, which we hold the write lock over.
		if err := c.cm.saveCreds(c); err != nil {
			c.logger.Println("failed to save creds:", err)
		}
	}
}

func (c *Client) HasCreds() bool {
	creds := c.getCreds()
	if creds == nil {
		return false
	}

	if creds.Valid() {
		return true
	}

	c.logger.Println("Google credentials not valid, updating token")
	src := c.config.TokenSource(context.TODO(), creds)
	newToken, err := src.Token() // this actually goes and renews the tokens
	if err != nil {
		c.logger.Printf("error updating google token %s", err)
		return false
	}
	if newToken.AccessToken != creds.AccessToken {
		c.updateCreds(newToken)
		c.logger.Println("Google credentials updated and saved to disk")
	}
	return true
}

func (c *Client) OauthRedirect(w http.ResponseWriter, r *http.Request) {
	c.logger.Println(c.config.RedirectURL)
	http.Redirect(w, r, c.config.RedirectURL, http.StatusFound)
}

// TODO: use / validate state token
func (c *Client) OauthHandler() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("refresh") != "" || !c.getCreds().Valid() {
			c.updateCreds(nil)
		}

		code := r.FormValue("code")
		if code == "" {
			redirectTo := c.config.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
			c.logger.Print("no code provided, sending to google auth ", redirectTo)
			http.Redirect(w, r, redirectTo, http.StatusFound)
			return
		}

		// exchange for token
		if token, err := c.config.Exchange(context.TODO(), code); err != nil {
			c.logger.Print(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else {
			c.updateCreds(token)
		}

		http.Redirect(w, r, "/", http.StatusFound)
	}
}

// lazyTokenSource reads the client's credentials fresh on every Token() call,
// so a source handed out before the OAuth web login still works once creds
// arrive (and after they're refreshed). It also fails cleanly - rather than
// returning a source stuck on a nil token - when creds are not yet present.
type lazyTokenSource struct {
	c   *Client
	ctx context.Context
}

func (l lazyTokenSource) Token() (*oauth2.Token, error) {
	creds := l.c.getCreds()
	if creds == nil {
		return nil, errors.New("google: no credentials yet, login required")
	}
	return l.c.config.TokenSource(l.ctx, creds).Token()
}

// TokenSource exposes an OAuth token source so other Google APIs (e.g. Meet)
// can authenticate with the same credentials. The source resolves credentials
// lazily, so it remains valid across a later web login or token refresh.
func (c *Client) TokenSource(ctx context.Context) oauth2.TokenSource {
	return lazyTokenSource{c: c, ctx: ctx}
}

func (c *Client) Service(ctx context.Context) (*drive.Service, error) {
	return drive.NewService(ctx, option.WithTokenSource(c.config.TokenSource(ctx, c.getCreds())))
}

func (c *Client) ListFiles(ctx context.Context, q string, pageToken string) (*drive.FileList, error) {
	driveService, err := c.Service(ctx)
	if err != nil {
		return nil, err
	}

	l := driveService.Files.List().SupportsTeamDrives(true).IncludeTeamDriveItems(true).Q(q)
	if pageToken != "" {
		l.PageToken(pageToken)
	}
	return l.Do()
}
