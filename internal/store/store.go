package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid input")
)

const (
	maxNameLen = 40
	// A note is one short line, as on a paper timesheet. The web form enforces
	// the same limit, so the API and the CLI cannot store something the page
	// would never let you type.
	maxNoteLen = 200
	dateLayout = "2006-01-02"
	// trashRetention is how long a deleted entry stays restorable.
	trashRetention = "-1 day"
)

type User struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// LinkSaved is whether this person has said they have their private link
	// somewhere safe. The link is the whole account, so until they say so,
	// every page reminds them. Kept out of the API: it is about nagging
	// someone in a browser, not about the data.
	LinkSaved bool `json:"-"`
}

// Entry is one stretch of work: a day, when it started and ended, and what
// it was. The duration is worked out from the times, never typed.
type Entry struct {
	ID      int64  `json:"id"`
	Date    string `json:"date"`  // YYYY-MM-DD
	Start   string `json:"start"` // HH:MM
	End     string `json:"end"`   // HH:MM, later the same day
	Minutes int    `json:"minutes"`
	Note    string `json:"note"`
}

// Duration is the entry's length, as "2h 30m".
func (e Entry) Duration() string { return Duration(e.Minutes) }

// EntryUpdate is a partial edit: nil fields are left alone.
type EntryUpdate struct {
	Date, Start, End, Note *string
}

// Day is one day of a timesheet, with its entries in the order they happened.
type Day struct {
	Date    string  `json:"date"`
	Entries []Entry `json:"entries"`
	Minutes int     `json:"minutes"`
}

// Timesheet is somebody's week: all seven days, worked or not, and the total.
type Timesheet struct {
	Week    Week   `json:"week"`
	Name    string `json:"name"`
	Days    []Day  `json:"days"`
	Minutes int    `json:"minutes"`
}

// Entries is every entry in the week, day by day.
func (t Timesheet) Entries() []Entry {
	var all []Entry
	for _, d := range t.Days {
		all = append(all, d.Entries...)
	}
	return all
}

// DaysWorked is how many days have anything logged on them.
func (t Timesheet) DaysWorked() int {
	n := 0
	for _, d := range t.Days {
		if d.Minutes > 0 {
			n++
		}
	}
	return n
}

