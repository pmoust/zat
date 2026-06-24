package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"
	"go.elastic.co/apm"
	"go.elastic.co/apm/module/apmhttp"
	"google.golang.org/api/drive/v3"
	"gopkg.in/yaml.v2"

	"github.com/graphaelli/zat/cmd"
	"github.com/graphaelli/zat/google"
	"github.com/graphaelli/zat/meet"
	"github.com/graphaelli/zat/slack"
	"github.com/graphaelli/zat/zoom"
)

// mustWriter is an io.Writer that panics on Write error
// for use where panics are recovered eg http handler
type mustWriter struct {
	w io.Writer
}

func (m mustWriter) Write(b []byte) {
	if _, err := m.w.Write(b); err != nil {
		panic(err)
	}
}

func NewMux(zat *Config, params runParams) *http.ServeMux {
	logger := zat.logger
	googleClient := zat.googleClient
	zoomClient := zat.zoomClient
	meetClient := zat.meetClient

	// zoomClient is nil when no zoom config is present (Meet-only install).
	zoomReady := func() bool { return zoomClient != nil && zoomClient.HasCreds() }

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		mw := mustWriter{w: w}
		w.Header().Set("Content-Type", "text/html")
		mw.Write([]byte("<meta http-equiv=\"refresh\" content=\"10\"/>"))
		mw.Write([]byte("<head><style>table, table th,table tr, table td{border-collapse: collapse;border:1px solid #000000;padding:3px}</style></head><body>"))

		mw.Write([]byte("<br>Google: "))
		if !googleClient.HasCreds() {
			mw.Write([]byte("<a href=\"/google\">login</a>"))
			// googleClient.OauthRedirect(w, r)
			//return
		} else {
			mw.Write([]byte("<span style=\"color:green\">OK</span>"))
		}

		if zoomClient != nil {
			mw.Write([]byte("<br>Zoom: "))
			if !zoomReady() {
				mw.Write([]byte("<a href=\"/zoom\">login</a>"))
				//zoomClient.OauthRedirect(w, r)
				//return
			} else {
				mw.Write([]byte("<span style=\"color:green\">OK</span>"))
			}
		}

		if meetClient != nil {
			mw.Write([]byte("<br>Meet: "))
			if googleClient.HasCreds() {
				mw.Write([]byte("<span style=\"color:green\">OK</span>"))
			} else {
				mw.Write([]byte("<a href=\"/google\">login</a>"))
			}
		}

		if archIsRunning {
			mw.Write([]byte("<br/>Archiving...</a>"))
		} else if googleClient.HasCreds() && (zoomReady() || len(zat.meetCopies) > 0) {
			mw.Write([]byte("<br/><a href=\"/archive\">Archive Now</a>"))
		} else {
			mw.Write([]byte("<br/>Login, to be able to archive"))
		}

		if len(archDetails) > 0 {
			mw.Write([]byte("<br/><br/><table><tr><th>Name</th><th>Date</th><th>Files</th><th>Status</th></th>"))
			for i := 0; i < len(archDetails); i++ {
				arch := archDetails[i]
				mw.Write([]byte(fmt.Sprintf("<tr><td><a href=\"%s\">%s</a></td><td>%s</td><td>%d</td><td><a href=\"%s\">%s</a></td></tr>",
					arch.sourceUrl, arch.name, arch.date, arch.fileNumber, arch.googleDriveURL, arch.status)))
			}
			mw.Write([]byte("</table>"))
		}
		mw.Write([]byte("</body>"))
	})

	mux.HandleFunc("/archive", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/archive" {
			http.NotFound(w, r)
			return
		}

		go doRun(zat, params)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.HandleFunc("/google", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/google" {
			http.NotFound(w, r)
			return
		}

		if !googleClient.HasCreds() {
			logger.Print("no google credentials, redirecting")
			googleClient.OauthRedirect(w, r)
			return
		}

		query := fmt.Sprintf("mimeType='%s'", google.MimeTypeFolder)
		andQuery := r.FormValue("q")
		if andQuery != "" {
			query += " and " + andQuery
		}
		files, err := googleClient.ListFiles(r.Context(), query, "")
		if err != nil {
			logger.Print(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(files); err != nil {
			logger.Print(err)
		}
	})

	mux.HandleFunc("/zoom", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zoom" {
			http.NotFound(w, r)
			return
		}

		if zoomClient == nil {
			http.NotFound(w, r)
			return
		}

		if !zoomReady() {
			logger.Print("no zoom credentials, redirecting")
			zoomClient.OauthRedirect(w, r)
			return
		}

		recordings, err := zoomClient.ListRecordings(r.Context(), time.Now().Add(-168*time.Hour), "")
		if err != nil {
			logger.Print(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if recordings == nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(recordings); err != nil {
			logger.Print(err)
		}
	})

	mux.HandleFunc("/meet", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/meet" {
			http.NotFound(w, r)
			return
		}
		if meetClient == nil {
			// no Meet client configured; logging in won't change that
			http.Error(w, "meet not configured", http.StatusServiceUnavailable)
			return
		}
		if !googleClient.HasCreds() {
			logger.Print("no google credentials for meet, redirecting")
			googleClient.OauthRedirect(w, r)
			return
		}
		conferences, err := meetClient.ListConferences(r.Context(), time.Now().Add(-168*time.Hour))
		if err != nil {
			logger.Print(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(conferences); err != nil {
			logger.Print(err)
		}
	})

	mux.HandleFunc("/oauth/google", googleClient.OauthHandler())
	if zoomClient != nil {
		mux.HandleFunc("/oauth/zoom", zoomClient.OauthHandler())
	}
	return mux
}

