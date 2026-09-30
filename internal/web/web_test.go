package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"weektime/internal/store"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
	app *Server // for setting its clock
}

// The tests live in the week of 21 to 27 September 2026, on the Wednesday.
var wednesday = time.Date(2026, 9, 23, 10, 0, 0, 0, time.Local)

func newEnv(t *testing.T) *env {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := New(s)
	app.now = func() time.Time { return wednesday }
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); s.Close() })
	return &env{t: t, srv: srv, st: s, app: app}
}

// noRedirect lets tests inspect redirects instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func want(t *testing.T, what string, got, wanted int) {
	t.Helper()
	if got != wanted {
		t.Errorf("%s: status %d, want %d", what, got, wanted)
	}
}

// call makes an API request; token (if any) is sent as a bearer token. The
// response body is decoded into out when out is non-nil.
func (e *env) call(method, path, token string, body string, out any) int {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// register makes a timesheet and returns its token.
func (e *env) register(name string) string {
	e.t.Helper()
	var u struct {
		Token string `json:"token"`
		Link  string `json:"link"`
	}
	if c := e.call("POST", "/api/users", "", `{"name":"`+name+`"}`, &u); c != 201 || u.Token == "" {
		e.t.Fatalf("register %s: status %d", name, c)
	}
	if !strings.HasSuffix(u.Link, "/u/"+u.Token) {
		e.t.Errorf("the private link %q does not end in the token", u.Link)
	}
	return u.Token
}

// log adds an entry through the API and returns it.
func (e *env) log(token, date, start, end, note string) store.Entry {
	e.t.Helper()
	var en store.Entry
	body, _ := json.Marshal(map[string]string{"date": date, "start": start, "end": end, "note": note})
	if c := e.call("POST", "/api/entries", token, string(body), &en); c != 201 {
		e.t.Fatalf("log %s %s-%s: status %d", date, start, end, c)
	}
	return en
}

// form posts a form the way a browser on this site would, signed in with
// the cookie when token is set, and returns the response unread.
func (e *env) form(token, path string, v url.Values) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// page fetches an HTML page as a browser signed in with token would.
func (e *env) page(token, path string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func cookieFrom(resp *http.Response) string {
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return c.Value
		}
	}
	return ""
}

func TestSigningUpHandsOverThePrivateLink(t *testing.T) {
	e := newEnv(t)
	resp := e.form("", "/welcome", url.Values{"name": {"Alex"}})
	want(t, "sign up", resp.StatusCode, http.StatusSeeOther)
	token := cookieFrom(resp)
	if token == "" || resp.Header.Get("Location") != "/u/"+token {
		t.Fatalf("signing up went to %q with cookie %q", resp.Header.Get("Location"), token)
	}

	// The private link is where they land, so bookmarking that page is
	// bookmarking the link.
	code, body := e.page(token, "/u/"+token)
	want(t, "the private link", code, 200)
	for _, s := range []string{"This link is your key", e.srv.URL + "/u/" + token, "Bookmark this page"} {
		if !strings.Contains(body, s) {
			t.Errorf("the page handing over the link does not say %q", s)
		}
	}
	// Until they say it is saved, every page reminds them.
	if _, week := e.page(token, "/week/2026-W39"); !strings.Contains(week, "Bookmark your private link") {
		t.Error("the week page does not remind them to save the link")
	}
	want(t, "saving it", e.form(token, "/me/link/saved", url.Values{"next": {"/"}}).StatusCode, http.StatusSeeOther)
	if _, week := e.page(token, "/week/2026-W39"); strings.Contains(week, "Bookmark your private link") {
		t.Error("still reminded after saying it is saved")
	}
	// From then on the link goes straight to the week.
	resp2, _ := noRedirect.Get(e.srv.URL + "/u/" + token)
	if resp2.StatusCode != http.StatusSeeOther || resp2.Header.Get("Location") != "/" {
		t.Errorf("the saved link answered %d, to %q", resp2.StatusCode, resp2.Header.Get("Location"))
	}
}

