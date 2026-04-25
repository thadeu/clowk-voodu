package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed 5-field cron expression. Holds bitmasks per
// field so Next() walks forward by minute and tests "does this minute
// match?" with five constant-time bit checks.
//
// Field grammar:
//
//	minute        0-59
//	hour          0-23
//	day of month  1-31
//	month         1-12
//	day of week   0-6  (0 = Sunday)
//
// Each field accepts a literal, "*", a range "a-b", a step "*\/n" or
// "a-b/n", or a comma-separated list of any of the above.
//
// When both day-of-month and day-of-week are restricted (neither is
// "*"), Vixie-cron semantics apply: a tick fires when EITHER matches.
// Otherwise both fields gate by AND. M4 keeps the simpler pure-AND
// behaviour to start; the OR semantics can be slotted in later under
// the same Schedule type without API churn.
type Schedule struct {
	expr      string
	minutes   uint64 // bits 0..59
	hours     uint32 // bits 0..23
	doms      uint32 // bits 1..31 (bit 0 unused)
	months    uint16 // bits 1..12 (bit 0 unused)
	dows      uint8  // bits 0..6
	loc       *time.Location
	domStar   bool
	dowStar   bool
}

// ParseSchedule decodes a cron expression. Whitespace separates
// fields; multiple spaces collapse. Predefined macros (@hourly, @daily,
// @weekly, @monthly, @yearly) are expanded before parsing.
//
// tz is the timezone the expression is interpreted in. Empty → UTC.
func ParseSchedule(expr, tz string) (*Schedule, error) {
	loc := time.UTC

	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("invalid timezone %q: %w", tz, err)
		}

		loc = l
	}

	canonical := strings.TrimSpace(expr)

	// Expand the @-prefixed macros to their 5-field equivalents. This
	// keeps the rest of the parser free of special cases — every code
	// path past this point sees a regular five-field expression.
	switch canonical {
	case "@yearly", "@annually":
		canonical = "0 0 1 1 *"
	case "@monthly":
		canonical = "0 0 1 * *"
	case "@weekly":
		canonical = "0 0 * * 0"
	case "@daily", "@midnight":
		canonical = "0 0 * * *"
	case "@hourly":
		canonical = "0 * * * *"
	}

	fields := strings.Fields(canonical)
	if len(fields) != 5 {
		return nil, fmt.Errorf("schedule %q: expected 5 fields, got %d", expr, len(fields))
	}

	s := &Schedule{expr: expr, loc: loc}

	mins, err := parseField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}

	s.minutes = uint64(mins)

	hrs, err := parseField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}

	s.hours = uint32(hrs)

	doms, err := parseField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}

	s.doms = uint32(doms)
	s.domStar = fields[2] == "*"

	months, err := parseField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}

	s.months = uint16(months)

	// Day-of-week accepts both 0 and 7 for Sunday — fold 7 into 0 so the
	// runtime check is a single bit lookup. Apply a small alias map for
	// Sun..Sat too.
	dowField := normalizeDOW(fields[4])

	dows, err := parseField(dowField, 0, 7)
	if err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}

	if dows&(1<<7) != 0 {
		dows = (dows &^ (1 << 7)) | 1 // fold 7 → 0
	}

	s.dows = uint8(dows)
	s.dowStar = fields[4] == "*"

	return s, nil
}

// Next returns the first time at or after `after` that matches the
// schedule, in the schedule's timezone. The returned time has zero
// seconds/nanos — cron resolution is per-minute.
//
// To avoid an infinite loop on a malformed mask (every field must have
// at least one set bit; ParseSchedule rejects empty fields, so this
// can't happen for parsed Schedules), the search caps at 4 years of
// minutes and returns the zero time if no match is found. 4y is enough
// for "Feb 29 every leap year" on a non-leap-year start.
func (s *Schedule) Next(after time.Time) time.Time {
	t := after.In(s.loc).Truncate(time.Minute).Add(time.Minute)

	limit := t.Add(4 * 366 * 24 * time.Hour)

	for t.Before(limit) {
		if s.match(t) {
			return t
		}

		t = t.Add(time.Minute)
	}

	return time.Time{}
}