type Directive struct {
	Name      string `json:"name"`
	Google    string `json:"google"`
	Zoom      string `json:"zoom"`
	Meet      string `json:"meet"`
	Organizer string `json:"organizer"`
	Slack     string `json:"slack"`
}

// use invalid json to avoid conflict
var skipDirective = Directive{Name: "{skip"}

// zoom meeting -> action
type Config struct {
	logger       *log.Logger
	copies       map[int64]Directive
	meetCopies   map[string]Directive
	googleClient *google.Client
	slackClient  *slackapi.Client
	zoomClient   *zoom.Client
	meetClient   *meet.Client
}

func NewConfigFromFile(logger *log.Logger, path string, googleClient *google.Client, zoomClient *zoom.Client,
	meetClient *meet.Client, slackClient *slackapi.Client) (*Config, error) {
	f, err := os.Open(path)

	// A missing config file is fine (zat runs with no directives); surface any
	// other error (e.g. permissions) instead of silently using an empty config.
	// os.IsExist is never true for an os.Open error, so the old guard swallowed
	// real failures.
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	var r io.Reader = f

	if f == nil {
		r = bytes.NewReader(nil)
	} else {
		defer f.Close()
	}

	return NewConfigFromReader(logger, r, googleClient, zoomClient, meetClient, slackClient)
}

// decodeDirectives reads the zat.yml directive list from r.
func decodeDirectives(r io.Reader) ([]Directive, error) {
	var directives []Directive
	if err := yaml.NewDecoder(r).Decode(&directives); err != nil && err != io.EOF {
		return nil, err
	}
	return directives, nil
}

// loadDirectives reads directives from a file, treating a missing file as no
// directives. Used to decide which Google scopes to request before building
// the client.
func loadDirectives(path string) ([]Directive, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return decodeDirectives(f)
}

// meetConfigured reports whether any directive maps a Meet meeting.
func meetConfigured(directives []Directive) bool {
	for _, d := range directives {
		if d.Meet != "" {
			return true
		}
	}
	return false
}

func NewConfigFromReader(logger *log.Logger, r io.Reader, googleClient *google.Client, zoomClient *zoom.Client,
	meetClient *meet.Client, slackClient *slackapi.Client) (*Config, error) {
	directives, err := decodeDirectives(r)
	if err != nil {
		return nil, err
	}
	c := map[int64]Directive{}
	mc := map[string]Directive{}
	for _, d := range directives {
		if d.Zoom != "" {
			key, err := strconv.ParseInt(strings.ReplaceAll(d.Zoom, "-", ""), 10, 64)
			if err != nil {
				return nil, err
			}
			if _, exists := c[key]; exists {
				logger.Printf("config for zoom %d already exists, disabling any action", key)
				c[key] = skipDirective
			} else {
				c[key] = d
			}
		}
		if d.Meet != "" {
			mkey := normalizeMeetingCode(d.Meet)
			if _, exists := mc[mkey]; exists {
				logger.Printf("config for meet %s already exists, disabling any action", mkey)
				mc[mkey] = skipDirective
			} else {
				mc[mkey] = d
			}
		}
	}
	return &Config{
		logger:       logger,
		copies:       c,
		meetCopies:   mc,
		googleClient: googleClient,
		slackClient:  slackClient,
		zoomClient:   zoomClient,
		meetClient:   meetClient,
	}, nil
}

