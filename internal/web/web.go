package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"weektime/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// manifestJSON makes Weektime installable: "Add to Home Screen" (or "Install"
// in Chrome/Edge) gives it an icon and a window without browser chrome. There
// is no service worker, so it is not usable offline.
const manifestJSON = `{
  "name": "Weektime",
  "short_name": "Weektime",
  "description": "A weekly timesheet you can share with a link",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#151f2c",
  "theme_color": "#1d273b",
  "icons": [
    {"src": "/static/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any maskable"},
    {"src": "/static/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable"}
  ]
}`

func serveManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.WriteString(w, manifestJSON)
}

const cookieName = "weektime_token"

var avatarColors = []string{"blue", "azure", "indigo", "purple", "pink", "red", "orange", "yellow", "lime", "green", "teal", "cyan"}

const dateLayout = "2006-01-02"

func parseDate(s string) (time.Time, bool) {
	t, err := time.Parse(dateLayout, s)
	return t, err == nil
}

var funcs = template.FuncMap{
	// color picks a stable Tabler colour name for a user.
	"color": func(id int64) string { return avatarColors[int(id%int64(len(avatarColors)))] },
	// initial is the first letter of a name, upper-cased, for avatars.
	"initial": func(name string) string {
		r, _ := utf8.DecodeRuneInString(name)
		if r == utf8.RuneError {
			return "?"
		}
		return strings.ToUpper(string(r))
	},
	// duration writes minutes as a timesheet does: "2h 30m".
	"duration": store.Duration,
	// dayName is "Tuesday", dayDate is "14 Oct", dayLetter is "T".
	"dayName":   func(date string) string { return formatDate(date, "Monday") },
	"dayDate":   func(date string) string { return formatDate(date, "2 Jan") },
	"dayShort":  func(date string) string { return formatDate(date, "Mon, 2 Jan") },
	"dayLetter": func(date string) string { return formatDate(date, "Mon")[:1] },
	"weekRange": weekRange,
	"weekURL":   weekURL,
	// row bundles an entry with the page it is shown on, for "entryrow".
	"row": func(path string, e store.Entry) entryRow { return entryRow{Entry: e, Path: path} },
	// barHeight is how tall a day's bar is in the week's chart, as a
	// percentage of the chart. The scale is a full working day or the longest
	// day of the week, whichever is more, so one short day does not fill it.
	"barHeight": func(minutes int, sheet store.Timesheet) int {
		most := 8 * 60
		for _, d := range sheet.Days {
			most = max(most, d.Minutes)
		}
		return minutes * 100 / most
	},
	"plural": func(n int, word string) string {
		if n == 1 {
			return "1 " + word
		}
		return strconv.Itoa(n) + " " + word + "s"
	},
	// average is the mean over the days worked, as a duration.
	"average": func(sheet store.Timesheet) string {
		if sheet.DaysWorked() == 0 {
			return "–"
		}
		return store.Duration(sheet.Minutes / sheet.DaysWorked())
	},
}

func formatDate(date, layout string) string {
	d, ok := parseDate(date)
	if !ok {
		return date
	}
	return d.Format(layout)
}

// weekRange writes the days a week covers as briefly as is unambiguous:
// "21 – 27 Sep 2026", "29 Sep – 5 Oct 2026", "29 Dec 2025 – 4 Jan 2026".
func weekRange(w store.Week) string {
	from, to := w.Monday(), w.Monday().AddDate(0, 0, 6)
	switch {
	case from.Year() != to.Year():
		return from.Format("2 Jan 2006") + " – " + to.Format("2 Jan 2006")
	case from.Month() != to.Month():
		return from.Format("2 Jan") + " – " + to.Format("2 Jan 2006")
	default:
		return from.Format("2") + " – " + to.Format("2 Jan 2006")
	}
}

func weekURL(w store.Week) string { return "/week/" + w.String() }

// entryRow is what the "entryrow" template renders.
type entryRow struct {
	Entry store.Entry
	Path  string // page to return to after an action
}

