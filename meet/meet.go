package meet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"time"

	"golang.org/x/oauth2"
)

const defaultBaseURL = "https://meet.googleapis.com/"

// Artifact is one copyable thing already living in Drive.
type Artifact struct {
	DriveFileID string    // Drive file id of recording mp4 or transcript doc
	Kind        string    // "recording" | "transcript"
	StartTime   time.Time
}

// Conference is one meeting occurrence with its artifacts.
type Conference struct {
	Name        string // conferenceRecords/xxx
	MeetingCode string // resolved from space, matches zat.yml `meet:`
	StartTime   time.Time
	EndTime     time.Time
	SourceURL   string // space.meetingUri
	Artifacts   []Artifact
}

// Client talks to the Google Meet REST API for discovery only.
type Client struct {
	logger     *log.Logger
	httpClient *http.Client
	ts         oauth2.TokenSource
	baseURL    *url.URL
}

type ClientOption func(*Client)

func CustomHTTPClientOption(httpClient *http.Client) ClientOption {
	return func(c *Client) { c.httpClient = httpClient }
}

func CustomBaseURLOption(raw string) ClientOption {
	return func(c *Client) {
		if u, err := url.Parse(raw); err == nil {
			c.baseURL = u
		}
	}
}

// NewClient builds a Meet client from any oauth2.TokenSource.
func NewClient(logger *log.Logger, ts oauth2.TokenSource, options ...ClientOption) (*Client, error) {
	if ts == nil {
		return nil, errors.New("meet: token source is required")
	}
	base, _ := url.Parse(defaultBaseURL)
	c := &Client{
		logger:     logger,
		httpClient: http.DefaultClient,
		ts:         ts,
		baseURL:    base,
	}
	for _, o := range options {
		o(c)
	}
	return c, nil
}

func (c *Client) authHeader() (string, error) {
	tok, err := c.ts.Token()
	if err != nil {
		return "", fmt.Errorf("meet: while obtaining token: %w", err)
	}
	return "Bearer " + tok.AccessToken, nil
}

// getJSON performs a GET against the Meet API and decodes the body into dst.
func (c *Client) getJSON(ctx context.Context, relPath string, query url.Values, dst interface{}) error {
	u := *c.baseURL
	u.Path = path.Join(u.Path, relPath)
	if query != nil {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("meet: while building request %s: %w", relPath, err)
	}
	auth, err := c.authHeader()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)

	rsp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("meet: while calling %s: %w", relPath, err)
	}
	defer rsp.Body.Close()
	if rsp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("meet: API call to %s failed: %d", u.String(), rsp.StatusCode)
		if body, rerr := ioutil.ReadAll(rsp.Body); rerr == nil {
			msg += ": " + string(body)
		}
		if c.logger != nil {
			c.logger.Print(msg)
		}
		return errors.New(msg)
	}
	if err := json.NewDecoder(rsp.Body).Decode(dst); err != nil {
		return fmt.Errorf("meet: while decoding %s: %w", relPath, err)
	}
	return nil
}

type driveDestination struct {
	File      string `json:"file"`
	ExportURI string `json:"exportUri"`
}

type docsDestination struct {
	Document  string `json:"document"`
	ExportURI string `json:"exportUri"`
}

type apiRecording struct {
	Name             string           `json:"name"`
	State            string           `json:"state"`
	StartTime        string           `json:"startTime"`
	EndTime          string           `json:"endTime"`
	DriveDestination driveDestination `json:"driveDestination"`
}

type apiTranscript struct {
	Name            string          `json:"name"`
	State           string          `json:"state"`
	StartTime       string          `json:"startTime"`
	EndTime         string          `json:"endTime"`
	DocsDestination docsDestination `json:"docsDestination"`
}

type apiSpace struct {
	Name        string `json:"name"`
	MeetingURI  string `json:"meetingUri"`
	MeetingCode string `json:"meetingCode"`
}