// normalizeMeetingCode lowercases and strips separators so "abc-defg-hij"
// and "ABCDEFGHIJ" map to the same directive key.
func normalizeMeetingCode(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return strings.ToLower(s)
}

// meetFolderName constructs the name of the gdrive folder for a Meet conference.
func meetFolderName(conf meet.Conference) string {
	return conf.StartTime.Format("2006-01-02")
}

// meetRecordingFileName constructs the destination file name for a Meet artifact.
// Naming uses the configured directive Name since the Meet API does not reliably
// expose a meeting title.
func meetRecordingFileName(action Directive, conf meet.Conference, artifact meet.Artifact) string {
	start := artifact.StartTime
	if start.IsZero() {
		start = conf.StartTime
	}
	baseName := fmt.Sprintf("%s %s", start.Format("2006-01-02-150405"), action.Name)
	var ext string
	switch artifact.Kind {
	case "transcript":
		ext = "transcript"
	default: // recording
		ext = "mp4"
	}
	return baseName + "." + ext
}

// meetingFolderName constructs the name of the gdrive folder containing the meeting
func meetingFolderName(meeting zoom.Meeting) string {
	// TODO: allow customization of folder name, perhaps "docker inspect --format" style
	return meeting.StartTime.Format("2006-01-02")
}

// recordingFileName constructs the name of the file for this meeting
func recordingFileName(meeting zoom.Meeting, recording zoom.RecordingFile) string {
	// TODO: allow customization of file name, perhaps "docker inspect --format" style
	start, err := time.Parse(time.RFC3339, recording.RecordingStart)
	if err != nil {
		start = meeting.StartTime
	}
	baseName := fmt.Sprintf("%s %s", start.Format("2006-01-02-150405"), meeting.Topic)
	var ext string
	switch e := strings.ToLower(recording.FileType); e {
	case "chat":
		ext = "chat.log"
	case "m4a", "mp4":
		ext = e
	case "timeline":
		ext = "timeline.json"
	case "transcript":
		ext = "vtt"
	default:
		if recording.RecordingType != "" {
			ext = strings.ToLower(recording.RecordingType) + "." + e
		} else {
			ext = e
		}
	}
	return baseName + "." + ext
}

func mkdir(ctx context.Context, gdrive *drive.Service, parent *drive.File, folder string) (*drive.File, error, bool) {
	span, ctx := apm.StartSpan(ctx, "mkdir", "app")
	defer span.End()

	// maybe no need to check if it exists first, can just "mkdir -p" no matter what? for now look to enable dryrun
	// exact match 1 folder
	query := fmt.Sprintf("mimeType=%q and %q in parents and name=%q and trashed=false", google.MimeTypeFolder, parent.Id, folder)
	if result, err := gdrive.Files.List().Context(ctx).SupportsTeamDrives(true).IncludeTeamDriveItems(true).Q(query).Do(); err != nil {
		return nil, err, false
	} else {
		fileCount := len(result.Files)
		if fileCount > 1 {
			return nil, fmt.Errorf("%d files found: %#v, expected 0 or 1", fileCount, result.Files), false
		} else if fileCount == 1 {
			return result.Files[0], nil, false
		}
	}

	// folder doesn't exist when we checked, create it.  no real problem if it was already created
	if result, err := gdrive.Files.Create(&drive.File{
		Name:     folder,
		MimeType: google.MimeTypeFolder,
		Parents:  []string{parent.Id},
	}).Context(ctx).SupportsAllDrives(true).Do(); err != nil {
		return nil, err, false
	} else {
		return result, nil, true
	}
}