// Share is a read-only link to one week of somebody's timesheet. Whoever has
// the link sees that week and nothing else, until the owner revokes it.
type Share struct {
	ID        int64     `json:"id"`
	Week      Week      `json:"week"`
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

// WeekTotal is how much was logged in a week.
type WeekTotal struct {
	Week    Week `json:"week"`
	Minutes int  `json:"minutes"`
}

type Store struct {
	db     *sql.DB
	notify func(userID int64)
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	link_saved INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS entries (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	date       TEXT NOT NULL,
	start_min  INTEGER NOT NULL,
	end_min    INTEGER NOT NULL,
	note       TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	deleted_at TIMESTAMP
);
-- Every page asks for one person's entries between two dates.
CREATE INDEX IF NOT EXISTS entries_user_date ON entries(user_id, date);
CREATE TABLE IF NOT EXISTS shares (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	year       INTEGER NOT NULL,
	week       INTEGER NOT NULL,
	token      TEXT NOT NULL UNIQUE,
	revoked    INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS shares_user ON shares(user_id, year, week);
`

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	// Write-ahead logging lets readers carry on while somebody writes, which
	// matters because every change makes each open page fetch itself again.
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite still allows only one writer at a time; busy_timeout above makes
	// the others wait their turn rather than fail. An in-memory database is
	// private to its connection, so a second one would open an empty database:
	// those stay on one.
	conns := 8
	if strings.Contains(path, ":memory:") || strings.Contains(path, "mode=memory") {
		conns = 1
	}
	db.SetMaxOpenConns(conns)
	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.purgeTrash(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate upgrades databases created by earlier versions.
func (s *Store) migrate() error {
	// A week has one live link at most, which the database now enforces.
	// Two requests at once could make a second one before, so a database
	// from then may have spares: keep each week's oldest, which is the one
	// the page showed and the owner will have sent, and revoke the rest.
	if _, err := s.db.Exec(`UPDATE shares SET revoked = 1 WHERE revoked = 0 AND id NOT IN
		(SELECT MIN(id) FROM shares WHERE revoked = 0 GROUP BY user_id, year, week)`); err != nil {
		return err
	}
	_, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS shares_one_live ON shares(user_id, year, week) WHERE revoked = 0`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// SetNotifier registers a callback fired after every change, with the person
// whose timesheet changed. Their open pages, and anyone watching a week they
// have shared, refresh themselves.
func (s *Store) SetNotifier(fn func(userID int64)) { s.notify = fn }

func (s *Store) changed(userID int64) {
	if s.notify != nil {
		s.notify(userID)
	}
}

// ---- users ------------------------------------------------------------

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func newShareToken() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxNameLen {
		return "", ErrInvalid
	}
	return name, nil
}

// CreateUser adds a person and returns their secret token, which is what
// their private link is made of. Only a hash of it is stored, so it cannot be
// recovered later from the database.
func (s *Store) CreateUser(name string) (User, string, error) {
	name, err := cleanName(name)
	if err != nil {
		return User{}, "", err
	}
	token := newToken()
	res, err := s.db.Exec(`INSERT INTO users (name, token_hash) VALUES (?, ?)`, name, hashToken(token))
	if err != nil {
		return User{}, "", err
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Name: name}, token, nil
}

// TokenIn finds the token in whatever someone pasted: their private link, or
// just the token that ends it.
func TokenIn(s string) string {
	s = strings.TrimSpace(s)
	if _, after, ok := strings.Cut(s, "/u/"); ok {
		s = after
	}
	s, _, _ = strings.Cut(s, "?")
	s, _, _ = strings.Cut(s, "#")
	return strings.Trim(s, "/ ")
}

func (s *Store) UserByToken(token string) (User, error) {
	var u User
	if token == "" {
		return u, ErrNotFound
	}
	err := s.db.QueryRow(`SELECT id, name, link_saved FROM users WHERE token_hash = ?`,
		hashToken(token)).Scan(&u.ID, &u.Name, &u.LinkSaved)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) RenameUser(id int64, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	if err := affected(s.db.Exec(`UPDATE users SET name = ? WHERE id = ?`, name, id)); err != nil {
		return err
	}
	s.changed(id) // the name is on every week they have shared
	return nil
}

// MarkLinkSaved records that someone has their private link somewhere safe,
// which stops the app reminding them about it.
func (s *Store) MarkLinkSaved(id int64) error {
	_, err := s.db.Exec(`UPDATE users SET link_saved = 1 WHERE id = ?`, id)
	return err
}

// ---- entries ----------------------------------------------------------

// cleanNote keeps a note to one short line.
func cleanNote(note string) (string, error) {
	note = strings.Join(strings.Fields(note), " ")
	if utf8.RuneCountInString(note) > maxNoteLen {
		return "", ErrInvalid
	}
	return note, nil
}

// cleanDate validates a YYYY-MM-DD date. It must fall in a week that can
// be written down and shown (see Week.valid), or the entry would be saved
// on a page nobody can open.
func cleanDate(date string) (string, error) {
	date = strings.TrimSpace(date)
	d, err := time.Parse(dateLayout, date)
	if err != nil || !WeekOf(d).valid() {
		return "", ErrInvalid
	}
	return date, nil
}

// cleanEntry validates an entry's fields. An entry stays inside one day and
// ends after it starts; work that runs past midnight is two entries.
func cleanEntry(date, start, end, note string) (string, int, int, string, error) {
	date, err := cleanDate(date)
	if err != nil {
		return "", 0, 0, "", err
	}
	from, ok1 := parseClock(start)
	to, ok2 := parseClock(end)
	if !ok1 || !ok2 || to <= from {
		return "", 0, 0, "", ErrInvalid
	}
	if note, err = cleanNote(note); err != nil {
		return "", 0, 0, "", err
	}
	return date, from, to, note, nil
}

const entrySelect = `SELECT id, date, start_min, end_min, note FROM entries `

func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var (
		e        Entry
		from, to int
	)
	if err := row.Scan(&e.ID, &e.Date, &from, &to, &e.Note); err != nil {
		return e, err
	}
	e.Start, e.End, e.Minutes = clock(from), clock(to), to-from
	return e, nil
}

// AddEntry logs a stretch of work for userID.
func (s *Store) AddEntry(userID int64, date, start, end, note string) (Entry, error) {
	date, from, to, note, err := cleanEntry(date, start, end, note)
	if err != nil {
		return Entry{}, err
	}
	res, err := s.db.Exec(`INSERT INTO entries (user_id, date, start_min, end_min, note) VALUES (?, ?, ?, ?, ?)`,
		userID, date, from, to, note)
	if err != nil {
		return Entry{}, err
	}
	id, _ := res.LastInsertId()
	s.changed(userID)
	return s.Entry(id, userID)
}

// Entry returns one of userID's entries. Someone else's looks exactly like one
// that does not exist.
func (s *Store) Entry(id, userID int64) (Entry, error) {
	e, err := scanEntry(s.db.QueryRow(entrySelect+`WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

// UpdateEntry changes the fields that are set in u. The result must still be
// a valid entry: an edit that would make it end before it starts is refused.
//
// It is one statement, so two edits at once to different fields (the CLI
// and a browser, say) both land: each field not being changed is left as
// the database has it, not as it was when somebody last looked.
func (s *Store) UpdateEntry(id, userID int64, u EntryUpdate) (Entry, error) {
	var date, from, to, note any // nil leaves the column as it is
	if u.Date != nil {
		d, err := cleanDate(*u.Date)
		if err != nil {
			return Entry{}, err
		}
		date = d
	}
	for _, f := range []struct {
		in  *string
		out *any
	}{{u.Start, &from}, {u.End, &to}} {
		if f.in != nil {
			m, ok := parseClock(*f.in)
			if !ok {
				return Entry{}, ErrInvalid
			}
			*f.out = m
		}
	}
	if u.Note != nil {
		n, err := cleanNote(*u.Note)
		if err != nil {
			return Entry{}, err
		}
		note = n
	}
	err := affected(s.db.Exec(`UPDATE entries SET date = COALESCE(?, date), start_min = COALESCE(?, start_min),
		end_min = COALESCE(?, end_min), note = COALESCE(?, note)
		WHERE id = ? AND user_id = ? AND deleted_at IS NULL AND COALESCE(?, end_min) > COALESCE(?, start_min)`,
		date, from, to, note, id, userID, to, from))
	if errors.Is(err, ErrNotFound) {
		// Nothing changed: either there is no such entry, or the edit would
		// have left it ending before it starts.
		if _, err := s.Entry(id, userID); err != nil {
			return Entry{}, err
		}
		return Entry{}, ErrInvalid
	} else if err != nil {
		return Entry{}, err
	}
	s.changed(userID)
	return s.Entry(id, userID)
}

// DeleteEntry moves an entry to the trash, from which RestoreEntry can bring
// it back for a day.
func (s *Store) DeleteEntry(id, userID int64) error {
	err := affected(s.db.Exec(`UPDATE entries SET deleted_at = CURRENT_TIMESTAMP
		WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, id, userID))
	if err == nil {
		s.changed(userID)
	}
	return err
}

// RestoreEntry takes an entry back out of the trash.
func (s *Store) RestoreEntry(id, userID int64) (Entry, error) {
	err := affected(s.db.Exec(`UPDATE entries SET deleted_at = NULL
		WHERE id = ? AND user_id = ? AND deleted_at >= datetime('now', ?)`, id, userID, trashRetention))
	if err != nil {
		return Entry{}, err
	}
	s.changed(userID)
	return s.Entry(id, userID)
}

// purgeTrash forgets entries deleted more than a day ago.
func (s *Store) purgeTrash() error {
	_, err := s.db.Exec(`DELETE FROM entries WHERE deleted_at < datetime('now', ?)`, trashRetention)
	return err
}

// Timesheet is userID's week: every day, Monday first, with what was logged
// on it in the order it happened, and the totals.
func (s *Store) Timesheet(userID int64, w Week) (Timesheet, error) {
	t := Timesheet{Week: w}
	if err := s.db.QueryRow(`SELECT name FROM users WHERE id = ?`, userID).Scan(&t.Name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return t, ErrNotFound
		}
		return t, err
	}
	rows, err := s.db.Query(entrySelect+`WHERE user_id = ? AND deleted_at IS NULL AND date BETWEEN ? AND ?
		ORDER BY date, start_min, end_min, id`, userID, w.First(), w.Last())
	if err != nil {
		return t, err
	}
	defer rows.Close()
	byDate := map[string][]Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return t, err
		}
		byDate[e.Date] = append(byDate[e.Date], e)
	}
	if err := rows.Err(); err != nil {
		return t, err
	}
	for _, date := range w.Days() {
		d := Day{Date: date, Entries: byDate[date]}
		if d.Entries == nil {
			d.Entries = []Entry{} // an empty day is [] in JSON, not null
		}
		for _, e := range d.Entries {
			d.Minutes += e.Minutes
		}
		t.Minutes += d.Minutes
		t.Days = append(t.Days, d)
	}
	return t, nil
}