type Server struct {
	store *store.Store
	tmpl  *template.Template
	mux   *http.ServeMux
	hub   *hub
	// signups limits how fast one address can create timesheets, the only
	// thing a stranger can do here without a link already.
	signups *limiter
	// sameOrigin refuses requests that change something when a browser sent
	// them from another site. SameSite=Lax stops such a request carrying the
	// victim's cookie, but not its response setting a new one: without this a
	// page elsewhere could sign you in as someone else, replacing your token.
	// Requests with no browser headers at all, like the CLI's, are allowed.
	sameOrigin *http.CrossOriginProtection
	// now is the time, which tests move to see a particular week.
	now func() time.Time
}

func New(s *store.Store) *Server {
	srv := &Server{
		store:      s,
		tmpl:       template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")),
		mux:        http.NewServeMux(),
		hub:        newHub(),
		signups:    newLimiter(signupBurst, signupRefill),
		sameOrigin: http.NewCrossOriginProtection(),
		now:        time.Now,
	}
	s.SetNotifier(srv.hub.publish)
	srv.routes()
	return srv
}

// today is the date on the server, which is what "today" means everywhere.
func (s *Server) today() string { return s.now().Format(dateLayout) }

func (s *Server) thisWeek() store.Week { return store.WeekOf(s.now()) }

type userKey struct{}

// csp keeps the page to its own origin. The inline scripts and styles need
// 'unsafe-inline', so this does not stop injected script from running; what it
// does stop is a page fetching or sending anything anywhere else, which is
// what an injection would want to do.
const csp = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// CloseStreams ends every live-update stream, for a server that is shutting
// down: register it with http.Server.RegisterOnShutdown. Ordinary requests are
// left to finish.
func (s *Server) CloseStreams() { s.hub.close() }

// ServeHTTP identifies the caller (cookie or bearer token) before routing.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A private link is the whole account and sits in the address bar, so no
	// page may pass its address on to wherever a link on it leads.
	w.Header().Set("Referrer-Policy", "no-referrer")
	// What comes back depends on whether the browser takes gzip, so a cache
	// between us must not hand one kind to the other.
	w.Header().Add("Vary", "Accept-Encoding")
	if wantsGzip(r) {
		gw := &gzipResponse{ResponseWriter: w}
		defer gw.finish()
		w = gw
	}
	if err := s.sameOrigin.Check(r); err != nil {
		http.Error(w, "refused: that request came from another site", http.StatusForbidden)
		return
	}
	if token := tokenFrom(r); token != "" {
		if u, err := s.store.UserByToken(token); err == nil {
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, &u))
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// App icons and the install manifest (public: browsers fetch them without cookies)
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	s.mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
	s.mux.HandleFunc("GET /manifest.webmanifest", serveManifest)

	// Signing up and in
	s.mux.HandleFunc("GET /welcome", s.welcomeGet)
	s.mux.HandleFunc("POST /welcome", s.limited(s.welcomePost, s.tooManySignups))
	s.mux.HandleFunc("POST /welcome/link", s.welcomeLink)
	s.mux.HandleFunc("GET /u/{token}", s.privateLink)
	s.mux.HandleFunc("POST /u/{token}", s.switchTo)
	s.mux.HandleFunc("GET /me", s.authed(s.meGet))
	s.mux.HandleFunc("GET /me/link", s.authed(s.meLink))
	s.mux.HandleFunc("POST /me/link/saved", s.authed(s.meLinkSaved))
	s.mux.HandleFunc("POST /me", s.authed(s.mePost))

	// HTML UI
	s.mux.HandleFunc("GET /{$}", s.authed(s.pageIndex))
	s.mux.HandleFunc("GET /week/{week}", s.authed(s.pageWeek))
	s.mux.HandleFunc("GET /shared", s.authed(s.pageShared))
	s.mux.HandleFunc("GET /events", s.authed(s.events))
	s.mux.HandleFunc("POST /entries", s.authed(s.formAddEntry))
	s.mux.HandleFunc("POST /entries/{id}/edit", s.authed(s.formEditEntry))
	s.mux.HandleFunc("POST /entries/{id}/delete", s.authed(s.formDeleteEntry))
	s.mux.HandleFunc("POST /entries/{id}/restore", s.authed(s.formRestoreEntry))
	s.mux.HandleFunc("POST /week/{week}/share", s.authed(s.formShareWeek))
	s.mux.HandleFunc("POST /shares/{id}/revoke", s.authed(s.formRevokeShare))

	// What a shared link shows: one week, read-only, to anybody who has it.
	s.mux.HandleFunc("GET /s/{token}", s.pageSharedWeek)
	s.mux.HandleFunc("GET /s/{token}/events", s.sharedEvents)

	// JSON API (used by the CLI). Callers authenticate with
	// "Authorization: Bearer <token>".
	s.mux.HandleFunc("POST /api/users", s.limited(s.apiCreateUser, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusTooManyRequests, "too many new people from this address; wait a minute")
	}))
	s.mux.HandleFunc("GET /api/me", s.api(s.apiMe))
	s.mux.HandleFunc("GET /api/weeks/{week}", s.api(s.apiWeek))
	s.mux.HandleFunc("POST /api/entries", s.api(s.apiAddEntry))
	s.mux.HandleFunc("PATCH /api/entries/{id}", s.api(s.apiPatchEntry))
	s.mux.HandleFunc("DELETE /api/entries/{id}", s.api(s.apiDeleteEntry))
	s.mux.HandleFunc("POST /api/entries/{id}/restore", s.api(s.apiRestoreEntry))
	s.mux.HandleFunc("GET /api/shares", s.api(s.apiShares))
	s.mux.HandleFunc("POST /api/weeks/{week}/share", s.api(s.apiShareWeek))
	s.mux.HandleFunc("DELETE /api/shares/{id}", s.api(s.apiRevokeShare))
}

