package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/graphaelli/zat/google"
	googlemock "github.com/graphaelli/zat/google/mock"
	"github.com/graphaelli/zat/meet"
	meetmock "github.com/graphaelli/zat/meet/mock"
	"github.com/graphaelli/zat/zoom"
	zoommock "github.com/graphaelli/zat/zoom/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	nopGoogleClient = &google.Client{}
	nopZoomClient   = &zoom.Client{}
	nopMeetClient   = &meet.Client{}
	rp              = runParams{
		minDuration: 5,
		since:       24 * time.Hour,
	}
)

func TestGoogleOauth(t *testing.T) {
	// mock google APIs and OAuth endpoints
	googleMux := http.NewServeMux()
	// googleMux.HandleFunc("/api/", googlemock.ApiHandler(t))
	googleServer := httptest.NewServer(googleMux)
	defer googleServer.Close()

	var muxBuf bytes.Buffer
	var googleBuf bytes.Buffer
	muxLog := log.New(&muxBuf, "[mux] ", 0)
	googleLog := log.New(&googleBuf, "[google] ", 0)
	googleConfig := &oauth2.Config{
		ClientID:     "test-id",
		ClientSecret: "test-secret",
		RedirectURL:  "tbd",
		Endpoint: oauth2.Endpoint{
			AuthURL:   googleServer.URL + "/oauth2/auth",
			TokenURL:  googleServer.URL + "/oauth2/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
	googleClient, err := google.NewClient(googleLog, googleConfig, google.CustomHTTPClientOption(googleServer.Client()))
	if err != nil {
		t.Error(err)
	}

	zat := &Config{
		logger:       muxLog,
		copies:       map[int64]Directive{},
		googleClient: googleClient,
		zoomClient:   nopZoomClient,
	}

	mux := NewMux(zat, rp)
	server := httptest.NewServer(mux)
	defer server.Close()

	// now that server has a URL, configure oauth redirect sender and handler
	oauthRedirect := server.URL + "/oauth/google"
	googleConfig.RedirectURL = oauthRedirect
	googleMux.HandleFunc("/oauth2/", googlemock.OauthHandler(t, oauthRedirect))

	client := server.Client()
	var redirects []*url.URL
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		redirects = append(redirects, req.URL)
		if len(via) >= 5 {
			return errors.New("stopped after 5 redirects")
		}
		return nil
	}
	rsp, err := client.Get(oauthRedirect)
	if err != nil {
		t.Error(err)
	}
	t.Log(strings.TrimSpace(muxBuf.String()))
	t.Log(strings.TrimSpace(googleBuf.String()))
	if rsp.StatusCode != http.StatusOK {
		t.Errorf("expected %d, got %d HTTP response", http.StatusOK, rsp.StatusCode)
	}

	expectedRedirects := []string{
		googleConfig.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.ApprovalForce),
		oauthRedirect + "?code=acode",
		server.URL + "/",
	}

	if len(redirects) != len(expectedRedirects) {
		t.Errorf("expected %d, got %d redirects", len(expectedRedirects), len(redirects))

	}
	for i, redirect := range redirects {
		e, err := url.Parse(expectedRedirects[i])
		if err != nil {
			t.Error(err)
		}
		if redirect.Path != e.Path || redirect.Query().Encode() != e.Query().Encode() {
			t.Errorf("expected %s, got %s in flow", e.String(), redirect.String())
		}
	}
}