func TestThePrivateLinkSignsInAnywhere(t *testing.T) {
	e := newEnv(t)
	token := e.register("Alex")
	resp, err := noRedirect.Get(e.srv.URL + "/u/" + token) // a new device: no cookie
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cookieFrom(resp) != token {
		t.Error("opening the private link did not sign this browser in")
	}
	if c := resp.Header.Get("Cache-Control"); c != "no-store" {
		t.Errorf("the private link's page may be cached: %q", c)
	}
	if code, _ := e.page("", "/u/"+strings.Repeat("0", 64)); code != 404 {
		t.Errorf("somebody else's made-up link: status %d", code)
	}

	// Pasting it in works too, whole or as just the token.
	for _, pasted := range []string{e.srv.URL + "/u/" + token, "  " + token + " ", e.srv.URL + "/u/" + token + "?next=/"} {
		resp := e.form("", "/welcome/link", url.Values{"link": {pasted}})
		if resp.StatusCode != http.StatusSeeOther || cookieFrom(resp) != token {
			t.Errorf("pasting %q: status %d", pasted, resp.StatusCode)
		}
	}
	want(t, "pasting nonsense", e.form("", "/welcome/link", url.Values{"link": {"nope"}}).StatusCode, 400)
}

func TestNoPageGivesTheLinkAway(t *testing.T) {
	e := newEnv(t)
	token := e.register("Alex")
	req, _ := http.NewRequest("GET", e.srv.URL+"/week/2026-W39", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	resp, _ := noRedirect.Do(req)
	resp.Body.Close()
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy is %q", got)
	}
	// It is only on the page that hands it over, and fetched on request.
	_, body := e.page(token, "/week/2026-W39")
	if strings.Contains(body, token) {
		t.Error("the week page has the private link written into it")
	}
}

func TestStrangersAreSentToSignUp(t *testing.T) {
	e := newEnv(t)
	resp, _ := noRedirect.Get(e.srv.URL + "/week/2026-W39")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/welcome?next=") {
		t.Errorf("a stranger got %d, to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	want(t, "the API without a token", e.call("GET", "/api/weeks/current", "", "", nil), 401)
}

func TestTheWeekPage(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	e.log(alex, "2026-09-22", "09:00", "11:30", "Client call and proposal draft")
	e.log(alex, "2026-09-22", "13:00", "17:00", "Workshop")
	e.log(alex, "2026-09-28", "09:00", "10:00", "Next week's")

	resp, _ := noRedirect.Do(func() *http.Request {
		r, _ := http.NewRequest("GET", e.srv.URL+"/", nil)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: alex})
		return r
	}())
	if resp.Header.Get("Location") != "/week/2026-W39" {
		t.Errorf("/ goes to %q, not this week", resp.Header.Get("Location"))
	}

	code, body := e.page(alex, "/week/2026-W39")
	want(t, "the week", code, 200)
	for _, s := range []string{"21 – 27 Sep 2026", "Client call and proposal draft", "2h 30m", "6h 30m", "Tuesday",
		`href="/week/2026-W38"`, `href="/week/2026-W40"`, `value="2026-09-23"`} {
		if !strings.Contains(body, s) {
			t.Errorf("the week page does not show %q", s)
		}
	}
	if strings.Contains(body, "Next week&#39;s") || strings.Contains(body, "Next week's") {
		t.Error("an entry from the next week is on this one")
	}
	// Another week's form starts on its Monday.
	if _, next := e.page(alex, "/week/2026-W40"); !strings.Contains(next, `value="2026-09-28"`) {
		t.Error("the next week's form does not start on its Monday")
	}
	if code, _ := e.page(alex, "/week/2026-W99"); code != 404 {
		t.Errorf("a week that does not exist: status %d", code)
	}
}

