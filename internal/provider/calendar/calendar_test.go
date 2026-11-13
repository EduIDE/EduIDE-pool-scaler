package calendar

import "testing"

func TestParse(t *testing.T) {
	evs, err := Parse([]byte(`
events:
  - appDefinition: java-17
    kind: exam
    time: 2026-11-20T08:00:00+01:00
    participants: 300
`))
	if err != nil || len(evs) != 1 || evs[0].Participants != 300 || evs[0].Source != "calendar" {
		t.Fatalf("%+v %v", evs, err)
	}
	for _, bad := range []string{
		"events: [{appDefinition: a, kind: party, time: 2026-11-20T08:00:00Z}]",
		"events: [{kind: exam, time: 2026-11-20T08:00:00Z}]",
		"events: [{appDefinition: a, kind: exam, time: 2026-11-20T08:00:00Z, typo: 1}]",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