func (z *Config) Archive(ctx context.Context, meeting zoom.Meeting, params runParams) error {
	span, ctx := apm.StartSpan(ctx, "Archive", "app")
	defer span.End()

	var curArchMeeting = archivedMeeting{name: meeting.Topic,
		fileNumber: 0,
		status:     "archiving",
		date:       meeting.StartTime.Format("2006-01-02 15:04"),
		sourceUrl:  meeting.ShareURL}
	archDetails = append(archDetails, &curArchMeeting)

	// check what is already uploaded for this meeting
	gdrive, err := z.googleClient.Service(ctx)
	if err != nil {
		return fmt.Errorf("while creating gdrive client: %w", err)
	}
	action := z.copies[meeting.ID]
	if action == skipDirective {
		curArchMeeting.status = "error"
		return fmt.Errorf("skipped mapping meeting %d %q", meeting.ID, meeting.Topic)
	}
	// parent folder of all meetings
	parentFolderName := action.Google
	if parentFolderName == "" {
		curArchMeeting.status = "error"
		return fmt.Errorf("no mapping found for meeting %d %q", meeting.ID, meeting.Topic)
	}

	parent, err := gdrive.Files.Get(parentFolderName).Context(ctx).SupportsAllDrives(true).Do()
	if err != nil {
		curArchMeeting.status = "error"
		return fmt.Errorf("while finding parent of %q: %w", parentFolderName, err)
	}

	// parent folder for this meeting
	meetingFolder, err, created := mkdir(ctx, gdrive, parent, meetingFolderName(meeting))
	if err != nil {
		curArchMeeting.status = "error"
		return fmt.Errorf("while finding/creating meeting folder: %w", err)
	}
	if created {
		z.logger.Printf("created folder %s: https://drive.google.com/drive/folders/%s",
			meetingFolder.Name, meetingFolder.Id)
	} else {
		z.logger.Printf("using existing folder %s: https://drive.google.com/drive/folders/%s",
			meetingFolder.Name, meetingFolder.Id)
	}

	curArchMeeting.googleDriveURL = "https://drive.google.com/drive/folders/" + meetingFolder.Id

	// list folder for this meeting
	alreadyUploaded := make(map[string]struct{})
	nextPageToken := ""
	for page := 0; page < 5; page++ {
		call := gdrive.Files.List().
			Context(ctx).
			SupportsTeamDrives(true).
			IncludeTeamDriveItems(true).
			Q(fmt.Sprintf("%q in parents", meetingFolder.Id))
		if nextPageToken != "" {
			call = call.PageToken(nextPageToken)
		}
		meetingFiles, err := call.Do()
		if err != nil {
			curArchMeeting.status = "error"
			return fmt.Errorf("while listing meeting folder: %w", err)
		}
		for _, f := range meetingFiles.Files {
			alreadyUploaded[f.Name] = struct{}{}
		}
		if meetingFiles.NextPageToken == "" {
			break
		}
		nextPageToken = meetingFiles.NextPageToken
	}

	// download & upload serially for now
	z.logger.Printf("archiving meeting %d to %s (https://drive.google.com/drive/folders/%s)",
		meeting.ID, meetingFolder.Name, meetingFolder.Id)
	notifyUpload := false

	exclude := func(string) bool { return false }
	if params.uploadFilter != "" {
		allowedFileTypes := map[string]bool{}
		for _, uf := range strings.Split(params.uploadFilter, ",") {
			allowedFileTypes[strings.ToLower(strings.TrimSpace(uf))] = true
		}
		exclude = func(fileType string) bool {
			return !allowedFileTypes[strings.ToLower(fileType)]
		}
	}

	for _, f := range meeting.RecordingFiles {
		//check if recording file duration is shorter than minimum
		start, err := time.Parse(time.RFC3339, f.RecordingStart)
		if err != nil {
			z.logger.Printf("couldn't parse file recording start %s - %s: %v", f.ID, f.RecordingStart, err)
		}

		end, err2 := time.Parse(time.RFC3339, f.RecordingEnd)
		if err2 != nil {
			z.logger.Printf("couldn't parse file recording end %s - %s: %v", f.ID, f.RecordingStart, err)
		}

		if err == nil && err2 == nil {
			duration := int(end.Sub(start).Minutes())
			if duration < params.minDuration {
				curArchMeeting.status = "skipped - length"
				z.logger.Printf("skipped %d minute recording at %s - %s", duration, start, end)
				continue
			}
		}

		name := recordingFileName(meeting, f)

		if exclude(f.FileType) {
			z.logger.Printf("skipping upload %s, file type %q excluded", name, strings.ToLower(f.FileType))
			continue
		}

		if _, exists := alreadyUploaded[name]; exists {
			curArchMeeting.status = "done"
			curArchMeeting.fileNumber++
			z.logger.Printf("skipping upload %s to %s/%s, already exists", name, parent.Name, meetingFolder.Name)
			continue
		}
		z.logger.Printf("uploading %q to \"%s/%s\"", name, parent.Name, meetingFolder.Name)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.DownloadURL, nil)
		if err != nil {
			curArchMeeting.status = "error"
			return fmt.Errorf("while building recording download request %s: %w", f.DownloadURL, err)
		}
		v := req.URL.Query()
		v.Add("access_token", z.zoomClient.AccessToken())
		req.URL.RawQuery = v.Encode()
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			curArchMeeting.status = "error"
			return fmt.Errorf("while downloading recording %s: %w", f.DownloadURL, err)
		}
		defer r.Body.Close()

		if r.StatusCode != http.StatusOK {
			curArchMeeting.status = "error"
			return fmt.Errorf("while downloading recording %s: download failed, got %d error: %#v",
				f.DownloadURL, r.StatusCode, r)
		}
		if contentType := r.Header.Get("content-type"); strings.HasPrefix(contentType, "text/html") {
			curArchMeeting.status = "error"
			return fmt.Errorf("while downloading recording %s: download failed, got %s content",
				f.DownloadURL, contentType)
		}
		_, err = gdrive.Files.Create(&drive.File{
			Name:    name,
			Parents: []string{meetingFolder.Id},
		}).Context(ctx).Media(r.Body).SupportsAllDrives(true).Do()
		if err != nil {
			curArchMeeting.status = "error"
			return fmt.Errorf("while uploading recording %s: %w", f.DownloadURL, err)
		}
		curArchMeeting.fileNumber++
		z.logger.Printf("uploaded %q to %s/%s", name, parent.Name, meetingFolder.Name)
		if strings.ToLower(f.FileType) == "mp4" {
			notifyUpload = true
		}
	}
	if notifyUpload && action.Slack != "" && z.slackClient != nil {
		slackSpan, ctx := apm.StartSpan(ctx, "slack", "app")
		body := fmt.Sprintf("%s recording now available: https://drive.google.com/drive/folders/%s", meeting.Topic, meetingFolder.Id)
		channel, _, text, err := z.slackClient.SendMessageContext(ctx, action.Slack, slackapi.MsgOptionText(body, true))
		if err != nil {
			z.logger.Printf("failed to notify slack %q: %s", action.Slack, err)
			apm.CaptureError(ctx, err).Send()
		} else {
			z.logger.Printf("notified slack %q: %s", channel, text)
		}
		slackSpan.End()
	}
	curArchMeeting.status = "done"
	return nil
}