func TestLoggingThroughTheForm(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	resp := e.form(alex, "/entries", url.Values{
		"date": {"2026-09-24"}, "start": {"09:00"}, "end": {"10:15"}, "note": {"Standup"}, "next": {"/week/2026-W39"}})
	if resp.Header.Get("Location") != "/week/2026-W39" {
		t.Errorf("logging on this week went to %q", resp.Header.Get("Location"))
	}
	// An entry on a day in another week goes to that week's page.
	resp = e.form(alex, "/entries", url.Values{
		"date": {"2026-10-01"}, "start": {"09:00"}, "end": {"10:00"}, "next": {"/week/2026-W39"}})
	if resp.Header.Get("Location") != "/week/2026-W40" {
		t.Errorf("logging on the next week went to %q", resp.Header.Get("Location"))
	}
	// No date means today.
	e.form(alex, "/entries", url.Values{"start": {"14:00"}, "end": {"15:00"}, "next": {"/week/2026-W39"}})
	var sheet store.Timesheet
	e.call("GET", "/api/weeks/2026-W39", alex, "", &sheet)
	if sheet.Minutes != 135 || len(sheet.Days[2].Entries) != 1 {
		t.Errorf("after logging: %d minutes, Wednesday %v", sheet.Minutes, sheet.Days[2].Entries)
	}
	// Nonsense is turned away without an error page.
	resp = e.form(alex, "/entries", url.Values{"date": {"2026-09-24"}, "start": {"11:00"}, "end": {"10:00"}, "next": {"/week/2026-W39"}})
	want(t, "ending before starting", resp.StatusCode, http.StatusSeeOther)
}

func TestEditingAndDeletingAnEntry(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	en := e.log(alex, "2026-09-22", "09:00", "10:00", "call")
	resp := e.form(alex, "/entries/"+itoa(en.ID)+"/edit", url.Values{
		"date": {"2026-09-22"}, "start": {"09:00"}, "end": {"11:00"}, "note": {"longer call"}, "next": {"/week/2026-W39"}})
	want(t, "editing", resp.StatusCode, http.StatusSeeOther)
	var got store.Entry
	e.call("PATCH", "/api/entries/"+itoa(en.ID), alex, `{"note":"longer call, notes sent"}`, &got)
	if got.Minutes != 120 || got.Note != "longer call, notes sent" {
		t.Errorf("after editing: %+v", got)
	}
	want(t, "an empty edit", e.call("PATCH", "/api/entries/"+itoa(en.ID), alex, `{}`, nil), 400)
	want(t, "an edit that ends first", e.call("PATCH", "/api/entries/"+itoa(en.ID), alex, `{"end":"08:00"}`, nil), 400)

	e.form(alex, "/entries/"+itoa(en.ID)+"/delete", url.Values{"next": {"/week/2026-W39"}})
	var sheet store.Timesheet
	e.call("GET", "/api/weeks/2026-W39", alex, "", &sheet)
	if sheet.Minutes != 0 {
		t.Errorf("a deleted entry still counts: %d", sheet.Minutes)
	}
	resp = e.form(alex, "/entries/"+itoa(en.ID)+"/restore", url.Values{"next": {"/week/2026-W39"}})
	want(t, "undo", resp.StatusCode, http.StatusSeeOther)
	e.call("GET", "/api/weeks/2026-W39", alex, "", &sheet)
	if sheet.Minutes != 120 {
		t.Errorf("after undo: %d minutes", sheet.Minutes)
	}
}

func TestSomebodyElsesEntriesAreOutOfReach(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	sam := e.register("Sam")
	en := e.log(alex, "2026-09-22", "09:00", "10:00", "Alex's")
	id := itoa(en.ID)
	want(t, "PATCH", e.call("PATCH", "/api/entries/"+id, sam, `{"note":"Sam's now"}`, nil), 404)
	want(t, "DELETE", e.call("DELETE", "/api/entries/"+id, sam, "", nil), 404)
	want(t, "edit form", e.form(sam, "/entries/"+id+"/edit", url.Values{
		"date": {"2026-09-22"}, "start": {"09:00"}, "end": {"10:00"}, "note": {"x"}}).StatusCode, 404)
	want(t, "delete form", e.form(sam, "/entries/"+id+"/delete", nil).StatusCode, 404)
	if _, body := e.page(sam, "/week/2026-W39"); strings.Contains(body, "Alex&#39;s") {
		t.Error("Sam's week shows Alex's entry")
	}
}