// ---- identity ---------------------------------------------------------

func tokenFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey{}).(*store.User)
	return u
}

// isHTTPS reports whether the browser reached us over HTTPS, either directly
// or through a proxy that terminated TLS and said so. Trusting the header is
// safe for both of its uses: forging it can only make a cookie stricter or a
// link more secure than it needed to be, never less.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSession(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// The token is the whole account, so it must never travel over plain
		// HTTP. Behind a proxy that terminates TLS, r.TLS is always nil, which
		// would leave this off in exactly the deployment it matters most.
		Secure: isHTTPS(r),
	})
}

// safeNext keeps post-login redirects on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}
	return next
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func privateLink(r *http.Request, token string) string { return baseURL(r) + "/u/" + token }
func shareLink(r *http.Request, sh store.Share) string { return baseURL(r) + "/s/" + sh.Token }

type userHandler func(http.ResponseWriter, *http.Request, *store.User)

// authed sends visitors without a timesheet to the welcome page first.
func (s *Server) authed(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r)
		if u == nil {
			dest := "/welcome"
			if r.Method == http.MethodGet {
				dest += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		h(w, r, u)
	}
}

// api is authed for the JSON API: no redirect, just a 401 saying what to do.
func (s *Server) api(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r)
		if u == nil {
			writeError(w, http.StatusUnauthorized,
				`missing or invalid token: run "weektime register <name>", or pass your private link with -t`)
			return
		}
		h(w, r, u)
	}
}

// ---- welcome / private link / profile --------------------------------

type simplePage struct {
	Title, Heading, Subtitle, Next, Error string
	// Link is set on the page that hands someone their private link.
	Link string
	// Switch is set on the page that asks whether to change timesheets.
	Switch *switchAsk
}

// switchAsk is what the page asking to change timesheets needs to say.
type switchAsk struct {
	From, To string // the names signed in now, and on the link
	Action   string // where to post to go ahead
	// Unsaved is set when the timesheet signed in now has never had its
	// link saved, so switching away may lose it for good.
	Unsaved bool
}