func TestZoomOauth(t *testing.T) {
	// mock zoom APIs and OAuth endpoints
	zoomMux := http.NewServeMux()
	zoomMux.HandleFunc("/api/", zoommock.ApiHandler(t))
	zoomServer := httptest.NewServer(zoomMux)
	defer zoomServer.Close()

	var muxBuf bytes.Buffer
	var zoomBuf bytes.Buffer
	muxLog := log.New(&muxBuf, "[mux] ", 0)
	zoomLog := log.New(&zoomBuf, "[zoom] ", 0)
	zoomConfig := zoom.Config{
		Id:            "test-id",
		Secret:        "test-secret",
		OauthRedirect: "tbd",
		ApiBaseUrl:    zoomServer.URL + "/api",
		AuthUrl:       zoomServer.URL + "/oauth/authorize",
		TokenUrl:      zoomServer.URL + "/oauth/token",
	}
	zoomClient, err := zoom.NewClient(zoomLog, zoomConfig, zoom.CustomHTTPClientOption(zoomServer.Client()))
	if err != nil {
		t.Error(err)
	}

	zat := &Config{
		logger:       muxLog,
		copies:       map[int64]Directive{},
		googleClient: nopGoogleClient,
		zoomClient:   zoomClient,
	}

	mux := NewMux(zat, rp)
	server := httptest.NewServer(mux)
	defer server.Close()

	// now that server has a URL, configure oauth redirect sender and handler
	oauthRedirect := server.URL + "/oauth/zoom"
	zoomClient.UpdateOauthRedirect(oauthRedirect)
	zoomMux.HandleFunc("/oauth/", zoommock.OauthHandler(t, oauthRedirect))

	client := server.Client()
	var redirects []*url.URL
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		redirects = append(redirects, req.URL)
		if len(via) >= 5 {
			return errors.New("stopped after 5 redirects")
		}
		return nil
	}
	rsp, err := client.Get(oauthRedirect)
	if err != nil {
		t.Error(err)
	}
	t.Log(strings.TrimSpace(muxBuf.String()))
	t.Log(strings.TrimSpace(zoomBuf.String()))
	if rsp.StatusCode != http.StatusOK {
		t.Errorf("expected %d, got %d HTTP response", http.StatusOK, rsp.StatusCode)
	}

	expectedRedirects := []string{
		zoomConfig.AuthUrl + fmt.Sprintf("?access_type=offline&state=state-token&client_id=test-id&redirect_uri=%s&response_type=code", url.QueryEscape(oauthRedirect)),
		oauthRedirect + "?code=acode",
		server.URL + "/",
	}

	if len(redirects) != len(expectedRedirects) {
		t.Errorf("expected %d, got %d redirects", len(expectedRedirects), len(redirects))

	}
	for i, redirect := range redirects {
		e, err := url.Parse(expectedRedirects[i])
		if err != nil {
			t.Error(err)
		}
		if redirect.Path != e.Path || redirect.Query().Encode() != e.Query().Encode() {
			t.Errorf("expected %s, got %s in flow", e.String(), redirect.String())
		}
	}
}

