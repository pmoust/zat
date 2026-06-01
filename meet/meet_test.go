package meet

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	meetmock "github.com/graphaelli/zat/meet/mock"
)

func staticTS() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-access-token"})
}

func TestNewClient(t *testing.T) {
	var clog bytes.Buffer
	c, err := NewClient(log.New(&clog, "", 0), staticTS())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClientNilTokenSource(t *testing.T) {
	var clog bytes.Buffer
	if _, err := NewClient(log.New(&clog, "", 0), nil); err == nil {
		t.Fatal("expected error from nil token source")
	}
}

func TestDoDecodesAndAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{"hello": "world"})
	}))
	defer srv.Close()

	var clog bytes.Buffer
	c, err := NewClient(log.New(&clog, "", 0), staticTS(),
		CustomHTTPClientOption(srv.Client()), CustomBaseURLOption(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	var dst map[string]string
	if err := c.getJSON(context.Background(), "/v2/anything", nil, &dst); err != nil {
		t.Fatalf("getJSON error: %v", err)
	}
	if dst["hello"] != "world" {
		t.Errorf("expected decoded body, got %v", dst)
	}
	if gotAuth != "Bearer test-access-token" {
		t.Errorf("expected bearer auth header, got %q", gotAuth)
	}
}

func TestDoErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var clog bytes.Buffer
	c, _ := NewClient(log.New(&clog, "", 0), staticTS(),
		CustomHTTPClientOption(srv.Client()), CustomBaseURLOption(srv.URL))
	var dst map[string]string
	if err := c.getJSON(context.Background(), "/v2/anything", nil, &dst); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestListConferences(t *testing.T) {
	srv := httptest.NewServer(meetmock.ApiHandler(t))
	defer srv.Close()

	var clog bytes.Buffer
	c, _ := NewClient(log.New(&clog, "", 0), staticTS(),
		CustomHTTPClientOption(srv.Client()), CustomBaseURLOption(srv.URL))

	confs, err := c.ListConferences(context.Background(), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListConferences error: %v", err)
	}
	if len(confs) != 1 {
		t.Fatalf("expected 1 conference, got %d", len(confs))
	}
	conf := confs[0]
	if conf.MeetingCode != "abc-defg-hij" {
		t.Errorf("expected meetingCode abc-defg-hij, got %q", conf.MeetingCode)
	}
	if conf.SourceURL != "https://meet.google.com/abc-defg-hij" {
		t.Errorf("unexpected SourceURL %q", conf.SourceURL)
	}
	if len(conf.Artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(conf.Artifacts))
	}
	var rec, tr int
	for _, a := range conf.Artifacts {
		switch a.Kind {
		case "recording":
			rec++
			if a.DriveFileID != "driveFileRec1" {
				t.Errorf("unexpected recording file id %q", a.DriveFileID)
			}
		case "transcript":
			tr++
			if a.DriveFileID != "docFileTr1" {
				t.Errorf("unexpected transcript file id %q", a.DriveFileID)
			}
		}
	}
	if rec != 1 || tr != 1 {
		t.Errorf("expected 1 recording and 1 transcript, got %d/%d", rec, tr)
	}
}

