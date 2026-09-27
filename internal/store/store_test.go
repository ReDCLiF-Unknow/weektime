package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func user(t *testing.T, s *Store, name string) User {
	t.Helper()
	u, _, err := s.CreateUser(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustWeek(t *testing.T, s string) Week {
	t.Helper()
	w, err := ParseWeek(s)
	if err != nil {
		t.Fatalf("ParseWeek(%q): %v", s, err)
	}
	return w
}

func TestWeeksAreISOWeeks(t *testing.T) {
	for _, c := range []struct{ week, monday, sunday string }{
		{"2026-W39", "2026-09-21", "2026-09-27"},
		// Week 1 is the one with the year's first Thursday, so it can start in
		// December, and the last days of a year can belong to the next one.
		{"2026-W01", "2025-12-29", "2026-01-04"},
		{"2020-W53", "2020-12-28", "2021-01-03"},
		{"2027-W1", "2027-01-04", "2027-01-10"},
	} {
		w := mustWeek(t, c.week)
		if w.First() != c.monday || w.Last() != c.sunday {
			t.Errorf("%s runs %s to %s, want %s to %s", c.week, w.First(), w.Last(), c.monday, c.sunday)
		}
		if got := WeekOf(w.Monday().AddDate(0, 0, 3)); got != w {
			t.Errorf("the Thursday of %s is in %s", c.week, got)
		}
	}
	for _, bad := range []string{"", "2026", "2026-W00", "2026-W54", "2025-W53", "twenty-W3", "2026-39"} {
		if _, err := ParseWeek(bad); err == nil {
			t.Errorf("ParseWeek(%q) accepted it", bad)
		}
	}
	if w := mustWeek(t, "2026-w1"); w.String() != "2026-W01" {
		t.Errorf("2026-w1 is written %s", w)
	}
	if w := mustWeek(t, "2026-W01"); w.Prev().String() != "2025-W52" || w.Next().String() != "2026-W02" {
		t.Errorf("around 2026-W01: %s and %s", w.Prev(), w.Next())
	}
}

func TestAWeekIsItsNameInJSON(t *testing.T) {
	b, _ := json.Marshal(struct{ W Week }{mustWeek(t, "2026-W39")})
	if string(b) != `{"W":"2026-W39"}` {
		t.Errorf("marshalled as %s", b)
	}
	var back struct{ W Week }
	if err := json.Unmarshal(b, &back); err != nil || back.W.String() != "2026-W39" {
		t.Errorf("unmarshalled as %v, %v", back.W, err)
	}
}

func TestDurations(t *testing.T) {
	for min, want := range map[int]string{0: "0m", 45: "45m", 60: "1h", 150: "2h 30m", 485: "8h 5m"} {
		if got := Duration(min); got != want {
			t.Errorf("Duration(%d) = %q, want %q", min, got, want)
		}
	}
}

func TestAnEntryIsWorkedOutFromItsTimes(t *testing.T) {
	s := open(t)
	alex := user(t, s, "Alex")
	e, err := s.AddEntry(alex.ID, "2026-10-14", "09:00", "11:30", "  Client call +\n proposal draft ")
	if err != nil {
		t.Fatal(err)
	}
	if e.Minutes != 150 || e.Duration() != "2h 30m" {
		t.Errorf("09:00 to 11:30 came out as %d minutes (%s)", e.Minutes, e.Duration())
	}
	if e.Note != "Client call + proposal draft" {
		t.Errorf("the note is kept to one line, but came out as %q", e.Note)
	}
	if e.Start != "09:00" || e.End != "11:30" || e.Date != "2026-10-14" {
		t.Errorf("stored as %+v", e)
	}
}

func TestBadEntriesAreRefused(t *testing.T) {
	s := open(t)
	alex := user(t, s, "Alex")
	long := make([]byte, maxNoteLen+1)
	for i := range long {
		long[i] = 'x'
	}
	for _, c := range []struct{ what, date, start, end, note string }{
		{"no date", "", "09:00", "10:00", ""},
		{"not a date", "2026-02-30", "09:00", "10:00", ""},
		{"not a time", "2026-10-14", "9am", "10:00", ""},
		{"ends before it starts", "2026-10-14", "11:00", "10:00", ""},
		{"takes no time", "2026-10-14", "10:00", "10:00", ""},
		{"note too long", "2026-10-14", "09:00", "10:00", string(long)},
	} {
		if _, err := s.AddEntry(alex.ID, c.date, c.start, c.end, c.note); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", c.what, err)
		}
	}
}

func TestTimesheetHasEveryDayAndTheTotals(t *testing.T) {
	s := open(t)
	alex := user(t, s, "Alex")
	sam := user(t, s, "Sam")
	add := func(u User, date, start, end string) {
		t.Helper()
		if _, err := s.AddEntry(u.ID, date, start, end, ""); err != nil {
			t.Fatal(err)
		}
	}
	add(alex, "2026-09-22", "13:00", "17:00") // Tuesday, added out of order
	add(alex, "2026-09-22", "09:00", "11:30")
	add(alex, "2026-09-24", "10:00", "10:45")
	add(alex, "2026-09-20", "09:00", "17:00") // the Sunday before: another week
	add(sam, "2026-09-22", "08:00", "18:00")  // somebody else's

	ts, err := s.Timesheet(alex.ID, mustWeek(t, "2026-W39"))
	if err != nil {
		t.Fatal(err)
	}
	if ts.Name != "Alex" || len(ts.Days) != 7 || ts.Days[0].Date != "2026-09-21" || ts.Days[6].Date != "2026-09-27" {
		t.Fatalf("got %s with days %v", ts.Name, ts.Days)
	}
	tue := ts.Days[1]
	if len(tue.Entries) != 2 || tue.Entries[0].Start != "09:00" || tue.Minutes != 390 {
		t.Errorf("Tuesday: %+v", tue)
	}
	if ts.Days[3].Minutes != 45 || ts.Minutes != 435 || ts.DaysWorked() != 2 || len(ts.Entries()) != 3 {
		t.Errorf("totals: Thursday %d, week %d, %d days worked, %d entries",
			ts.Days[3].Minutes, ts.Minutes, ts.DaysWorked(), len(ts.Entries()))
	}
	if ts.Days[0].Entries == nil {
		t.Error("an empty day has a nil list of entries, which is null in JSON")
	}
}

func TestEntriesArePrivate(t *testing.T) {
	s := open(t)
	alex := user(t, s, "Alex")
	sam := user(t, s, "Sam")
	e, _ := s.AddEntry(alex.ID, "2026-10-14", "09:00", "10:00", "mine")
	note := "Sam's now"
	if _, err := s.Entry(e.ID, sam.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Sam read Alex's entry: %v", err)
	}
	if _, err := s.UpdateEntry(e.ID, sam.ID, EntryUpdate{Note: &note}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Sam edited Alex's entry: %v", err)
	}
	if err := s.DeleteEntry(e.ID, sam.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Sam deleted Alex's entry: %v", err)
	}
	if got, _ := s.Entry(e.ID, alex.ID); got.Note != "mine" {
		t.Errorf("Alex's entry became %+v", got)
	}
}

func TestEditingChangesOnlyWhatIsGiven(t *testing.T) {
	s := open(t)
	alex := user(t, s, "Alex")
	e, _ := s.AddEntry(alex.ID, "2026-10-14", "09:00", "10:00", "call")
	end := "12:15"
	e, err := s.UpdateEntry(e.ID, alex.ID, EntryUpdate{End: &end})
	if err != nil {
		t.Fatal(err)
	}
	if e.Start != "09:00" || e.End != "12:15" || e.Minutes != 195 || e.Note != "call" {
		t.Errorf("after moving the end: %+v", e)
	}
	early := "08:00"
	if _, err := s.UpdateEntry(e.ID, alex.ID, EntryUpdate{End: &early}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an edit that ends before the start: %v", err)
	}
}

func TestADeletedEntryCanBeRestored(t *testing.T) {
	s := open(t)
	alex := user(t, s, "Alex")
	e, _ := s.AddEntry(alex.ID, "2026-10-14", "09:00", "10:00", "")
	if err := s.DeleteEntry(e.ID, alex.ID); err != nil {
		t.Fatal(err)
	}
	ts, _ := s.Timesheet(alex.ID, WeekOf(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)))
	if ts.Minutes != 0 {
		t.Errorf("a deleted entry still counts: %d minutes", ts.Minutes)
	}
	if _, err := s.RestoreEntry(e.ID, alex.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreEntry(e.ID, alex.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("restoring something not in the trash: %v", err)
	}
	// Past a day it is gone for good.
	s.DeleteEntry(e.ID, alex.ID)
	s.db.Exec(`UPDATE entries SET deleted_at = datetime('now', '-2 days')`)
	if err := s.purgeTrash(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreEntry(e.ID, alex.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("restored something deleted two days ago: %v", err)
	}
}

func TestSharingAWeek(t *testing.T) {
	s := open(t)
	alex := user(t, s, "Alex")
	sam := user(t, s, "Sam")
	w := mustWeek(t, "2026-W42")

	sh, err := s.ShareWeek(alex.ID, w)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := s.ShareWeek(alex.ID, w); again.Token != sh.Token {
		t.Error("sharing the same week twice made a second link")
	}
	got, owner, err := s.SharedWeek(sh.Token)
	if err != nil || owner != alex.ID || got.Week != w {
		t.Errorf("the link shows %v of user %d (%v)", got.Week, owner, err)
	}
	if err := s.RevokeShare(sh.ID, sam.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Sam revoked Alex's link: %v", err)
	}
	if err := s.RevokeShare(sh.ID, alex.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SharedWeek(sh.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked link still works: %v", err)
	}
	fresh, _ := s.ShareWeek(alex.ID, w)
	if fresh.Token == sh.Token {
		t.Error("sharing again brought the revoked link back")
	}
	if shares, _ := s.Shares(alex.ID); len(shares) != 1 || shares[0].Token != fresh.Token {
		t.Errorf("Alex has these links out: %+v", shares)
	}
}

func TestRecentWeeks(t *testing.T) {
	s := open(t)
	alex := user(t, s, "Alex")
	for _, d := range []string{"2026-09-01", "2026-09-14", "2026-09-15", "2026-09-22"} {
		s.AddEntry(alex.ID, d, "09:00", "10:00", "")
	}
	weeks, err := s.RecentWeeks(alex.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(weeks) != 2 || weeks[0].Week.String() != "2026-W39" || weeks[1].Week.String() != "2026-W38" || weeks[1].Minutes != 120 {
		t.Errorf("got %+v", weeks)
	}
}