// RecentWeeks is how much userID logged in each of their latest weeks with
// anything in them, newest first, at most limit of them.
//
// Every page asks for this, so it must not cost more as the years go by. It
// does not: the entries_user_date index hands SQLite the days newest first,
// already grouped, so it streams them, and the loop stops reading as soon as
// it has limit weeks.
func (s *Store) RecentWeeks(userID int64, limit int) ([]WeekTotal, error) {
	rows, err := s.db.Query(`SELECT date, SUM(end_min - start_min) FROM entries
		WHERE user_id = ? AND deleted_at IS NULL GROUP BY date ORDER BY date DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	weeks := []WeekTotal{}
	for rows.Next() {
		var (
			date    string
			minutes int
		)
		if err := rows.Scan(&date, &minutes); err != nil {
			return nil, err
		}
		day, err := time.Parse(dateLayout, date)
		if err != nil {
			continue
		}
		w := WeekOf(day)
		if n := len(weeks); n > 0 && weeks[n-1].Week == w {
			weeks[n-1].Minutes += minutes
			continue
		}
		if len(weeks) == limit {
			break // an older week than anyone asked for
		}
		weeks = append(weeks, WeekTotal{w, minutes})
	}
	return weeks, rows.Err()
}

// ---- shares -----------------------------------------------------------

const shareSelect = `SELECT id, year, week, token, created_at FROM shares `

func scanShare(row interface{ Scan(...any) error }) (Share, error) {
	var sh Share
	err := row.Scan(&sh.ID, &sh.Week.Year, &sh.Week.Num, &sh.Token, &sh.CreatedAt)
	return sh, err
}

// ShareWeek returns the read-only link to userID's week w, making one if
// there is none. Asking twice gives the same link, so sending it again does
// not leave several in circulation.
func (s *Store) ShareWeek(userID int64, w Week) (Share, error) {
	// Two requests at once (a double click) must not make two links, so
	// rather than look first and insert after, insert unless the week has a
	// live link already, and then read back whichever one it has.
	if _, err := s.db.Exec(`INSERT INTO shares (user_id, year, week, token) VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING`, userID, w.Year, w.Num, newShareToken()); err != nil {
		return Share{}, err
	}
	return s.WeekShare(userID, w)
}

// WeekShare is the link userID has out for week w, if there is one.
func (s *Store) WeekShare(userID int64, w Week) (Share, error) {
	sh, err := scanShare(s.db.QueryRow(shareSelect+`WHERE user_id = ? AND year = ? AND week = ? AND revoked = 0`,
		userID, w.Year, w.Num))
	if errors.Is(err, sql.ErrNoRows) {
		return sh, ErrNotFound
	}
	return sh, err
}

// Shares is every link userID has out, the latest week first.
func (s *Store) Shares(userID int64) ([]Share, error) {
	rows, err := s.db.Query(shareSelect+`WHERE user_id = ? AND revoked = 0 ORDER BY year DESC, week DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	shares := []Share{}
	for rows.Next() {
		sh, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		shares = append(shares, sh)
	}
	return shares, rows.Err()
}

// SharedWeek looks up a link: the week it shows and whose it is. A revoked
// link, or one that never existed, is ErrNotFound either way.
func (s *Store) SharedWeek(token string) (Share, int64, error) {
	if token == "" {
		return Share{}, 0, ErrNotFound
	}
	var owner int64
	var sh Share
	err := s.db.QueryRow(`SELECT id, user_id, year, week, token, created_at FROM shares WHERE token = ? AND revoked = 0`, token).
		Scan(&sh.ID, &owner, &sh.Week.Year, &sh.Week.Num, &sh.Token, &sh.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sh, 0, ErrNotFound
	}
	return sh, owner, err
}

// RevokeShare turns one of userID's links off for good. Sharing the week
// again makes a new link; the old one never comes back.
func (s *Store) RevokeShare(id, userID int64) error {
	err := affected(s.db.Exec(`UPDATE shares SET revoked = 1 WHERE id = ? AND user_id = ? AND revoked = 0`, id, userID))
	if err == nil {
		s.changed(userID) // so anyone looking at it finds out now
	}
	return err
}

// affected turns "no rows changed" into ErrNotFound.
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
