package sync

import "time"

// Cadence is how often a task is worth refetching.
//
// It is a minimum age rather than a schedule. A schedule needs a clock to have
// fired at the right moment; a minimum age lets a run that started late, or a
// second run of the day, ask the same question and get a correct answer.
type Cadence struct {
	// Name is the cadence's label, used in the state key and in logs.
	Name string
	// MinAge is how old the last success must be before the task is due.
	MinAge time.Duration
}

// The cadences the donors use. A caller with different needs declares its own;
// these exist so the common two are not restated in every project.
var (
	// Daily refetches once per calendar day's worth of elapsed time, with
	// enough slack that a run at 09:00 and one at 08:55 the next day do not
	// skip.
	Daily = Cadence{Name: "daily", MinAge: 20 * time.Hour}
	// Weekly is for reference data that changes on a corporate timetable
	// rather than a market one.
	Weekly = Cadence{Name: "weekly", MinAge: 6 * 24 * time.Hour}
	// Always refetches every pass, for the intraday tasks whose answer
	// changes minute to minute.
	Always = Cadence{Name: "always", MinAge: 0}
)

// Due reports whether a task should be refetched.
//
// A task that has never succeeded is always due — a zero lastSuccess means "no
// record", not "succeeded at the epoch", and treating it as the latter would
// leave a fresh install permanently up to date and permanently empty.
//
// force overrides the gate, which is what an operator re-running a failed sync
// needs; without it the run they just triggered would report everything as
// still fresh and do nothing.
func Due(cadence Cadence, lastSuccess, now time.Time, force bool) bool {
	if force || lastSuccess.IsZero() {
		return true
	}
	if cadence.MinAge <= 0 {
		return true
	}
	return !now.Before(lastSuccess.Add(cadence.MinAge))
}