func TestRecordingFileName(t *testing.T) {
	start := "2019-06-12T13:00:00Z"
	end := "2019-06-12T13:57:54Z"
	meeting := zoom.Meeting{
		Topic: "Some Meeting",
	}
	type args struct {
		meeting   zoom.Meeting
		recording zoom.RecordingFile
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "audio only",
			args: args{
				meeting: meeting,
				recording: zoom.RecordingFile{
					RecordingStart: start,
					RecordingEnd:   end,
					FileType:       "M4A",
					RecordingType:  "audio_only",
				},
			},
			want: "2019-06-12-130000 Some Meeting.m4a",
		},
		{
			name: "chat log",
			args: args{
				meeting: meeting,
				recording: zoom.RecordingFile{
					RecordingStart: start,
					RecordingEnd:   end,
					FileType:       "CHAT",
					RecordingType:  "chat_file",
				},
			},
			want: "2019-06-12-130000 Some Meeting.chat.log",
		},
		{
			name: "timeline",
			args: args{
				meeting: meeting,
				recording: zoom.RecordingFile{
					RecordingStart: start,
					RecordingEnd:   end,
					FileType:       "TIMELINE",
				},
			},
			want: "2019-06-12-130000 Some Meeting.timeline.json",
		},
		{
			name: "transcript",
			args: args{
				meeting: meeting,
				recording: zoom.RecordingFile{
					RecordingStart: start,
					RecordingEnd:   end,
					FileType:       "TRANSCRIPT",
					RecordingType:  "audio_transcript",
				},
			},
			want: "2019-06-12-130000 Some Meeting.vtt",
		},
		{
			name: "video",
			args: args{
				meeting: meeting,
				recording: zoom.RecordingFile{
					RecordingStart: start,
					RecordingEnd:   end,
					FileType:       "MP4",
					RecordingType:  "shared_screen_with_speaker_view",
				},
			},
			want: "2019-06-12-130000 Some Meeting.mp4",
		},
		{
			name: "unknown",
			args: args{
				meeting: meeting,
				recording: zoom.RecordingFile{
					RecordingStart: start,
					RecordingEnd:   end,
					FileType:       "FOO",
					RecordingType:  "bar",
				},
			},
			want: "2019-06-12-130000 Some Meeting.bar.foo",
		},
		{
			name: "missing",
			args: args{
				meeting: meeting,
				recording: zoom.RecordingFile{
					RecordingStart: start,
					RecordingEnd:   end,
					FileType:       "FOO",
				},
			},
			want: "2019-06-12-130000 Some Meeting.foo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := recordingFileName(tt.args.meeting, tt.args.recording); got != tt.want {
				t.Errorf("recordingFileName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigFromFile(t *testing.T) {
	c, err := NewConfigFromFile(nil, "does-not-exist", nopGoogleClient, nopZoomClient, nopMeetClient, nil)
	require.NoError(t, err)
	assert.NotNil(t, c)
}

func TestMeetRecordingFileName(t *testing.T) {
	start := time.Date(2026, 5, 20, 13, 0, 0, 0, time.UTC)
	conf := meet.Conference{StartTime: start}
	action := Directive{Name: "UI Weekly"}

	tests := []struct {
		name     string
		artifact meet.Artifact
		want     string
	}{
		{
			name:     "recording",
			artifact: meet.Artifact{Kind: "recording", StartTime: start},
			want:     "2026-05-20-130000 UI Weekly.mp4",
		},
		{
			name:     "transcript",
			artifact: meet.Artifact{Kind: "transcript", StartTime: start},
			want:     "2026-05-20-130000 UI Weekly.transcript",
		},
		{
			name:     "artifact without start falls back to conference start",
			artifact: meet.Artifact{Kind: "recording"},
			want:     "2026-05-20-130000 UI Weekly.mp4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := meetRecordingFileName(action, conf, tt.artifact); got != tt.want {
				t.Errorf("meetRecordingFileName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigMeetMapping(t *testing.T) {
	yml := `
- name: UI Weekly
  google: folderA
  meet: abc-defg-hij
- name: Dup
  google: folderB
  meet: ABCDEFGHIJ
- name: Team Weekly
  google: folderC
  meet: zzz-yyyy-xxx
`
	var clog bytes.Buffer
	c, err := NewConfigFromReader(log.New(&clog, "", 0), strings.NewReader(yml),
		nopGoogleClient, nopZoomClient, nopMeetClient, nil)
	require.NoError(t, err)

	// "abc-defg-hij" and "ABCDEFGHIJ" normalize to the same key -> skipDirective
	dup := c.meetCopies[normalizeMeetingCode("abc-defg-hij")]
	assert.Equal(t, skipDirective, dup)

	// distinct code maps normally
	ok := c.meetCopies[normalizeMeetingCode("zzz-yyyy-xxx")]
	assert.Equal(t, "folderC", ok.Google)
}

func TestMeetConfigured(t *testing.T) {
	zoomOnly, err := decodeDirectives(strings.NewReader("- name: Z\n  google: f\n  zoom: 123-456-789\n"))
	require.NoError(t, err)
	assert.False(t, meetConfigured(zoomOnly), "zoom-only config should not enable Meet")

	withMeet, err := decodeDirectives(strings.NewReader("- name: M\n  google: f\n  meet: abc-defg-hij\n"))
	require.NoError(t, err)
	assert.True(t, meetConfigured(withMeet), "config with a meet directive should enable Meet")

	assert.False(t, meetConfigured(nil), "no directives should not enable Meet")
}

func TestLoadDirectivesMissingFile(t *testing.T) {
	directives, err := loadDirectives(filepath.Join(t.TempDir(), "nope.yml"))
	require.NoError(t, err, "a missing config file should not be an error")
	assert.Empty(t, directives)
}

// TestMuxMeetOnly verifies the web UI does not panic when zoomClient is nil
// (a Meet-only install with no zoom config) and that /zoom 404s.
func TestMuxMeetOnly(t *testing.T) {
	var clog bytes.Buffer
	zat := &Config{
		logger:       log.New(&clog, "", 0),
		copies:       map[int64]Directive{},
		meetCopies:   map[string]Directive{"abcdefghij": {Name: "UI Weekly", Google: "folderA", Meet: "abc-defg-hij"}},
		googleClient: nopGoogleClient,
		zoomClient:   nil,
		meetClient:   nil,
	}
	server := httptest.NewServer(NewMux(zat, rp))
	defer server.Close()

	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	rsp, err := client.Get(server.URL + "/")
	require.NoError(t, err)
	defer rsp.Body.Close()
	assert.Equal(t, http.StatusOK, rsp.StatusCode, "/ should render without a zoom client")

	zrsp, err := client.Get(server.URL + "/zoom")
	require.NoError(t, err)
	defer zrsp.Body.Close()
	assert.Equal(t, http.StatusNotFound, zrsp.StatusCode, "/zoom should 404 when zoom is disabled")
}

// loggedInGoogleClient builds a google.Client whose HasCreds() is true by
// loading a non-expired token from a temp creds file.
func loggedInGoogleClient(t *testing.T) *google.Client {
	t.Helper()
	credsPath := filepath.Join(t.TempDir(), "google.creds.json")
	b, err := json.Marshal(oauth2.Token{AccessToken: "test-token", Expiry: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(credsPath, b, 0600))

	var clog bytes.Buffer
	gc, err := google.NewClient(log.New(&clog, "", 0),
		&oauth2.Config{RedirectURL: "http://localhost:8080/oauth/google"},
		google.NewCredentialsManager(credsPath).ClientOption)
	require.NoError(t, err)
	require.True(t, gc.HasCreds(), "expected loaded creds to be valid")
	return gc
}

func meetClientAt(t *testing.T, baseURL string, hc *http.Client) *meet.Client {
	t.Helper()
	var mlog bytes.Buffer
	mc, err := meet.NewClient(log.New(&mlog, "", 0),
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}),
		meet.CustomHTTPClientOption(hc), meet.CustomBaseURLOption(baseURL))
	require.NoError(t, err)
	return mc
}

func TestMuxMeet(t *testing.T) {
	noRedirect := func(c *http.Client) { c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse } }

	t.Run("503 when meet client not configured", func(t *testing.T) {
		var clog bytes.Buffer
		zat := &Config{logger: log.New(&clog, "", 0), googleClient: loggedInGoogleClient(t), zoomClient: nil, meetClient: nil}
		srv := httptest.NewServer(NewMux(zat, rp))
		defer srv.Close()

		rsp, err := srv.Client().Get(srv.URL + "/meet")
		require.NoError(t, err)
		defer rsp.Body.Close()
		assert.Equal(t, http.StatusServiceUnavailable, rsp.StatusCode)
	})

	t.Run("redirects to login when google creds missing", func(t *testing.T) {
		var clog bytes.Buffer
		gc, err := google.NewClient(log.New(&clog, "", 0), &oauth2.Config{RedirectURL: "http://localhost:8080/oauth/google"})
		require.NoError(t, err)
		meetSrv := httptest.NewServer(meetmock.ApiHandler(t))
		defer meetSrv.Close()
		zat := &Config{logger: log.New(&clog, "", 0), googleClient: gc, zoomClient: nil,
			meetClient: meetClientAt(t, meetSrv.URL, meetSrv.Client())}
		srv := httptest.NewServer(NewMux(zat, rp))
		defer srv.Close()

		client := srv.Client()
		noRedirect(client)
		rsp, err := client.Get(srv.URL + "/meet")
		require.NoError(t, err)
		defer rsp.Body.Close()
		assert.Equal(t, http.StatusFound, rsp.StatusCode)
		assert.Equal(t, "http://localhost:8080/oauth/google", rsp.Header.Get("Location"))
	})

	t.Run("returns JSON conferences when authed", func(t *testing.T) {
		meetSrv := httptest.NewServer(meetmock.ApiHandler(t))
		defer meetSrv.Close()
		var clog bytes.Buffer
		zat := &Config{logger: log.New(&clog, "", 0), googleClient: loggedInGoogleClient(t), zoomClient: nil,
			meetClient: meetClientAt(t, meetSrv.URL, meetSrv.Client())}
		srv := httptest.NewServer(NewMux(zat, rp))
		defer srv.Close()

		rsp, err := srv.Client().Get(srv.URL + "/meet")
		require.NoError(t, err)
		defer rsp.Body.Close()
		assert.Equal(t, http.StatusOK, rsp.StatusCode)
		assert.Equal(t, "application/json", rsp.Header.Get("Content-Type"))
		var confs []meet.Conference
		require.NoError(t, json.NewDecoder(rsp.Body).Decode(&confs))
		assert.Len(t, confs, 1)
	})

	t.Run("500 when ListConferences errors", func(t *testing.T) {
		meetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer meetSrv.Close()
		var clog bytes.Buffer
		zat := &Config{logger: log.New(&clog, "", 0), googleClient: loggedInGoogleClient(t), zoomClient: nil,
			meetClient: meetClientAt(t, meetSrv.URL, meetSrv.Client())}
		srv := httptest.NewServer(NewMux(zat, rp))
		defer srv.Close()

		rsp, err := srv.Client().Get(srv.URL + "/meet")
		require.NoError(t, err)
		defer rsp.Body.Close()
		assert.Equal(t, http.StatusInternalServerError, rsp.StatusCode)
	})
}
