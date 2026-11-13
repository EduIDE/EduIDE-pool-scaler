// Package artemis pulls upcoming exercises and exams from the Artemis
// endpoint built for the scaler. It returns aggregated numbers only.
package artemis

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

// Path of the endpoint, fixed by the shared contract.
const Path = "/api/programming/admin/online-ide/upcoming"

// Entry is one element of the endpoint's JSON array.
type Entry struct {
	ExerciseID    int64      `json:"exerciseId"`
	CourseID      int64      `json:"courseId"`
	ExamID        *int64     `json:"examId"`
	AppDefinition string     `json:"appDefinition"`
	ReleaseDate   *time.Time `json:"releaseDate"`
	StartDate     *time.Time `json:"startDate"`
	DueDate       *time.Time `json:"dueDate"`
	ExamStartDate *time.Time `json:"examStartDate"`
	ExamEndDate   *time.Time `json:"examEndDate"`
	StudentCount  int        `json:"studentCount"`
}

// Provider fetches at most once per Interval and serves the cached events in
// between, and also when a fetch fails.
type Provider struct {
	BaseURL  string
	Token    string
	Days     int
	Interval time.Duration
	Client   *http.Client

	mu      sync.Mutex
	fetched time.Time
	events  []model.Event
}

// Events returns the cached events, refreshing them when due. On a failed
// refresh it returns the previous events together with the error.
func (p *Provider) Events(ctx context.Context, now time.Time) ([]model.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.fetched.IsZero() && now.Sub(p.fetched) < p.Interval {
		return p.events, nil
	}
	entries, err := p.fetch(ctx)
	if err != nil {
		return p.events, err
	}
	p.events, p.fetched = ToEvents(entries), now
	return p.events, nil
}

func (p *Provider) fetch(ctx context.Context) ([]Entry, error) {
	u, err := url.Parse(strings.TrimSuffix(p.BaseURL, "/") + Path)
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{"days": {strconv.Itoa(p.Days)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Accept", "application/json")
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artemis answered %s", resp.Status)
	}
	var entries []Entry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&entries); err != nil {
		return nil, fmt.Errorf("artemis answer: %w", err)
	}
	return entries, nil
}

// ToEvents maps entries to events. An exam becomes one exam event per
// AppDefinition, however many of its exercises use it. An exercise becomes a
// release event (start date, else release date) and a due event.
func ToEvents(entries []Entry) []model.Event {
	var out []model.Event
	exams := map[string]bool{}
	for _, e := range entries {
		if e.AppDefinition == "" {
			continue
		}
		if e.ExamID != nil {
			key := fmt.Sprintf("%d/%s", *e.ExamID, e.AppDefinition)
			if exams[key] || e.ExamStartDate == nil {
				continue
			}
			exams[key] = true
			ev := model.Event{AppDefinition: e.AppDefinition, Kind: model.EventExam, Time: *e.ExamStartDate, Participants: e.StudentCount, Source: "artemis"}
			if e.ExamEndDate != nil {
				ev.End = *e.ExamEndDate
			}
			out = append(out, ev)
			continue
		}
		start := e.StartDate
		if start == nil {
			start = e.ReleaseDate
		}
		if start != nil {
			out = append(out, model.Event{AppDefinition: e.AppDefinition, Kind: model.EventRelease, Time: *start, Participants: e.StudentCount, Source: "artemis"})
		}
		if e.DueDate != nil {
			out = append(out, model.Event{AppDefinition: e.AppDefinition, Kind: model.EventDue, Time: *e.DueDate, Participants: e.StudentCount, Source: "artemis"})
		}
	}
	return out
}