type apiConferenceRecord struct {
	Name      string `json:"name"`
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
	Space     string `json:"space"`
}

type listConferenceRecordsResponse struct {
	ConferenceRecords []apiConferenceRecord `json:"conferenceRecords"`
	NextPageToken     string                `json:"nextPageToken"`
}

type listRecordingsResponse struct {
	Recordings    []apiRecording `json:"recordings"`
	NextPageToken string         `json:"nextPageToken"`
}

type listTranscriptsResponse struct {
	Transcripts   []apiTranscript `json:"transcripts"`
	NextPageToken string          `json:"nextPageToken"`
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ListConferences enumerates conference records since `since`, resolving each
// space's meetingCode and listing its recordings + transcripts.
func (c *Client) ListConferences(ctx context.Context, since time.Time) ([]Conference, error) {
	var out []Conference
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("filter", fmt.Sprintf("start_time>=%q", since.Format(time.RFC3339)))
		q.Set("pageSize", "100")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var resp listConferenceRecordsResponse
		if err := c.getJSON(ctx, "/v2/conferenceRecords", q, &resp); err != nil {
			return nil, fmt.Errorf("meet: while listing conference records: %w", err)
		}
		for _, cr := range resp.ConferenceRecords {
			conf := Conference{
				Name:      cr.Name,
				StartTime: parseTime(cr.StartTime),
				EndTime:   parseTime(cr.EndTime),
			}

			// resolve space -> meeting code; skip conference on failure
			if cr.Space != "" {
				var sp apiSpace
				if err := c.getJSON(ctx, "/v2/"+cr.Space, nil, &sp); err != nil {
					if c.logger != nil {
						c.logger.Printf("meet: skipping %s, could not resolve space %s: %v", cr.Name, cr.Space, err)
					}
					continue
				}
				conf.MeetingCode = sp.MeetingCode
				conf.SourceURL = sp.MeetingURI
			}

			conf.Artifacts = append(conf.Artifacts, c.recordingArtifacts(ctx, cr.Name)...)
			conf.Artifacts = append(conf.Artifacts, c.transcriptArtifacts(ctx, cr.Name)...)
			out = append(out, conf)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return out, nil
}

func (c *Client) recordingArtifacts(ctx context.Context, record string) []Artifact {
	var arts []Artifact
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("pageSize", "100")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var resp listRecordingsResponse
		if err := c.getJSON(ctx, "/v2/"+record+"/recordings", q, &resp); err != nil {
			if c.logger != nil {
				c.logger.Printf("meet: while listing recordings for %s: %v", record, err)
			}
			break
		}
		for _, r := range resp.Recordings {
			if r.DriveDestination.File == "" {
				if c.logger != nil {
					c.logger.Printf("meet: recording %s not yet in Drive, skipping", r.Name)
				}
				continue
			}
			arts = append(arts, Artifact{
				DriveFileID: r.DriveDestination.File,
				Kind:        "recording",
				StartTime:   parseTime(r.StartTime),
			})
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return arts
}

func (c *Client) transcriptArtifacts(ctx context.Context, record string) []Artifact {
	var arts []Artifact
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("pageSize", "100")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var resp listTranscriptsResponse
		if err := c.getJSON(ctx, "/v2/"+record+"/transcripts", q, &resp); err != nil {
			if c.logger != nil {
				c.logger.Printf("meet: while listing transcripts for %s: %v", record, err)
			}
			break
		}
		for _, tr := range resp.Transcripts {
			if tr.DocsDestination.Document == "" {
				if c.logger != nil {
					c.logger.Printf("meet: transcript %s not yet in Drive, skipping", tr.Name)
				}
				continue
			}
			arts = append(arts, Artifact{
				DriveFileID: tr.DocsDestination.Document,
				Kind:        "transcript",
				StartTime:   parseTime(tr.StartTime),
			})
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return arts
}