func (z *Config) archiveMeetConference(ctx context.Context, conf meet.Conference, params runParams) error {
	span, ctx := apm.StartSpan(ctx, "archiveMeetConference", "app")
	defer span.End()

	// ListConferences returns every conference the account attended, so an
	// unmapped or duplicate-disabled meeting code is the common case, not an
	// error worth reporting to APM - log and skip quietly.
	action := z.meetCopies[normalizeMeetingCode(conf.MeetingCode)]
	if action == skipDirective {
		z.logger.Printf("skipped mapping meet conference %q", conf.MeetingCode)
		return nil
	}
	if action.Google == "" {
		z.logger.Printf("no mapping found for meet conference %q, skipping", conf.MeetingCode)
		return nil
	}

	// nothing to copy (e.g. a mapped meeting occurred but wasn't recorded) -
	// skip before creating an empty dated folder in Drive.
	if len(conf.Artifacts) == 0 {
		z.logger.Printf("no artifacts for meet conference %q, skipping", conf.MeetingCode)
		return nil
	}

	var curArchMeeting = archivedMeeting{
		name:       action.Name,
		fileNumber: 0,
		status:     "archiving",
		date:       conf.StartTime.Format("2006-01-02 15:04"),
		sourceUrl:  conf.SourceURL,
	}
	archDetails = append(archDetails, &curArchMeeting)

	gdrive, err := z.googleClient.Service(ctx)
	if err != nil {
		curArchMeeting.status = "error"
		return fmt.Errorf("while creating gdrive client: %w", err)
	}

	parent, err := gdrive.Files.Get(action.Google).Context(ctx).SupportsAllDrives(true).Do()
	if err != nil {
		curArchMeeting.status = "error"
		return fmt.Errorf("while finding parent of %q: %w", action.Google, err)
	}

	meetingFolder, err, created := mkdir(ctx, gdrive, parent, meetFolderName(conf))
	if err != nil {
		curArchMeeting.status = "error"
		return fmt.Errorf("while finding/creating meeting folder: %w", err)
	}
	if created {
		z.logger.Printf("created folder %s: https://drive.google.com/drive/folders/%s", meetingFolder.Name, meetingFolder.Id)
	} else {
		z.logger.Printf("using existing folder %s: https://drive.google.com/drive/folders/%s", meetingFolder.Name, meetingFolder.Id)
	}
	curArchMeeting.googleDriveURL = "https://drive.google.com/drive/folders/" + meetingFolder.Id

	// list folder for this meeting to dedupe
	alreadyUploaded := make(map[string]struct{})
	nextPageToken := ""
	for page := 0; page < 5; page++ {
		call := gdrive.Files.List().
			Context(ctx).
			SupportsTeamDrives(true).
			IncludeTeamDriveItems(true).
			Q(fmt.Sprintf("%q in parents", meetingFolder.Id))
		if nextPageToken != "" {
			call = call.PageToken(nextPageToken)
		}
		meetingFiles, err := call.Do()
		if err != nil {
			curArchMeeting.status = "error"
			return fmt.Errorf("while listing meeting folder: %w", err)
		}
		for _, f := range meetingFiles.Files {
			alreadyUploaded[f.Name] = struct{}{}
		}
		if meetingFiles.NextPageToken == "" {
			break
		}
		nextPageToken = meetingFiles.NextPageToken
	}

	exclude := func(string) bool { return false }
	if params.uploadFilter != "" {
		allowedFileTypes := map[string]bool{}
		for _, uf := range strings.Split(params.uploadFilter, ",") {
			allowedFileTypes[strings.ToLower(strings.TrimSpace(uf))] = true
		}
		exclude = func(kind string) bool {
			return !allowedFileTypes[strings.ToLower(kind)]
		}
	}

	notifyUpload := false
	for _, a := range conf.Artifacts {
		name := meetRecordingFileName(action, conf, a)
		if exclude(a.Kind) {
			z.logger.Printf("skipping copy %s, kind %q excluded", name, a.Kind)
			continue
		}
		if _, exists := alreadyUploaded[name]; exists {
			curArchMeeting.status = "done"
			curArchMeeting.fileNumber++
			z.logger.Printf("skipping copy %s to %s/%s, already exists", name, parent.Name, meetingFolder.Name)
			continue
		}
		z.logger.Printf("copying %q to \"%s/%s\"", name, parent.Name, meetingFolder.Name)
		_, err := gdrive.Files.Copy(a.DriveFileID, &drive.File{
			Name:    name,
			Parents: []string{meetingFolder.Id},
		}).Context(ctx).SupportsAllDrives(true).Do()
		if err != nil {
			curArchMeeting.status = "error"
			return fmt.Errorf("while copying %s (%s): %w", name, a.DriveFileID, err)
		}
		curArchMeeting.fileNumber++
		z.logger.Printf("copied %q to %s/%s", name, parent.Name, meetingFolder.Name)
		if a.Kind == "recording" {
			notifyUpload = true
		}
	}

	if notifyUpload && action.Slack != "" && z.slackClient != nil {
		slackSpan, ctx := apm.StartSpan(ctx, "slack", "app")
		body := fmt.Sprintf("%s recording now available: https://drive.google.com/drive/folders/%s", action.Name, meetingFolder.Id)
		channel, _, text, err := z.slackClient.SendMessageContext(ctx, action.Slack, slackapi.MsgOptionText(body, true))
		if err != nil {
			z.logger.Printf("failed to notify slack %q: %s", action.Slack, err)
			apm.CaptureError(ctx, err).Send()
		} else {
			z.logger.Printf("notified slack %q: %s", channel, text)
		}
		slackSpan.End()
	}
	curArchMeeting.status = "done"
	return nil
}

