package main

import "testing"

func TestOverallProgressAcrossRetriesAndNewDependencies(t *testing.T) {
	w := newWorkProgress(true)
	w.begin(stageBaseline, 2)
	w.advance()
	if w.overall != 10 {
		t.Fatalf("baseline estimate = %d", w.overall)
	}
	w.finish()
	w.begin(stageTarget, 4)
	w.finish()
	w.begin(stageDiscovery, 48)
	for range 12 {
		w.advance()
	}
	if w.overall != 45 || phasePercent(w.completed, w.total) != 25 {
		t.Fatalf("discovery progress = %+v", w)
	}
	w.finish()
	w.begin(stageDependencies, 3)
	w.resolve("a")
	w.resolve("a") // Splits or retries must not count a dependency twice.
	if w.completed != 1 || w.overall != 66 {
		t.Fatalf("dependency progress = %+v", w)
	}
	w.resolve("b")
	w.resolve("c")
	w.begin(stageDiscovery, 1) // Newly introduced dependency.
	if w.overall != 80 || w.completed != 0 {
		t.Fatalf("estimate rewound on discovery: %+v", w)
	}
	w.advance()
	w.begin(stageDependencies, 1)
	w.resolve("new")
	w.begin(stageFinal, 4)
	w.finish()
	if w.overall != 99 {
		t.Fatalf("reported success before final completion: %+v", w)
	}
}

func TestProgressWithoutDependencyUpgrades(t *testing.T) {
	w := newWorkProgress(false)
	for _, stage := range []progressStage{stageBaseline, stageTarget, stageFinal} {
		w.begin(stage, 1)
		w.advance()
	}
	if len(w.stages) != 3 || w.overall != 99 {
		t.Fatalf("unexpected enabled stage weights: %+v", w)
	}
	w.begin(stageDiscovery, 0)
	if phasePercent(0, 0) != 0 || w.overall != 99 {
		t.Fatalf("zero total broke progress: %+v", w)
	}
}

func TestEnabledCheckCounts(t *testing.T) {
	for _, tc := range []struct {
		opts                    options
		baseline, target, final int
	}{
		{options{skipTests: true, skipVet: true}, 1, 2, 4},
		{options{upgradeDeps: true, skipTests: true, skipVet: true}, 1, 4, 4},
		{options{upgradeDeps: true}, 2, 7, 7},
		{options{upgradeDeps: true, skipTests: true}, 1, 5, 5},
	} {
		a := &app{opts: tc.opts}
		if a.baselineUnits() != tc.baseline || a.targetUnits() != tc.target || a.finalUnits() != tc.final {
			t.Errorf("incorrect checks for %+v: %d/%d/%d", tc.opts, a.baselineUnits(), a.targetUnits(), a.finalUnits())
		}
	}
}