// TestListConferencesPaginates verifies the pagination loops across all three
// list endpoints, and that the start_time filter is sent.
func TestListConferencesPaginates(t *testing.T) {
	var sawFilter string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token := r.URL.Query().Get("pageToken")
		switch r.URL.Path {
		case "/v2/conferenceRecords":
			sawFilter = r.URL.Query().Get("filter")
			if token == "" {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"conferenceRecords": []map[string]interface{}{
						{"name": "conferenceRecords/c1", "startTime": "2026-05-20T13:00:00Z", "endTime": "2026-05-20T13:30:00Z", "space": "spaces/s1"},
					},
					"nextPageToken": "page2",
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"conferenceRecords": []map[string]interface{}{
						{"name": "conferenceRecords/c2", "startTime": "2026-05-21T13:00:00Z", "endTime": "2026-05-21T13:30:00Z", "space": "spaces/s2"},
					},
				})
			}
		case "/v2/spaces/s1":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"meetingCode": "aaa-bbbb-ccc", "meetingUri": "https://meet.google.com/aaa-bbbb-ccc"})
		case "/v2/spaces/s2":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"meetingCode": "ddd-eeee-fff", "meetingUri": "https://meet.google.com/ddd-eeee-fff"})
		case "/v2/conferenceRecords/c1/recordings":
			if token == "" {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"recordings":    []map[string]interface{}{{"name": "r1", "driveDestination": map[string]string{"file": "rec1a"}}},
					"nextPageToken": "rpage2",
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"recordings": []map[string]interface{}{{"name": "r2", "driveDestination": map[string]string{"file": "rec1b"}}},
				})
			}
		case "/v2/conferenceRecords/c1/transcripts":
			if token == "" {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"transcripts":   []map[string]interface{}{{"name": "t1", "docsDestination": map[string]string{"document": "tr1a"}}},
					"nextPageToken": "tpage2",
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"transcripts": []map[string]interface{}{{"name": "t2", "docsDestination": map[string]string{"document": "tr1b"}}},
				})
			}
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		}
	}))
	defer srv.Close()

	var clog bytes.Buffer
	c, _ := NewClient(log.New(&clog, "", 0), staticTS(),
		CustomHTTPClientOption(srv.Client()), CustomBaseURLOption(srv.URL))

	confs, err := c.ListConferences(context.Background(), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListConferences error: %v", err)
	}
	if len(confs) != 2 {
		t.Fatalf("expected 2 conferences across pages, got %d", len(confs))
	}
	if sawFilter == "" || !strings.Contains(sawFilter, "start_time>=") {
		t.Errorf("expected start_time filter, got %q", sawFilter)
	}
	// c1 paginated recordings (rec1a, rec1b) and transcripts (tr1a, tr1b)
	var recs, trs int
	for _, a := range confs[0].Artifacts {
		switch a.Kind {
		case "recording":
			recs++
		case "transcript":
			trs++
		}
	}
	if recs != 2 {
		t.Errorf("expected 2 paginated recording artifacts for c1, got %d", recs)
	}
	if trs != 2 {
		t.Errorf("expected 2 paginated transcript artifacts for c1, got %d", trs)
	}
	if confs[0].MeetingCode != "aaa-bbbb-ccc" || confs[1].MeetingCode != "ddd-eeee-fff" {
		t.Errorf("unexpected meeting codes %q / %q", confs[0].MeetingCode, confs[1].MeetingCode)
	}
}

// TestListConferencesSkipsOnSpaceFailure verifies a conference whose space
// cannot be resolved is skipped without failing the whole listing.
func TestListConferencesSkipsOnSpaceFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/conferenceRecords":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"conferenceRecords": []map[string]interface{}{
					{"name": "conferenceRecords/c1", "startTime": "2026-05-20T13:00:00Z", "space": "spaces/gone"},
				},
			})
		case "/v2/spaces/gone":
			http.Error(w, "not found", http.StatusNotFound)
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		}
	}))
	defer srv.Close()

	var clog bytes.Buffer
	c, _ := NewClient(log.New(&clog, "", 0), staticTS(),
		CustomHTTPClientOption(srv.Client()), CustomBaseURLOption(srv.URL))

	confs, err := c.ListConferences(context.Background(), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListConferences should not fail when a space is unresolvable: %v", err)
	}
	if len(confs) != 0 {
		t.Errorf("expected the unresolvable conference to be skipped, got %d", len(confs))
	}
}

// TestListConferencesSkipsArtifactsNotInDrive verifies artifacts whose Drive
// destination is empty (still processing) are skipped.
func TestListConferencesSkipsArtifactsNotInDrive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/conferenceRecords":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"conferenceRecords": []map[string]interface{}{
					{"name": "conferenceRecords/c1", "startTime": "2026-05-20T13:00:00Z", "space": "spaces/s1"},
				},
			})
		case "/v2/spaces/s1":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"meetingCode": "aaa-bbbb-ccc"})
		case "/v2/conferenceRecords/c1/recordings":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"recordings": []map[string]interface{}{
					{"name": "ready", "driveDestination": map[string]string{"file": "rec1"}},
					{"name": "processing", "driveDestination": map[string]string{"file": ""}},
				},
			})
		case "/v2/conferenceRecords/c1/transcripts":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"transcripts": []map[string]interface{}{
					{"name": "tprocessing", "docsDestination": map[string]string{"document": ""}},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		}
	}))
	defer srv.Close()

	var clog bytes.Buffer
	c, _ := NewClient(log.New(&clog, "", 0), staticTS(),
		CustomHTTPClientOption(srv.Client()), CustomBaseURLOption(srv.URL))

	confs, err := c.ListConferences(context.Background(), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListConferences error: %v", err)
	}
	if len(confs) != 1 {
		t.Fatalf("expected 1 conference, got %d", len(confs))
	}
	if got := len(confs[0].Artifacts); got != 1 {
		t.Fatalf("expected only the ready recording, got %d artifacts", got)
	}
	if confs[0].Artifacts[0].DriveFileID != "rec1" || confs[0].Artifacts[0].Kind != "recording" {
		t.Errorf("unexpected artifact %+v", confs[0].Artifacts[0])
	}
}