type runParams struct {
	minDuration  int
	since        time.Duration
	uploadFilter string
}

func (z *Config) Run(params runParams) error {
	tx := apm.DefaultTracer.StartTransaction("archiveRecordings", "background")
	defer tx.End()
	ctx := apm.ContextWithTransaction(context.Background(), tx)

	z.logger.Print("archiving recordings")
	nextPageToken := ""
	for {
		recordings, err := z.zoomClient.ListRecordings(ctx, time.Now().Add(-1*params.since), nextPageToken)
		if err != nil {
			apm.CaptureError(ctx, err).Send()
			return fmt.Errorf("failed to list recordings: %w", err)
		}
		for _, meeting := range recordings.Meetings {
			if meeting.Duration < params.minDuration {
				z.logger.Printf("skipped %d minute meeting at %s", meeting.Duration, meeting.StartTime)
				continue
			}
			if err := z.Archive(ctx, meeting, params); err != nil {
				z.logger.Print(err)
				apm.CaptureError(ctx, err).Send()
			}
		}
		nextPageToken = recordings.NextPageToken
		if nextPageToken == "" {
			break
		}
	}
	z.logger.Print("done archiving recordings")
	return nil
}

func (z *Config) RunMeet(params runParams) error {
	tx := apm.DefaultTracer.StartTransaction("archiveMeetRecordings", "background")
	defer tx.End()
	ctx := apm.ContextWithTransaction(context.Background(), tx)

	z.logger.Print("archiving meet recordings")
	conferences, err := z.meetClient.ListConferences(ctx, time.Now().Add(-1*params.since))
	if err != nil {
		apm.CaptureError(ctx, err).Send()
		return fmt.Errorf("failed to list meet conferences: %w", err)
	}
	for _, conf := range conferences {
		if !conf.EndTime.IsZero() && !conf.StartTime.IsZero() {
			duration := int(conf.EndTime.Sub(conf.StartTime).Minutes())
			if duration < params.minDuration {
				z.logger.Printf("skipped %d minute meet conference at %s", duration, conf.StartTime)
				continue
			}
		}
		if err := z.archiveMeetConference(ctx, conf, params); err != nil {
			z.logger.Print(err)
			apm.CaptureError(ctx, err).Send()
		}
	}
	z.logger.Print("done archiving meet recordings")
	return nil
}

