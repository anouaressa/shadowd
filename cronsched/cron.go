// Package cronsched parses standard 5-field cron expressions
// ("minute hour day-of-month month day-of-week") and runs a callback each
// time the schedule fires. It also accepts the common "@every <duration>"
// shorthand (e.g. "@every 30m") for simple interval-based snapshots.
//
// This is intentionally small: it supports '*', '*/n' (step), 'a-b'
// (range), 'a-b/n' (stepped range) and 'a,b,c' (list) per field — enough
// for real-world schedules like "0 * * * *" (hourly) or "*/15 9-18 * * 1-5"
// (every 15 min, business hours, weekdays) — without pulling in a
// third-party cron library.
package cronsched

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type field struct {
	min, max int
	allowed  map[int]bool // nil means "every value in range" (i.e. "*")
}

type Schedule struct {
	minute, hour, dom, month, dow field
	every                         time.Duration // set only for "@every ..." schedules
}

// Parse compiles a cron expression. It returns an error for anything it
// can't confidently parse rather than silently guessing.
func Parse(expr string) (*Schedule, error) {
	expr = strings.TrimSpace(expr)

	if strings.HasPrefix(expr, "@every ") {
		d, err := time.ParseDuration(strings.TrimPrefix(expr, "@every "))
		if err != nil {
			return nil, fmt.Errorf("invalid @every duration: %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("@every duration must be positive")
		}
		return &Schedule{every: d}, nil
	}

	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("expected 5 fields (minute hour dom month dow), got %d in %q", len(parts), expr)
	}

	ranges := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	fields := make([]field, 5)
	for i, p := range parts {
		f, err := parseField(p, ranges[i][0], ranges[i][1])
		if err != nil {
			return nil, fmt.Errorf("field %d (%q): %w", i+1, p, err)
		}
		fields[i] = f
	}

	return &Schedule{
		minute: fields[0], hour: fields[1], dom: fields[2], month: fields[3], dow: fields[4],
	}, nil
}

func parseField(spec string, min, max int) (field, error) {
	f := field{min: min, max: max}
	if spec == "*" {
		return f, nil // allowed == nil means "match anything in range"
	}

	f.allowed = map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		base, step := part, 1
		if idx := strings.Index(part, "/"); idx != -1 {
			var err error
			step, err = strconv.Atoi(part[idx+1:])
			if err != nil || step <= 0 {
				return f, fmt.Errorf("bad step in %q", part)
			}
			base = part[:idx]
		}

		var lo, hi int
		switch {
		case base == "*":
			lo, hi = min, max
		case strings.Contains(base, "-"):
			bounds := strings.SplitN(base, "-", 2)
			var err error
			lo, err = strconv.Atoi(bounds[0])
			if err != nil {
				return f, fmt.Errorf("bad range start %q", bounds[0])
			}
			hi, err = strconv.Atoi(bounds[1])
			if err != nil {
				return f, fmt.Errorf("bad range end %q", bounds[1])
			}
		default:
			v, err := strconv.Atoi(base)
			if err != nil {
				return f, fmt.Errorf("bad value %q", base)
			}
			lo, hi = v, v
		}

		if lo < min || hi > max || lo > hi {
			return f, fmt.Errorf("value out of range [%d,%d]", min, max)
		}
		for v := lo; v <= hi; v += step {
			f.allowed[v] = true
		}
	}
	return f, nil
}

func (f field) matches(v int) bool {
	return f.allowed == nil || f.allowed[v]
}

// Next returns the next time at or after `after` that the schedule fires,
// truncated to the minute (cron's usual resolution).
func (s *Schedule) Next(after time.Time) time.Time {
	if s.every > 0 {
		return after.Add(s.every)
	}

	t := after.Truncate(time.Minute).Add(time.Minute)
	// Bounded search: cron schedules repeat at least yearly, so five years
	// of minutes is a safe ceiling that still fails fast on a bad schedule
	// (e.g. Feb 30) instead of looping forever.
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.minute.matches(t.Minute()) && s.hour.matches(t.Hour()) &&
			s.dom.matches(t.Day()) && s.month.matches(int(t.Month())) &&
			s.dow.matches(int(t.Weekday())) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{} // no match found within the search horizon
}

// Run blocks, calling fn every time the schedule fires, until stop is
// closed. Errors from fn are passed to onErr rather than stopping the loop
// — one failed snapshot shouldn't take down the whole schedule.
func (s *Schedule) Run(stop <-chan struct{}, fn func() error, onErr func(error)) {
	for {
		next := s.Next(time.Now())
		if next.IsZero() {
			onErr(fmt.Errorf("cron: no matching time found within search horizon"))
			return
		}

		timer := time.NewTimer(time.Until(next))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			if err := fn(); err != nil {
				onErr(err)
			}
		}
	}
}
