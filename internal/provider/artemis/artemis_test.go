package artemis

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

// The example from the shared contract, plus an exam with two exercises.
const body = `[
 {"exerciseId": 123, "courseId": 4, "examId": null, "appDefinition": "java-17",
  "releaseDate": "2026-11-20T08:00:00Z", "startDate": null, "dueDate": "2026-11-27T23:59:00Z",
  "examStartDate": null, "examEndDate": null, "studentCount": 412},
 {"exerciseId": 7, "courseId": 4, "examId": 9, "appDefinition": "java-17",
  "examStartDate": "2026-11-21T08:00:00Z", "examEndDate": "2026-11-21T10:00:00Z", "studentCount": 300},
 {"exerciseId": 8, "courseId": 4, "examId": 9, "appDefinition": "java-17",
  "examStartDate": "2026-11-21T08:00:00Z", "examEndDate": "2026-11-21T10:00:00Z", "studentCount": 300}
]`

func TestEvents(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != Path || r.URL.Query().Get("days") != "14" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := &Provider{BaseURL: srv.URL + "/", Token: "tok", Days: 14, Interval: 15 * time.Minute}
	now := time.Now()
	evs, err := p.Events(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("want release, due and one exam, got %+v", evs)
	}
	want := []model.EventKind{model.EventRelease, model.EventDue, model.EventExam}
	for i, e := range evs {
		if e.Kind != want[i] || e.AppDefinition != "java-17" {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	if evs[0].Participants != 412 || !evs[0].Time.Equal(time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("release %+v", evs[0])
	}
	if evs[2].End.IsZero() || evs[2].Participants != 300 {
		t.Fatalf("exam %+v", evs[2])
	}

	// Cached within the interval.
	_, _ = p.Events(context.Background(), now.Add(time.Minute))
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
	// A failed refresh keeps the previous events.
	p.Token = "wrong"
	evs, err = p.Events(context.Background(), now.Add(time.Hour))
	if err == nil || len(evs) != 3 {
		t.Fatalf("err %v, events %d", err, len(evs))
	}
}
