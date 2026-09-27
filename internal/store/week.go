package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Week is an ISO 8601 week: Monday to Sunday, numbered so that week 1 is the
// one with the year's first Thursday in it. That is the numbering calendars
// and payroll use in most of Europe, and it means a week never belongs to two
// years: 29 Dec 2025 to 4 Jan 2026 is all of it 2026-W01.
//
// In JSON a week is its name, "2026-W39".
type Week struct {
	Year, Num int
}

func (w Week) MarshalText() ([]byte, error) { return []byte(w.String()), nil }

func (w *Week) UnmarshalText(b []byte) error {
	parsed, err := ParseWeek(string(b))
	if err == nil {
		*w = parsed
	}
	return err
}

// WeekOf is the week a day falls in.
func WeekOf(t time.Time) Week {
	y, w := t.ISOWeek()
	return Week{y, w}
}

// ParseWeek reads a week written the ISO way, "2026-W39". The W may be lower
// case and the number may drop its leading zero.
func ParseWeek(s string) (Week, error) {
	y, w, ok := strings.Cut(strings.ToUpper(strings.TrimSpace(s)), "-W")
	if !ok {
		return Week{}, ErrInvalid
	}
	year, err1 := strconv.Atoi(y)
	num, err2 := strconv.Atoi(w)
	if err1 != nil || err2 != nil || year < 1000 || year > 9999 || num < 1 || num > weeksIn(year) {
		return Week{}, ErrInvalid
	}
	return Week{year, num}, nil
}

// weeksIn is 52 or 53: 28 December is always in a year's last week.
func weeksIn(year int) int {
	_, w := time.Date(year, 12, 28, 0, 0, 0, 0, time.UTC).ISOWeek()
	return w
}

func (w Week) String() string { return fmt.Sprintf("%d-W%02d", w.Year, w.Num) }

// Monday is the week's first day, at midnight UTC. Only its date matters.
func (w Week) Monday() time.Time {
	// 4 January is always in week 1, so step back from it to that week's
	// Monday and forward from there.
	jan4 := time.Date(w.Year, 1, 4, 0, 0, 0, 0, time.UTC)
	back := (int(jan4.Weekday()) + 6) % 7 // days since Monday
	return jan4.AddDate(0, 0, -back+(w.Num-1)*7)
}

// Days are the week's seven dates, Monday first, as YYYY-MM-DD.
func (w Week) Days() []string {
	m := w.Monday()
	days := make([]string, 7)
	for i := range days {
		days[i] = m.AddDate(0, 0, i).Format(dateLayout)
	}
	return days
}

// First and Last are the week's Monday and Sunday, as YYYY-MM-DD.
func (w Week) First() string { return w.Monday().Format(dateLayout) }
func (w Week) Last() string  { return w.Monday().AddDate(0, 0, 6).Format(dateLayout) }

func (w Week) Prev() Week { return WeekOf(w.Monday().AddDate(0, 0, -7)) }
func (w Week) Next() Week { return WeekOf(w.Monday().AddDate(0, 0, 7)) }

// Contains reports whether a YYYY-MM-DD date is in the week.
func (w Week) Contains(date string) bool { return date >= w.First() && date <= w.Last() }

// Duration writes a number of minutes the way a timesheet does: "2h 30m",
// "45m", "8h", and "0m" for nothing at all.
func Duration(minutes int) string {
	h, m := minutes/60, minutes%60
	switch {
	case h == 0:
		return strconv.Itoa(m) + "m"
	case m == 0:
		return strconv.Itoa(h) + "h"
	default:
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	}
}

// parseClock reads "09:00" as minutes after midnight.
func parseClock(s string) (int, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// clock writes minutes after midnight as "09:00".
func clock(min int) string { return fmt.Sprintf("%02d:%02d", min/60, min%60) }