// match returns true when t (already in s.loc) ticks the schedule.
// Five constant-time bit checks — fast enough that Next's per-minute
// scan is fine for this PaaS use case (a cluster has hundreds of
// cronjobs at most, not millions).
func (s *Schedule) match(t time.Time) bool {
	if s.minutes&(1<<uint(t.Minute())) == 0 {
		return false
	}

	if s.hours&(1<<uint(t.Hour())) == 0 {
		return false
	}

	if s.months&(1<<uint(t.Month())) == 0 {
		return false
	}

	domHit := s.doms&(1<<uint(t.Day())) != 0
	dowHit := s.dows&(1<<uint(t.Weekday())) != 0

	// Vixie semantics: if both day-of-month and day-of-week are
	// restricted, fire on either match. If exactly one is restricted,
	// only that one gates (its mask). If both are "*", both pass and
	// the AND below is true trivially.
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowHit
	case s.dowStar:
		return domHit
	default:
		return domHit || dowHit
	}
}

// String returns the canonical expression so logs identify which
// schedule fired without the caller threading the cronjob name through.
func (s *Schedule) String() string { return s.expr }

// parseField decodes one cron field into a bitmask covering [min, max].
// Returns a uint64 sized to match the largest field (minutes, 0..59) so
// callers can downcast to uint32/uint16/uint8 as appropriate.
func parseField(field string, min, max int) (uint64, error) {
	if field == "" {
		return 0, fmt.Errorf("empty field")
	}

	var bits uint64

	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)

		if part == "" {
			return 0, fmt.Errorf("empty list element")
		}

		mask, err := parseRange(part, min, max)
		if err != nil {
			return 0, err
		}

		bits |= mask
	}

	return bits, nil
}

// parseRange handles a single comma-separated piece: "*", "a", "a-b",
// "*\/n", "a-b/n". Returns the bitmask of values it expands to.
func parseRange(part string, min, max int) (uint64, error) {
	step := 1

	if i := strings.IndexByte(part, '/'); i >= 0 {
		stepStr := part[i+1:]
		part = part[:i]

		n, err := strconv.Atoi(stepStr)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid step %q", stepStr)
		}

		step = n
	}

	var lo, hi int

	switch {
	case part == "*":
		lo, hi = min, max
	case strings.IndexByte(part, '-') > 0:
		dash := strings.IndexByte(part, '-')

		l, err := strconv.Atoi(part[:dash])
		if err != nil {
			return 0, fmt.Errorf("invalid range start %q", part[:dash])
		}

		h, err := strconv.Atoi(part[dash+1:])
		if err != nil {
			return 0, fmt.Errorf("invalid range end %q", part[dash+1:])
		}

		lo, hi = l, h
	default:
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0, fmt.Errorf("invalid value %q", part)
		}

		lo, hi = n, n
	}

	if lo < min || hi > max || lo > hi {
		return 0, fmt.Errorf("range %d-%d out of bounds [%d, %d]", lo, hi, min, max)
	}

	var bits uint64

	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
	}

	return bits, nil
}

// normalizeDOW maps Sun/Mon/.../Sat (case-insensitive) to 0..6 so the
// rest of the parser only deals with numbers. Compound expressions like
// "Mon-Fri" or "Sun,Wed" get rewritten piece by piece.
func normalizeDOW(field string) string {
	aliases := map[string]string{
		"sun": "0", "mon": "1", "tue": "2", "wed": "3",
		"thu": "4", "fri": "5", "sat": "6",
	}

	out := strings.ToLower(field)

	for name, num := range aliases {
		out = strings.ReplaceAll(out, name, num)
	}

	return out
}
