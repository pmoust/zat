package mock

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ApiHandler mocks the Google Meet REST API v2 endpoints used by ListConferences.
func ApiHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Log("mock meet handler handling", r.URL.String())
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")

		switch {
		case p == "/v2/conferenceRecords":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"conferenceRecords": []map[string]interface{}{
					{
						"name":      "conferenceRecords/abc",
						"startTime": "2026-05-20T13:00:00Z",
						"endTime":   "2026-05-20T13:45:00Z",
						"space":     "spaces/space123",
					},
				},
			})
		case p == "/v2/spaces/space123":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"name":        "spaces/space123",
				"meetingUri":  "https://meet.google.com/abc-defg-hij",
				"meetingCode": "abc-defg-hij",
			})
		case p == "/v2/conferenceRecords/abc/recordings":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"recordings": []map[string]interface{}{
					{
						"name":             "conferenceRecords/abc/recordings/r1",
						"state":            "FILE_GENERATED",
						"startTime":        "2026-05-20T13:00:00Z",
						"endTime":          "2026-05-20T13:45:00Z",
						"driveDestination": map[string]string{"file": "driveFileRec1"},
					},
				},
			})
		case p == "/v2/conferenceRecords/abc/transcripts":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"transcripts": []map[string]interface{}{
					{
						"name":            "conferenceRecords/abc/transcripts/t1",
						"state":           "FILE_GENERATED",
						"startTime":       "2026-05-20T13:00:00Z",
						"endTime":         "2026-05-20T13:45:00Z",
						"docsDestination": map[string]string{"document": "docFileTr1"},
					},
				},
			})
		default:
			if strings.HasPrefix(p, "/v2/") {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{})
				return
			}
			http.NotFound(w, r)
		}
	}
}