type archivedMeeting struct {
	name           string
	fileNumber     int
	status         string
	date           string
	sourceUrl      string
	googleDriveURL string
}

var (
	archIsRunning   bool
	archIsRunningMu sync.Mutex
	archDetails     = []*archivedMeeting{}
)

func doRun(zat *Config, params runParams) {
	if zat == nil {
		// no logger to log with
		return
	}
	zat.logger.Print("starting archive tool")
	if !zat.googleClient.HasCreds() {
		zat.logger.Println("no Google creds")
		return
	}
	// zoomClient is nil on a Meet-only install (no zoom config).
	zoomReady := zat.zoomClient != nil && zat.zoomClient.HasCreds()
	if !zoomReady && len(zat.meetCopies) == 0 {
		zat.logger.Println("no Zoom creds and no Meet directives")
		return
	}

	archIsRunningMu.Lock()
	start := !archIsRunning
	archIsRunning = true
	archIsRunningMu.Unlock()

	if !start {
		zat.logger.Println("archiving skipped, it's already running")
		return
	}

	// reset once per cycle, before any backend runs, so a Meet-only deployment
	// (where Run is never called) doesn't accumulate rows across cycles.
	archDetails = []*archivedMeeting{}

	if zoomReady {
		if err := zat.Run(params); err != nil {
			zat.logger.Println(err)
		}
	}
	if zat.meetClient != nil && len(zat.meetCopies) > 0 && zat.googleClient.HasCreds() {
		if err := zat.RunMeet(params); err != nil {
			zat.logger.Println(err)
		}
	}

	archIsRunningMu.Lock()
	archIsRunning = false
	archIsRunningMu.Unlock()
}

