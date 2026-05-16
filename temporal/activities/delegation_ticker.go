package activities

import "time"

// secondsTicker is a tiny abstraction so tests can swap the real
// time.Ticker for a deterministic one. The single-method interface keeps
// the production cost zero.
type secondsTicker interface {
	C() <-chan time.Time
	Stop()
}

type realSecondsTicker struct {
	t *time.Ticker
}

func (r *realSecondsTicker) C() <-chan time.Time { return r.t.C }
func (r *realSecondsTicker) Stop()                { r.t.Stop() }

func newSecondsTicker(seconds int) secondsTicker {
	return &realSecondsTicker{t: time.NewTicker(time.Duration(seconds) * time.Second)}
}
