// Package budget computes how many idle pods fit and shares them out.
package budget

import (
	"math"
	"sort"
)

// Resources are amounts in base units: CPU in cores, memory in bytes.
type Resources struct {
	CPU    float64
	Memory float64
}

// Unlimited is used for a source that sets no limit.
var Unlimited = Resources{CPU: math.Inf(1), Memory: math.Inf(1)}

// Sources are the three independent limits on new requests. A nil source sets
// no limit (no ResourceQuota, no matching nodes known, no usage query).
type Sources struct {
	// Quota is what the namespace ResourceQuota still allows.
	Quota *Resources
	// Nodes is the allocatable not yet requested on the session nodes.
	Nodes *Resources
	// UsageMemory is the memory still free by actual usage, already scaled by
	// the headroom fraction.
	UsageMemory *float64
}

// Free is the budget for idle pods: the minimum of the sources, plus what the
// idle pods already hold, since they are what is being decided anew.
func Free(s Sources, idleHeld Resources) Resources {
	free := Unlimited
	for _, r := range []*Resources{s.Quota, s.Nodes} {
		if r != nil {
			free.CPU = math.Min(free.CPU, r.CPU)
			free.Memory = math.Min(free.Memory, r.Memory)
		}
	}
	if s.UsageMemory != nil {
		free.Memory = math.Min(free.Memory, *s.UsageMemory)
	}
	free.CPU = math.Max(free.CPU, 0) + idleHeld.CPU
	free.Memory = math.Max(free.Memory, 0) + idleHeld.Memory
	return free
}

// Demand is one AppDefinition's request for idle pods.
type Demand struct {
	App     string
	Floor   int
	Desired int
	Pod     Resources
}

// Grant is what one AppDefinition gets.
type Grant struct {
	Value   int
	Clamped bool
}

// Allocate grants every floor first, whatever it costs, then shares the rest
// in proportion to each deficit (desired - floor), rounded down. One scale
// factor for everyone keeps the shares proportional on the scarcest resource.
func Allocate(free Resources, demands []Demand) map[string]Grant {
	rest := free
	for _, d := range demands {
		rest.CPU -= float64(d.Floor) * d.Pod.CPU
		rest.Memory -= float64(d.Floor) * d.Pod.Memory
	}
	rest.CPU, rest.Memory = math.Max(rest.CPU, 0), math.Max(rest.Memory, 0)

	var need Resources
	for _, d := range demands {
		deficit := float64(max(d.Desired-d.Floor, 0))
		need.CPU += deficit * d.Pod.CPU
		need.Memory += deficit * d.Pod.Memory
	}
	scale := 1.0
	if need.CPU > rest.CPU {
		scale = math.Min(scale, rest.CPU/need.CPU)
	}
	if need.Memory > rest.Memory {
		scale = math.Min(scale, rest.Memory/need.Memory)
	}

	out := make(map[string]Grant, len(demands))
	sort.Slice(demands, func(i, j int) bool { return demands[i].App < demands[j].App })
	for _, d := range demands {
		deficit := max(d.Desired-d.Floor, 0)
		// The epsilon keeps 0.1*30 = 2.9999... from rounding a full share down.
		v := d.Floor + int(math.Floor(float64(deficit)*scale+1e-9))
		out[d.App] = Grant{Value: v, Clamped: v < d.Desired}
	}
	return out
}