func TestSharingAWeek(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	e.log(alex, "2026-09-22", "09:00", "11:30", "Client call")
	e.log(alex, "2026-09-29", "09:00", "10:00", "The week after")

	resp := e.form(alex, "/week/2026-W39/share", nil)
	if resp.Header.Get("Location") != "/week/2026-W39#share" {
		t.Errorf("sharing went to %q", resp.Header.Get("Location"))
	}
	var shares []struct {
		ID    int64  `json:"id"`
		Week  string `json:"week"`
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	e.call("GET", "/api/shares", alex, "", &shares)
	if len(shares) != 1 || shares[0].Week != "2026-W39" || shares[0].URL != e.srv.URL+"/s/"+shares[0].Token {
		t.Fatalf("shares: %+v", shares)
	}
	link := "/s/" + shares[0].Token
	if _, week := e.page(alex, "/week/2026-W39"); !strings.Contains(week, e.srv.URL+link) {
		t.Error("the week's share dialog does not show its link")
	}

	// Anyone with the link sees the week, read-only, and nothing else.
	resp2, err := http.Get(e.srv.URL + link)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	body := string(b)
	want(t, "the shared week", resp2.StatusCode, 200)
	for _, s := range []string{"Alex", "21 – 27 Sep 2026", "Client call", "2h 30m", "Read-only"} {
		if !strings.Contains(body, s) {
			t.Errorf("the shared week does not show %q", s)
		}
	}
	for _, s := range []string{"The week after", `action="/entries`, "Bookmark your private link", "/me/link"} {
		if strings.Contains(body, s) {
			t.Errorf("the shared week shows %q", s)
		}
	}
	if cookieFrom(resp2) != "" {
		t.Error("looking at a shared week signed the viewer in as somebody")
	}

	// Revoked, it shows nothing to anyone.
	want(t, "revoking", e.form(alex, "/shares/"+itoa(shares[0].ID)+"/revoke", url.Values{"next": {"/shared"}}).StatusCode, http.StatusSeeOther)
	if code, body := e.page("", link); code != 404 || strings.Contains(body, "Client call") {
		t.Errorf("a revoked link: status %d", code)
	}
	if code, _ := e.page("", link+"/events"); code != 404 {
		t.Errorf("a revoked link's live updates: status %d", code)
	}
	// Sharing again makes a new link; the old one stays dead.
	var again struct{ Token string }
	e.call("POST", "/api/weeks/2026-W39/share", alex, "", &again)
	if again.Token == "" || again.Token == shares[0].Token {
		t.Errorf("sharing again gave %q", again.Token)
	}
}

func TestOnlyTheOwnerCanRevoke(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	sam := e.register("Sam")
	var sh struct {
		ID    int64
		Token string
	}
	e.call("POST", "/api/weeks/current/share", alex, "", &sh)
	want(t, "Sam revoking", e.call("DELETE", "/api/shares/"+itoa(sh.ID), sam, "", nil), 404)
	e.form(sam, "/shares/"+itoa(sh.ID)+"/revoke", nil)
	if code, _ := e.page("", "/s/"+sh.Token); code != 200 {
		t.Errorf("Alex's link stopped working after Sam tried: %d", code)
	}
}

func TestTheSharedLinksPage(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	e.log(alex, "2026-09-15", "09:00", "12:00", "")
	e.call("POST", "/api/weeks/2026-W38/share", alex, "", nil)
	_, body := e.page(alex, "/shared")
	for _, s := range []string{"14 – 20 Sep 2026", "3h logged", "Revoke"} {
		if !strings.Contains(body, s) {
			t.Errorf("the shared links page does not show %q", s)
		}
	}
}

func TestTheAPI(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	var me store.User
	e.call("GET", "/api/me", alex, "", &me)
	if me.Name != "Alex" {
		t.Errorf("/api/me: %+v", me)
	}
	// No date means today, on the server.
	en := e.log(alex, "", "09:00", "09:45", "")
	if en.Date != "2026-09-23" || en.Minutes != 45 {
		t.Errorf("an entry with no date: %+v", en)
	}
	var sheet store.Timesheet
	want(t, "this week", e.call("GET", "/api/weeks/current", alex, "", &sheet), 200)
	if sheet.Week.String() != "2026-W39" || sheet.Minutes != 45 || len(sheet.Days) != 7 {
		t.Errorf("this week: %+v", sheet)
	}
	want(t, "a bad week", e.call("GET", "/api/weeks/next-tuesday", alex, "", nil), 400)
	var msg struct{ Error string }
	want(t, "a bad entry", e.call("POST", "/api/entries", alex, `{"start":"10:00","end":"09:00"}`, &msg), 400)
	if !strings.Contains(msg.Error, "end after the start") {
		t.Errorf("the error does not say what was wrong: %q", msg.Error)
	}
	want(t, "not JSON", e.call("POST", "/api/entries", alex, `nope`, nil), 400)
}

func TestRequestsFromOtherSitesAreRefused(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	req, _ := http.NewRequest("POST", e.srv.URL+"/entries", strings.NewReader("start=09:00&end=10:00"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: alex})
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want(t, "a cross-site form post", resp.StatusCode, 403)
}

// listen opens a live-update stream and reports each signal on the channel.
func (e *env) listen(path, token string) <-chan string {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		e.t.Fatalf("%s: status %d", path, resp.StatusCode)
	}
	events := make(chan string, 10)
	ready := make(chan struct{})
	go func() {
		defer close(events) // the stream has ended
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "retry:") {
				close(ready)
			}
			if strings.HasPrefix(line, "event: ") {
				events <- strings.TrimPrefix(line, "event: ")
			}
		}
	}()
	<-ready
	return events
}