func (s *Server) renderTmpl(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func welcomePage(next, errMsg string) simplePage {
	return simplePage{
		Title:    "Welcome",
		Heading:  "Your week, on one page",
		Subtitle: "Type your name and you get a timesheet of your own. There is no password and no email: a private link is your key.",
		Next:     safeNext(next),
		Error:    errMsg,
	}
}

func (s *Server) welcomeGet(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if userFrom(r) != nil {
		http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
		return
	}
	s.renderTmpl(w, http.StatusOK, "welcome.html", welcomePage(next, ""))
}

// tooManySignups turns the sign-up form down without losing the page.
func (s *Server) tooManySignups(w http.ResponseWriter, r *http.Request) {
	s.renderTmpl(w, http.StatusTooManyRequests, "welcome.html",
		welcomePage(r.FormValue("next"), "Too many new timesheets from this connection. Try again in a minute."))
}

// welcomePost makes a timesheet and sends the browser to its private link,
// so that the page asking them to bookmark it is the page to bookmark.
func (s *Server) welcomePost(w http.ResponseWriter, r *http.Request) {
	_, token, err := s.store.CreateUser(r.FormValue("name"))
	if errors.Is(err, store.ErrInvalid) {
		s.renderTmpl(w, http.StatusBadRequest, "welcome.html",
			welcomePage(r.FormValue("next"), "Please enter a name (up to 40 characters)."))
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	setSession(w, r, token)
	dest := "/u/" + token
	if next := safeNext(r.FormValue("next")); next != "/" {
		dest += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// welcomeLink signs in with a private link pasted on another device.
func (s *Server) welcomeLink(w http.ResponseWriter, r *http.Request) {
	next := r.FormValue("next")
	token := store.TokenIn(r.FormValue("link"))
	if _, err := s.store.UserByToken(token); err != nil {
		s.renderTmpl(w, http.StatusBadRequest, "welcome.html", welcomePage(next, "That link isn't anyone's timesheet."))
		return
	}
	setSession(w, r, token)
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

// privateLink is somebody's key: opening it signs this browser in as them.
// Until they say the link is somewhere safe, the page stays here and tells
// them to bookmark it; after that the link goes straight to their week.
func (s *Server) privateLink(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	token := r.PathValue("token")
	u, err := s.store.UserByToken(token)
	if err != nil {
		s.renderTmpl(w, http.StatusNotFound, "welcome.html",
			welcomePage("", "That link isn't anyone's timesheet. Check it was copied whole, or start a new one."))
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	// Opening a link is a GET, which any website can make a browser do. If
	// that quietly swapped whoever is signed in here for whoever the link
	// belongs to, a page elsewhere could log you out of your own timesheet,
	// which without its link saved is gone for good, and have you log your
	// hours into one somebody else can read. So a link to a different
	// timesheet asks first, and only a click on this site goes ahead.
	if cur := userFrom(r); cur != nil && cur.ID != u.ID {
		action := "/u/" + token
		if next != "/" {
			action += "?next=" + url.QueryEscape(next)
		}
		s.renderTmpl(w, http.StatusOK, "welcome.html", simplePage{
			Title:    "Switch timesheets?",
			Heading:  "Switch timesheets?",
			Subtitle: "This browser is signed in to " + cur.Name + "'s timesheet. The link you opened is " + u.Name + "'s.",
			Next:     "/",
			Switch:   &switchAsk{From: cur.Name, To: u.Name, Action: action, Unsaved: !cur.LinkSaved},
		})
		return
	}
	setSession(w, r, token)
	if u.LinkSaved {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.renderTmpl(w, http.StatusOK, "welcome.html", simplePage{
		Title:    "Your private link",
		Heading:  "This link is your key",
		Subtitle: "Hi " + u.Name + ". There is no password: opening this link is how you get back to your timesheet, on this device or any other.",
		Next:     next,
		Link:     privateLink(r, token),
	})
}

// switchTo signs this browser in with a private link, having asked first
// (see privateLink). It is a POST, so only a form on this site can make it.
func (s *Server) switchTo(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if _, err := s.store.UserByToken(token); err != nil {
		http.NotFound(w, r)
		return
	}
	setSession(w, r, token)
	dest := "/u/" + token
	if next := safeNext(r.URL.Query().Get("next")); next != "/" {
		dest += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// meGet is where old links to a profile page go: it is a dialog on every page.
func (s *Server) meGet(w http.ResponseWriter, r *http.Request, u *store.User) {
	http.Redirect(w, r, "/#profile", http.StatusSeeOther)
}

// meLink hands the caller their own private link. It is fetched on demand
// when they choose to reveal or copy it, rather than being written into every
// page, and it is never cached.
func (s *Server) meLink(w http.ResponseWriter, r *http.Request, u *store.User) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"link": privateLink(r, tokenFrom(r))})
}

// meLinkSaved records that someone has their private link somewhere safe.
// There is no way back from losing it, so until this is set every page says so.
func (s *Server) meLinkSaved(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.store.MarkLinkSaved(u.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
}

// mePost renames the caller. A blank or over-long name leaves it unchanged
// (the form's input already enforces both).
func (s *Server) mePost(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.store.RenameUser(u.ID, r.FormValue("name")); err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
}

// ---- HTML UI ----------------------------------------------------------

type pageData struct {
	View     string // "week" or "shared"
	User     *store.User
	Path     string // this page's URL, so actions can return here
	Today    string // today's date, YYYY-MM-DD
	ThisWeek store.Week
	// The sidebar: the latest weeks with anything logged, and how many
	// links are out.
	Recent     []store.WeekTotal
	ShareCount int

	// The week being shown, and its link if it has been shared.
	Sheet    store.Timesheet
	Share    *store.Share
	ShareURL string
	// NewDate is where the add form starts: today in this week, Monday in any other.
	NewDate string

	// The shared links page.
	Shares []shareRow
}

// IsThisWeek reports whether the page shows the current week.
func (d pageData) IsThisWeek() bool { return d.Sheet.Week == d.ThisWeek }

type shareRow struct {
	store.Share
	URL     string
	Minutes int
}

// recentWeeks is how many weeks the sidebar lists.
const recentWeeks = 8

// basePage fills in what every page with the sidebar needs.
func (s *Server) basePage(r *http.Request, u *store.User, view string) (pageData, error) {
	d := pageData{View: view, User: u, Path: r.URL.RequestURI(), Today: s.today(), ThisWeek: s.thisWeek()}
	recent, err := s.store.RecentWeeks(u.ID, recentWeeks)
	if err != nil {
		return d, err
	}
	// This week is always on the list, logged or not, in its place among
	// the others: newest first.
	if !slices.ContainsFunc(recent, func(wt store.WeekTotal) bool { return wt.Week == d.ThisWeek }) {
		recent = append(recent, store.WeekTotal{Week: d.ThisWeek})
		slices.SortFunc(recent, func(a, b store.WeekTotal) int { return b.Week.Monday().Compare(a.Week.Monday()) })
	}
	d.Recent = recent
	shares, err := s.store.Shares(u.ID)
	d.ShareCount = len(shares)
	return d, err
}

// backTo is where an action should send the browser: the page it came from
// (the form's "next" field), or fallback.
func backTo(r *http.Request, fallback string) string {
	if next := r.FormValue("next"); next != "" {
		return safeNext(next)
	}
	return fallback
}

func (s *Server) pageIndex(w http.ResponseWriter, r *http.Request, u *store.User) {
	http.Redirect(w, r, weekURL(s.thisWeek()), http.StatusSeeOther)
}

func (s *Server) pageWeek(w http.ResponseWriter, r *http.Request, u *store.User) {
	week, err := store.ParseWeek(r.PathValue("week"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.basePage(r, u, "week")
	if err != nil {
		s.fail(w, err)
		return
	}
	if d.Sheet, err = s.store.Timesheet(u.ID, week); err != nil {
		s.fail(w, err)
		return
	}
	d.NewDate = week.First()
	if week.Contains(d.Today) {
		d.NewDate = d.Today
	}
	if sh, err := s.store.WeekShare(u.ID, week); err == nil {
		d.Share, d.ShareURL = &sh, shareLink(r, sh)
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

// pageShared lists every link the caller has out, so they can see who might
// be looking and turn any of them off.
func (s *Server) pageShared(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, err := s.basePage(r, u, "shared")
	if err != nil {
		s.fail(w, err)
		return
	}
	shares, err := s.store.Shares(u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.Shares = []shareRow{}
	for _, sh := range shares {
		sheet, err := s.store.Timesheet(u.ID, sh.Week)
		if err != nil {
			s.fail(w, err)
			return
		}
		d.Shares = append(d.Shares, shareRow{Share: sh, URL: shareLink(r, sh), Minutes: sheet.Minutes})
	}
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.stream(w, r, u.ID, "user:"+strconv.FormatInt(u.ID, 10), maxStreams, nil)
}

// weekOfDate is the week page an entry on date belongs on.
func weekOfDate(date string) string {
	d, ok := parseDate(date)
	if !ok {
		return "/"
	}
	return weekURL(store.WeekOf(d))
}

func (s *Server) formAddEntry(w http.ResponseWriter, r *http.Request, u *store.User) {
	date := r.FormValue("date")
	if strings.TrimSpace(date) == "" {
		date = s.today()
	}
	e, err := s.store.AddEntry(u.ID, date, r.FormValue("start"), r.FormValue("end"), r.FormValue("note"))
	if errors.Is(err, store.ErrInvalid) {
		// The form's own checks catch almost everything; what gets past them
		// (an end before the start) just comes back unchanged.
		http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	// An entry on a day in another week goes to that week's page, so it is
	// on screen rather than seemingly lost.
	http.Redirect(w, r, s.pageFor(r, e.Date), http.StatusSeeOther)
}

// pageFor is where to go after changing an entry on date: back to the page
// the form was on if the entry is there, otherwise to the entry's own week.
func (s *Server) pageFor(r *http.Request, date string) string {
	back := backTo(r, "")
	if week, err := store.ParseWeek(strings.TrimPrefix(strings.SplitN(back, "?", 2)[0], "/week/")); err == nil && week.Contains(date) {
		return back
	}
	return weekOfDate(date)
}

func (s *Server) formEditEntry(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	date, start, end, note := r.FormValue("date"), r.FormValue("start"), r.FormValue("end"), r.FormValue("note")
	e, err := s.store.UpdateEntry(id, u.ID, store.EntryUpdate{Date: &date, Start: &start, End: &end, Note: &note})
	if errors.Is(err, store.ErrInvalid) {
		http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
		return
	} else if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, s.pageFor(r, e.Date), http.StatusSeeOther)
}

func (s *Server) formDeleteEntry(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteEntry(id, u.ID); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
}

func (s *Server) formRestoreEntry(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	e, err := s.store.RestoreEntry(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, s.pageFor(r, e.Date), http.StatusSeeOther)
}

func (s *Server) formShareWeek(w http.ResponseWriter, r *http.Request, u *store.User) {
	week, err := store.ParseWeek(r.PathValue("week"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.ShareWeek(u.ID, week); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, weekURL(week)+"#share", http.StatusSeeOther)
}

func (s *Server) formRevokeShare(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RevokeShare(id, u.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/shared"), http.StatusSeeOther)
}

// ---- shared weeks -----------------------------------------------------

type sharedData struct {
	Sheet store.Timesheet
	Today string
	Token string
	// Owner is the person the week belongs to, when they are the one looking.
	Owner bool
}

// pageSharedWeek is what the link's recipient sees: whose week it is, its
// entries and its totals, and no way to change any of it.
func (s *Server) pageSharedWeek(w http.ResponseWriter, r *http.Request) {
	sh, owner, err := s.store.SharedWeek(r.PathValue("token"))
	if err != nil {
		s.renderTmpl(w, http.StatusNotFound, "gone.html", nil)
		return
	}
	sheet, err := s.store.Timesheet(owner, sh.Week)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	// The link carries no identity and must not ask for one; a cookie the
	// viewer happens to have only decides whether to point out it is theirs.
	u := userFrom(r)
	w.Header().Set("Cache-Control", "no-store")
	s.renderTmpl(w, http.StatusOK, "shared.html", sharedData{
		Sheet: sheet, Today: s.today(), Token: sh.Token, Owner: u != nil && u.ID == owner,
	})
}

// sharedEvents tells a shared week's viewers when its owner changes something,
// including revoking the link, which the page then finds out by reloading.
func (s *Server) sharedEvents(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	_, owner, err := s.store.SharedWeek(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Viewers are counted per link, not per address: behind a proxy every
	// viewer has the proxy's address, and they would share one small cap.
	// While the link lives the owner's changes reach it; once it is revoked,
	// the next signal is its last.
	s.stream(w, r, owner, "share:"+token, maxViewers, func() bool {
		_, _, err := s.store.SharedWeek(token)
		return err == nil
	})
}

// ---- JSON API ---------------------------------------------------------

func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, token, err := s.store.CreateUser(in.Name)
	s.respond(w, http.StatusCreated, struct {
		store.User
		Token string `json:"token"`
		Link  string `json:"link"`
	}{u, token, privateLink(r, token)}, err)
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.respond(w, http.StatusOK, u, nil)
}

// pathWeek reads the week in the path: "2026-W39", or "current".
func (s *Server) pathWeek(w http.ResponseWriter, r *http.Request) (store.Week, bool) {
	v := r.PathValue("week")
	if v == "current" {
		return s.thisWeek(), true
	}
	week, err := store.ParseWeek(v)
	if err != nil {
		writeError(w, http.StatusBadRequest, `a week is written like 2026-W39, or "current"`)
		return week, false
	}
	return week, true
}

func (s *Server) apiWeek(w http.ResponseWriter, r *http.Request, u *store.User) {
	week, ok := s.pathWeek(w, r)
	if !ok {
		return
	}
	sheet, err := s.store.Timesheet(u.ID, week)
	s.respond(w, http.StatusOK, sheet, err)
}

func (s *Server) apiAddEntry(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Date  string `json:"date"`
		Start string `json:"start"`
		End   string `json:"end"`
		Note  string `json:"note"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Date) == "" {
		in.Date = s.today()
	}
	e, err := s.store.AddEntry(u.ID, in.Date, in.Start, in.End, in.Note)
	s.respond(w, http.StatusCreated, e, err)
}

// apiPatchEntry edits an entry. Every field is optional but at least one is
// required.
func (s *Server) apiPatchEntry(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in store.EntryUpdate
	var body struct {
		Date  *string `json:"date"`
		Start *string `json:"start"`
		End   *string `json:"end"`
		Note  *string `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Date == nil && body.Start == nil && body.End == nil && body.Note == nil {
		writeError(w, http.StatusBadRequest, `provide at least one of "date", "start", "end", "note"`)
		return
	}
	in.Date, in.Start, in.End, in.Note = body.Date, body.Start, body.End, body.Note
	e, err := s.store.UpdateEntry(id, u.ID, in)
	s.respond(w, http.StatusOK, e, err)
}

func (s *Server) apiDeleteEntry(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteEntry(id, u.ID); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiRestoreEntry takes an entry back out of the trash, for the day it stays there.
func (s *Server) apiRestoreEntry(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	e, err := s.store.RestoreEntry(id, u.ID)
	s.respond(w, http.StatusOK, e, err)
}

// apiShare is a share as the API gives it: with its link, ready to send.
type apiShare struct {
	store.Share
	URL string `json:"url"`
}

func (s *Server) apiShares(w http.ResponseWriter, r *http.Request, u *store.User) {
	shares, err := s.store.Shares(u.ID)
	out := []apiShare{}
	for _, sh := range shares {
		out = append(out, apiShare{sh, shareLink(r, sh)})
	}
	s.respond(w, http.StatusOK, out, err)
}

func (s *Server) apiShareWeek(w http.ResponseWriter, r *http.Request, u *store.User) {
	week, ok := s.pathWeek(w, r)
	if !ok {
		return
	}
	sh, err := s.store.ShareWeek(u.ID, week)
	s.respond(w, http.StatusOK, apiShare{sh, shareLink(r, sh)}, err)
}

func (s *Server) apiRevokeShare(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.RevokeShare(id, u.ID); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers ----------------------------------------------------------

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// respond writes v as JSON with the given status, or maps err to an HTTP error.
func (s *Server) respond(w http.ResponseWriter, status int, v any, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid input: dates are YYYY-MM-DD, times HH:MM with the end after the start, "+
			"notes one line of up to 200 characters, and names up to 40")
	case err != nil:
		log.Printf("api: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// htmlErr maps a store error to a plain HTTP error page.
func (s *Server) htmlErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	s.fail(w, err)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("web: %v", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