func main() {
	cfgDir := cmd.FlagConfigDir()
	addr := flag.String("addr", "localhost:8080", "web server listener address")
	noServer := flag.Bool("no-server", false, "don't start web server")
	minDuration := flag.Int("min-duration", 5, "minimum meeting duration in minutes to archive")
	since := flag.Duration("since", 168*time.Hour, "since")
	uploadFilter := flag.String("t", "",
		"comma separated list of file types to archive; Zoom: mp4, m4a, timeline, transcript, chat, cc, csv; "+
			"Meet: recording, transcript")
	flag.Parse()

	logger := log.New(os.Stderr, "", cmd.LogFmt)

	// Instrument http.DefaultClient and http.DefaultTransport.
	http.DefaultClient = apmhttp.WrapClient(http.DefaultClient)
	http.DefaultTransport = apmhttp.WrapRoundTripper(http.DefaultTransport)

	zatConfigPath := path.Join(*cfgDir, cmd.ZatConfigPath)

	// Decide up front whether Meet is in use, so the Google login only requests
	// the Meet scope (and a Meet client is built) when there are meet directives.
	// Drive/Zoom-only users aren't prompted for Meet permissions.
	directives, err := loadDirectives(zatConfigPath)
	if err != nil {
		logger.Println("failed to load config", err)
	}
	useMeet := meetConfigured(directives)

	googleOptions := []google.ClientOption{
		google.NewCredentialsManager(path.Join(*cfgDir, cmd.GoogleCredsPath)).ClientOption,
	}
	if useMeet {
		googleOptions = append(googleOptions, google.WithMeetScope())
	}
	googleClient, err := google.NewClientFromFile(
		logger,
		path.Join(*cfgDir, cmd.GoogleConfigPath),
		googleOptions...,
	)
	if err != nil {
		logger.Fatal(err)
	}
	// Zoom is optional: a missing/unreadable zoom config disables Zoom archival
	// (e.g. a Meet-only install) rather than aborting startup.
	zoomClient, err := zoom.NewClientFromFile(
		logger,
		path.Join(*cfgDir, cmd.ZoomConfigPath),
		zoom.NewCredentialsManager(path.Join(*cfgDir, cmd.ZoomCredsPath)).ClientOption,
	)
	if err != nil {
		logger.Printf("zoom archival disabled: %s", err)
		zoomClient = nil
	}
	slackClient, _ := slack.NewClientFromEnvOrFile(logger, path.Join(*cfgDir, cmd.SlackConfigPath), slackapi.OptionHTTPClient(http.DefaultClient))

	// Meet client only when Meet is configured; nil otherwise (handlers guard it).
	var meetClient *meet.Client
	if useMeet {
		meetClient, err = meet.NewClient(logger, googleClient.TokenSource(context.Background()))
		if err != nil {
			logger.Fatal(err)
		}
	}
	rp := runParams{
		minDuration:  *minDuration,
		since:        *since,
		uploadFilter: *uploadFilter,
	}

	zat, err := NewConfigFromFile(logger, zatConfigPath, googleClient, zoomClient, meetClient, slackClient)
	if err != nil {
		// ok to continue without config, just can't do archival
		logger.Println("failed to load config", err)
	}

	var wg sync.WaitGroup
	if !*noServer {
		wg.Add(1)
		server := http.Server{
			Addr:    *addr,
			Handler: apmhttp.Wrap(NewMux(zat, rp)),
		}
		go func() {
			logger.Printf("starting on http://%s", server.Addr)
			if err := server.ListenAndServe(); err != nil {
				logger.Fatal(err)
			}
			wg.Done()
		}()
	}

	if zat != nil {
		// already logged that config is loaded, just skip the run
		wg.Add(1)
		go func() {
			doRun(zat, rp)
			wg.Done()
		}()
	}

	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	apm.DefaultTracer.Flush(ctx.Done())
}