func expectEvent(t *testing.T, what string, events <-chan string) {
	t.Helper()
	select {
	case ev := <-events:
		if ev != "changed" {
			t.Errorf("%s: got %q", what, ev)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("%s: no update arrived", what)
	}
}

func TestChangesReachOpenPagesAndSharedViewers(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	sam := e.register("Sam")
	var sh struct{ Token string }
	e.call("POST", "/api/weeks/current/share", alex, "", &sh)

	own := e.listen("/events", alex)
	viewer := e.listen("/s/"+sh.Token+"/events", "")
	other := e.listen("/events", sam)

	e.log(alex, "2026-09-23", "09:00", "10:00", "")
	expectEvent(t, "Alex's own page", own)
	expectEvent(t, "the shared week", viewer)
	select {
	case <-other:
		t.Error("Sam was told about Alex's timesheet")
	case <-time.After(300 * time.Millisecond):
	}
}

// Opening a link is a GET, which any site can make a browser do, so a link
// to someone else's timesheet must not quietly sign out whoever is here.
func TestAnotherTimesheetsLinkAsksBeforeSwitching(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	sam := e.register("Sam")

	req, _ := http.NewRequest("GET", e.srv.URL+"/u/"+sam, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: alex})
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	want(t, "Sam's link in Alex's browser", resp.StatusCode, 200)
	if got := cookieFrom(resp); got != "" {
		t.Error("opening Sam's link signed Alex's browser in as somebody without asking")
	}
	for _, s := range []string{"Switch timesheets?", "Switch to Sam", "Stay with Alex", "never been saved"} {
		if !strings.Contains(string(b), s) {
			t.Errorf("the page asking to switch does not say %q", s)
		}
	}
	if code, _ := e.page(alex, "/api/me"); code != 200 {
		t.Errorf("Alex is no longer signed in: %d", code)
	}

	// Going ahead is a POST, which only this site's own form can send.
	req, _ = http.NewRequest("POST", e.srv.URL+"/u/"+sam, nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: alex})
	resp, _ = noRedirect.Do(req)
	resp.Body.Close()
	want(t, "switching from another site", resp.StatusCode, 403)

	resp = e.form(alex, "/u/"+sam, nil)
	if resp.StatusCode != http.StatusSeeOther || cookieFrom(resp) != sam {
		t.Errorf("switching here: status %d, cookie %q", resp.StatusCode, cookieFrom(resp))
	}
	want(t, "switching to a link that is nobody's", e.form(alex, "/u/nope", nil).StatusCode, 404)

	// Your own link, signed in or not, goes straight through.
	if code, _ := e.page(alex, "/u/"+alex); code != 200 {
		t.Errorf("Alex opening Alex's own link: %d", code)
	}
}

