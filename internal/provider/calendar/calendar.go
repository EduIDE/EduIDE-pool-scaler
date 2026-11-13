// Package calendar reads events from a static YAML file, for installations
// without Artemis or for dates entered by hand.
package calendar

import (
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

// File is the calendar file format:
//
//	events:
//	  - appDefinition: java-17
//	    kind: exam            # exam | release | due
//	    time: 2026-11-20T08:00:00+01:00
//	    end: 2026-11-20T10:00:00+01:00   # optional
//	    participants: 300
type File struct {
	Events []Entry `json:"events"`
}

// Entry is one event in the file.
type Entry struct {
	AppDefinition string    `json:"appDefinition"`
	Kind          string    `json:"kind"`
	Time          time.Time `json:"time"`
	End           time.Time `json:"end"`
	Participants  int       `json:"participants"`
}

// Provider reads the file on every call, so an updated ConfigMap applies
// without a restart.
type Provider struct {
	Path string
}

// Events returns every event in the file.
func (p *Provider) Events() ([]model.Event, error) {
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse reads calendar YAML.
func Parse(b []byte) ([]model.Event, error) {
	var f File
	if err := yaml.UnmarshalStrict(b, &f); err != nil {
		return nil, err
	}
	out := make([]model.Event, 0, len(f.Events))
	for i, e := range f.Events {
		kind := model.EventKind(e.Kind)
		if kind != model.EventExam && kind != model.EventRelease && kind != model.EventDue {
			return nil, fmt.Errorf("event %d: kind must be exam, release or due, got %q", i, e.Kind)
		}
		if e.AppDefinition == "" || e.Time.IsZero() {
			return nil, fmt.Errorf("event %d: appDefinition and time are required", i)
		}
		out = append(out, model.Event{
			AppDefinition: e.AppDefinition,
			Kind:          kind,
			Time:          e.Time,
			End:           e.End,
			Participants:  e.Participants,
			Source:        "calendar",
		})
	}
	return out, nil
}
