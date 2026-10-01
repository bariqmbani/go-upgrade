package main

import "time"

type progressStage int

const (
	stageBaseline progressStage = iota
	stageTarget
	stageDiscovery
	stageDependencies
	stageFinal
)

func (s progressStage) label() string {
	return [...]string{"Baseline", "Target setup", "Metadata", "Dependencies", "Final checks"}[s]
}

// Overall completion is a work estimate, not an estimate of remaining time.
// Each enabled stage has the same weight. Revisiting discovery or validation
// may add work, but must never rewind the displayed overall percentage.
type workProgress struct {
	stages           []progressStage
	stage            progressStage
	completed, total int
	overall          int
	started          time.Time
	resolved         map[string]bool
}

func newWorkProgress(upgradeDeps bool) workProgress {
	stages := []progressStage{stageBaseline, stageTarget}
	if upgradeDeps {
		stages = append(stages, stageDiscovery, stageDependencies)
	}
	stages = append(stages, stageFinal)
	return workProgress{stages: stages}
}

func (w *workProgress) begin(stage progressStage, total int) {
	w.stage, w.total, w.completed = stage, max(0, total), 0
	w.started = time.Now()
	w.resolved = make(map[string]bool)
	w.update()
}

func (w *workProgress) advance() {
	w.completed = min(w.total, w.completed+1)
	w.update()
}

func (w *workProgress) resolve(module string) {
	if w.resolved[module] {
		return
	}
	w.resolved[module] = true
	w.advance()
}

func (w *workProgress) finish() {
	w.completed = w.total
	w.update()
}

func (w *workProgress) update() {
	for index, stage := range w.stages {
		if stage != w.stage {
			continue
		}
		value := index * 100 / len(w.stages)
		if w.total > 0 {
			value = 100 * (index*w.total + w.completed) / (len(w.stages) * w.total)
		}
		w.overall = max(w.overall, min(99, value))
		return
	}
}

func phasePercent(completed, total int) int {
	if total <= 0 {
		return 0
	}
	return min(100, max(0, completed)*100/total)
}