// Revoking a link stops it for anyone still watching: their stream gets one
// last signal, so the page finds out, and then ends. Before, it went on
// telling them whenever the owner did anything, for as long as it was open.
func TestARevokedLinksStreamEnds(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	var sh struct {
		ID    int64
		Token string
	}
	e.call("POST", "/api/weeks/current/share", alex, "", &sh)
	viewer := e.listen("/s/"+sh.Token+"/events", "")

	want(t, "revoking", e.call("DELETE", "/api/shares/"+itoa(sh.ID), alex, "", nil), 204)
	expectEvent(t, "the revoked link's viewer", viewer)
	select {
	case _, open := <-viewer:
		if open {
			t.Error("the stream went on after its last signal")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stream of a revoked link stayed open")
	}
}

// A shared week's viewers are counted per link. Behind a proxy they all have
// the proxy's address, and counting per address gave them one cap between
// them, so the seventeenth stopped getting updates.
func TestManyViewersOfOneLinkAllGetUpdates(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	var sh struct{ Token string }
	e.call("POST", "/api/weeks/current/share", alex, "", &sh)
	var streams []<-chan string
	for i := 0; i < maxStreams+4; i++ {
		streams = append(streams, e.listen("/s/"+sh.Token+"/events", "")) // fails the test on a 429
	}
	e.log(alex, "2026-09-23", "09:00", "10:00", "")
	expectEvent(t, "the last viewer to arrive", streams[len(streams)-1])
}

func TestTheViewerCapIsStillACap(t *testing.T) {
	h := newHub()
	for i := 0; i < 3; i++ {
		if h.subscribe(1, "share:x", 3) == nil {
			t.Fatalf("stream %d was refused under the cap", i+1)
		}
	}
	if h.subscribe(1, "share:x", 3) != nil {
		t.Error("a stream over the cap was let in")
	}
	if h.subscribe(1, "share:y", 3) == nil {
		t.Error("another link's viewers were counted against this one")
	}
}

// The button between the arrows names the week on screen, so stepping back
// and forth says where you are; clicking it still goes back to this week.
func TestTheWeekButtonSaysWhichWeekItIs(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	for path, label := range map[string]string{
		"/week/2026-W39": "This week", "/week/2026-W38": "Last week", "/week/2026-W40": "Next week",
		"/week/2026-W30": "Week 30", "/week/2025-W52": "Week 52, 2025",
	} {
		_, body := e.page(alex, path)
		if !strings.Contains(body, `href="/"`) || !strings.Contains(body, ">"+label+"</a>") {
			t.Errorf("%s: the button does not say %q", path, label)
		}
	}
}

// A private link is the account, so it has to keep working: after the
// server is stopped and started again on the same database, days later,
// with the clean-up and upgrades that run on start-up in between.
func TestAPrivateLinkStillWorksDaysLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keep.db")
	start := func(now time.Time) (*httptest.Server, *store.Store) {
		s, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		app := New(s)
		app.now = func() time.Time { return now }
		return httptest.NewServer(app), s
	}

	srv, st := start(wednesday)
	e := &env{t: t, srv: srv, st: st}
	alex := e.register("Alex")
	e.log(alex, "2026-09-23", "09:00", "11:30", "Before the restart")
	srv.Close()
	st.Close()

	later := wednesday.AddDate(0, 0, 5)
	srv, st = start(later)
	defer func() { srv.Close(); st.Close() }()
	e = &env{t: t, srv: srv, st: st}

	// A browser that has never seen the server opens the link.
	resp, err := noRedirect.Get(srv.URL + "/u/" + alex)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || cookieFrom(resp) != alex {
		t.Fatalf("the link five days later: status %d, cookie %q", resp.StatusCode, cookieFrom(resp))
	}
	// ...and a browser that kept its cookie is still signed in.
	if _, body := e.page(alex, "/week/2026-W39"); !strings.Contains(body, "Before the restart") {
		t.Error("the week logged before the restart is not there after it")
	}
	// The cookie outlasts a few days by a long way.
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.MaxAge < 300*24*3600 {
			t.Errorf("the sign-in cookie only lasts %d seconds", c.MaxAge)
		}
	}
}
