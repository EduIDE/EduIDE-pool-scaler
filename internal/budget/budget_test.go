package budget

import (
	"math"
	"testing"
)

var java = Resources{CPU: 0.6, Memory: 650e6}

func TestFreeIsMinimumPlusIdleHeld(t *testing.T) {
	usage := 5e9
	free := Free(Sources{
		Quota:       &Resources{CPU: 10, Memory: 100e9},
		Nodes:       &Resources{CPU: 50, Memory: 20e9},
		UsageMemory: &usage,
	}, Resources{CPU: 1, Memory: 1e9})
	if free.CPU != 11 || free.Memory != 6e9 {
		t.Fatalf("got %+v", free)
	}
	if f := Free(Sources{}, Resources{}); !math.IsInf(f.CPU, 1) {
		t.Fatalf("no source means no limit, got %+v", f)
	}
	neg := Free(Sources{Quota: &Resources{CPU: -2, Memory: -1}}, Resources{CPU: 1, Memory: 1})
	if neg.CPU != 1 || neg.Memory != 1 {
		t.Fatalf("an overrun quota leaves only what idle pods hold, got %+v", neg)
	}
}

func TestAllocateEnough(t *testing.T) {
	g := Allocate(Unlimited, []Demand{{App: "a", Floor: 1, Desired: 10, Pod: java}})
	if g["a"].Value != 10 || g["a"].Clamped {
		t.Fatalf("%+v", g)
	}
}

func TestAllocateFloorFirstThenProportional(t *testing.T) {
	// 6 CPU: floors take 2 x 0.6 = 1.2, 4.8 CPU = 8 pods left for a deficit
	// of 10 + 6 = 16, so each gets half of its deficit.
	g := Allocate(Resources{CPU: 6, Memory: 1e12}, []Demand{
		{App: "a", Floor: 1, Desired: 11, Pod: java},
		{App: "b", Floor: 1, Desired: 7, Pod: java},
		{App: "c", Floor: 0, Desired: 0, Pod: java},
	})
	if g["a"].Value != 6 || g["b"].Value != 4 || g["c"].Value != 0 {
		t.Fatalf("%+v", g)
	}
	if !g["a"].Clamped || !g["b"].Clamped || g["c"].Clamped {
		t.Fatalf("clamp flags %+v", g)
	}
}

func TestAllocateFloorsEvenOverBudget(t *testing.T) {
	g := Allocate(Resources{CPU: 0.5, Memory: 1e12}, []Demand{{App: "a", Floor: 3, Desired: 9, Pod: java}})
	if g["a"].Value != 3 {
		t.Fatalf("floor must hold, got %+v", g)
	}
}

func TestAllocateScarcestResourceWins(t *testing.T) {
	// CPU allows 10, memory 650e6*4 allows 4.
	g := Allocate(Resources{CPU: 6, Memory: 4 * 650e6}, []Demand{{App: "a", Desired: 10, Pod: java}})
	if g["a"].Value != 4 {
		t.Fatalf("%+v", g)
	}
}
