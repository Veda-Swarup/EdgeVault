package clock

import "time"

// Clock is an interface that wraps time.Now so that tests can inject a
// deterministic time source without patching globals.
type Clock interface {
	Now() time.Time
}

// Real is the production Clock backed by the system clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

// Fake is a test Clock whose time advances only when you call Advance.
type Fake struct {
	current time.Time
}

func NewFake(t time.Time) *Fake { return &Fake{current: t} }

func (f *Fake) Now() time.Time          { return f.current }
func (f *Fake) Advance(d time.Duration) { f.current = f.current.Add(d) }
func (f *Fake) Set(t time.Time)         { f.current = t }
