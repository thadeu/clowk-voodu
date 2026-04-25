package controller

import (
	"testing"
	"time"
)

// TestParseScheduleHappyPaths covers the field-grammar cases an
// operator is most likely to write. Each case returns the next fire
// time after a fixed `from` so the assertion is exact rather than
// fuzzy.
func TestParseScheduleHappyPaths(t *testing.T) {
	from := time.Date(2026, 4, 24, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		expr     string
		wantNext time.Time
	}{
		{
			name:     "every minute",
			expr:     "* * * * *",
			wantNext: from.Add(time.Minute),
		},
		{
			name:     "fixed minute every hour",
			expr:     "30 * * * *",
			wantNext: time.Date(2026, 4, 24, 10, 30, 0, 0, time.UTC),
		},
		{
			name:     "every 15 minutes",
			expr:     "*/15 * * * *",
			wantNext: time.Date(2026, 4, 24, 10, 15, 0, 0, time.UTC),
		},
		{
			name:     "daily at 2:30",
			expr:     "30 2 * * *",
			wantNext: time.Date(2026, 4, 25, 2, 30, 0, 0, time.UTC),
		},
		{
			name:     "weekly on Monday at 9am",
			expr:     "0 9 * * 1",
			wantNext: time.Date(2026, 4, 27, 9, 0, 0, 0, time.UTC),
		},
		{
			name:     "monthly on the 1st",
			expr:     "0 0 1 * *",
			wantNext: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "ranges and lists",
			expr:     "0 9-17 * * 1-5",
			wantNext: time.Date(2026, 4, 24, 11, 0, 0, 0, time.UTC),
		},
		{
			name:     "named DOW",
			expr:     "0 9 * * Mon",
			wantNext: time.Date(2026, 4, 27, 9, 0, 0, 0, time.UTC),
		},
		{
			name:     "macro @daily",
			expr:     "@daily",
			wantNext: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "macro @hourly",
			expr:     "@hourly",
			wantNext: time.Date(2026, 4, 24, 11, 0, 0, 0, time.UTC),
		},
		{
			name:     "macro @yearly",
			expr:     "@yearly",
			wantNext: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseSchedule(tc.expr, "")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			got := s.Next(from)
			if !got.Equal(tc.wantNext) {
				t.Errorf("Next: got %s, want %s", got.Format(time.RFC3339), tc.wantNext.Format(time.RFC3339))
			}
		})
	}
}

// TestParseScheduleTimezone checks the schedule fires at local time in
// the configured zone, not UTC. Crucial for "every day at 3am
// America/Sao_Paulo" — without timezone support that becomes 3am UTC,
// which is midnight in São Paulo, off by three hours.
func TestParseScheduleTimezone(t *testing.T) {
	s, err := ParseSchedule("0 3 * * *", "America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}

	// Friday April 24th 2026, 02:00 UTC → 23:00 (Apr 23rd) in São Paulo.
	// The next 3am São Paulo is Apr 24th 03:00 BRT == 06:00 UTC.
	from := time.Date(2026, 4, 24, 2, 0, 0, 0, time.UTC)
	want := time.Date(2026, 4, 24, 6, 0, 0, 0, time.UTC)

	got := s.Next(from)
	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got.UTC(), want)
	}
}

// TestParseScheduleVixieORSemantics locks in the OR rule when both
// day-of-month and day-of-week are restricted. Classic example:
// "every Monday OR the 15th" — fires on both, not the intersection.
func TestParseScheduleVixieORSemantics(t *testing.T) {
	s, err := ParseSchedule("0 9 15 * 1", "")
	if err != nil {
		t.Fatal(err)
	}

	// April 24th 2026 is Friday; the 15th was a Wednesday already past;
	// next match is Monday April 27th.
	from := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	got := s.Next(from)

	want := time.Date(2026, 4, 27, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Mon match: got %s, want %s", got, want)
	}

	// Then May 4th (Monday). Stepping a day past the previous gets us
	// the same Monday again, so step a week.
	got = s.Next(got)

	want = time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("next Mon: got %s, want %s", got, want)
	}

	// Now jump to the 14th and the next match should be the 15th
	// (day-of-month branch of the OR).
	from = time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	got = s.Next(from)

	want = time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("DOM match: got %s, want %s", got, want)
	}
}

// TestParseScheduleRejectsBadInput ensures malformed expressions error
// at parse time rather than producing a Schedule that silently never
// fires. The error message must name the offending field.
func TestParseScheduleRejectsBadInput(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"", "expected 5 fields"},
		{"* * * *", "expected 5 fields"},
		{"60 * * * *", "minute"},
		{"* 24 * * *", "hour"},
		{"* * 0 * *", "day of month"},
		{"* * * 13 *", "month"},
		{"* * * * 8", "day of week"},
		{"*/0 * * * *", "minute"},
		{"5-3 * * * *", "minute"},
		{"abc * * * *", "minute"},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			_, err := ParseSchedule(tc.expr, "")
			if err == nil {
				t.Fatalf("expected error for %q", tc.expr)
			}

			if got := err.Error(); !contains(got, tc.want) {
				t.Errorf("error for %q should mention %q, got %q", tc.expr, tc.want, got)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (substr == "" || indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}

	return -1
}
