package main

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"weektime/internal/store"
	"weektime/internal/web"
)

// The CLI is a documented way to use Weektime, so it is tested the way
// someone uses it: run a command against a real server and read what it
// printed.

type cli struct {
	t     *testing.T
	url   string
	token string
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "cli.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(web.New(s))
	t.Cleanup(func() { srv.Close(); s.Close() })
	return &cli{t: t, url: srv.URL}
}

// run executes a command and returns what it wrote. It fails the test if the
// command did.
func (c *cli) run(args ...string) string {
	c.t.Helper()
	out, err := c.try(args...)
	if err != nil {
		c.t.Fatalf("weektime %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// try is run for commands that are expected to fail. The server is named
// unless the token is a whole link, which names it already.
func (c *cli) try(args ...string) (string, error) {
	c.t.Helper()
	full := args
	if !strings.Contains(c.token, "/u/") {
		full = append([]string{"-s", c.url}, full...)
	}
	if c.token != "" {
		full = append([]string{"-t", c.token}, full...)
	}
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(full)
	err := root.Execute()
	return out.String(), err
}

// signIn starts a timesheet and keeps its private link for later commands.
func (c *cli) signIn(name string) string {
	c.t.Helper()
	out := c.run("register", name)
	m := regexp.MustCompile(`(http://\S+/u/[0-9a-f]{64})`).FindStringSubmatch(out)
	if m == nil {
		c.t.Fatalf("register printed no private link:\n%s", out)
	}
	c.token = m[1]
	return m[1]
}

func contains(t *testing.T, what, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("%s: expected %q in:\n%s", what, want, out)
	}
}

func TestLoggingAWeek(t *testing.T) {
	c := newCLI(t)
	c.signIn("Alex")
	contains(t, "whoami", c.run("whoami"), "Alex")

	out := c.run("log", "09:00", "11:30", "Client", "call", "--date", "2026-10-13")
	contains(t, "log", out, "Tue 13 Oct, 09:00–11:30 (2h 30m)")
	c.run("log", "13:00", "17:00", "Workshop", "-d", "2026-10-13")
	c.run("log", "10:00", "10:45", "-d", "2026-10-15")

	out = c.run("week", "2026-W42")
	for _, want := range []string{"Alex · 2026-W42", "Client call", "Workshop", "2h 30m", "6h 30m", "7h 15m", "2 days"} {
		contains(t, "week", out, want)
	}
	if strings.Contains(c.run("week", "2026-W41"), "Workshop") {
		t.Error("the week before shows this week's entries")
	}
	contains(t, "an empty week", c.run("week", "2026-W43"), "Nothing logged")
}

func TestEditingAndDeleting(t *testing.T) {
	c := newCLI(t)
	c.signIn("Alex")
	c.run("log", "09:00", "10:00", "call", "-d", "2026-10-13")
	contains(t, "edit", c.run("edit", "1", "--end", "12:15"), "09:00–12:15 (3h 15m)")
	contains(t, "rm", c.run("rm", "1"), "weektime restore 1")
	if strings.Contains(c.run("week", "2026-W42"), "call") {
		t.Error("a deleted entry is still listed")
	}
	c.run("restore", "1")
	contains(t, "after restore", c.run("week", "2026-W42"), "call")

	if _, err := c.try("edit", "1"); err == nil || !strings.Contains(err.Error(), "nothing to change") {
		t.Errorf("an edit with no flags: %v", err)
	}
	if _, err := c.try("log", "11:00", "10:00"); err == nil || !strings.Contains(err.Error(), "end after the start") {
		t.Errorf("logging backwards: %v", err)
	}
	if _, err := c.try("week", "next-week"); err == nil || !strings.Contains(err.Error(), "2026-W39") {
		t.Errorf("a week that is not one: %v", err)
	}
}

func TestSharingFromTheCLI(t *testing.T) {
	c := newCLI(t)
	c.signIn("Alex")
	out := c.run("share", "2026-W42")
	m := regexp.MustCompile(`(http://\S+/s/\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("share printed no link:\n%s", out)
	}
	contains(t, "sharing the same week again", c.run("share", "2026-W42"), m[1])
	contains(t, "shares", c.run("shares"), m[1])
	contains(t, "revoke", c.run("revoke", "1"), "Revoked share 1")
	contains(t, "shares after revoking", c.run("shares"), "No week is shared")
}

func TestTheTokenCanBeTheLinkOrJustTheToken(t *testing.T) {
	c := newCLI(t)
	link := c.signIn("Alex")
	// The whole link names the server, so -s is not needed (try leaves it out).
	contains(t, "with the link", c.run("whoami"), "Alex")
	// Just the token needs the server named.
	c.token = link[strings.LastIndex(link, "/")+1:]
	contains(t, "with the token", c.run("whoami"), "Alex")
	c.token = ""
	if _, err := c.try("whoami"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("without a token: %v", err)
	}
}

// The totals under a week sit in the table's own columns: the day under
// DATE, the time under DURATION.
func TestWeekTotalsLineUpWithTheTable(t *testing.T) {
	c := newCLI(t)
	c.signIn("Alex")
	for i := 0; i < 12; i++ { // two-digit IDs widen the first column
		c.run("log", "09:00", "10:00", "-d", "2026-10-13")
	}
	c.run("log", "10:00", "10:45", "a longer note than the others", "-d", "2026-10-15")
	out := c.run("week", "2026-W42")
	var header, week string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "ID"):
			header = line
		case strings.Contains(line, "WEEK"):
			week = line
		}
	}
	if header == "" || week == "" {
		t.Fatalf("no header or week total in:\n%s", out)
	}
	if at, want := strings.Index(week, "WEEK"), strings.Index(header, "DATE"); at != want {
		t.Errorf("WEEK is at column %d, DATE at %d:\n%s", at, want, out)
	}
	if at, want := strings.Index(week, "12h 45m"), strings.Index(header, "DURATION"); at != want {
		t.Errorf("the week's total is at column %d, DURATION at %d:\n%s", at, want, out)
	}
}
