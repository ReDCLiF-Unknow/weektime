// Command weektime is a CLI client for the Weektime server's HTTP API.
package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/spf13/cobra"

	"weektime/internal/store"
)

type apiError struct {
	Error string `json:"error"`
}

// share is a read-only link as the API gives it.
type share struct {
	store.Share
	URL string `json:"url"`
}

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newRoot builds the command tree. It is separate from main so that tests can
// run commands against a test server and read what they print.
func newRoot() *cobra.Command {
	var server, token string
	client := resty.New()

	root := &cobra.Command{
		Use:           "weektime",
		Short:         "Log and share your working hours from the terminal",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			// A private link says which server it belongs to, so with one of
			// those there is no need to name the server as well.
			if !cmd.Flags().Changed("server") && os.Getenv("WEEKTIME_SERVER") == "" {
				if u, err := url.Parse(strings.TrimSpace(token)); err == nil && u.Host != "" && strings.Contains(u.Path, "/u/") {
					server = u.Scheme + "://" + u.Host
				}
			}
			client.SetBaseURL(server).SetError(&apiError{})
			if t := store.TokenIn(token); t != "" {
				client.SetAuthToken(t)
			}
		},
	}
	root.PersistentFlags().StringVarP(&server, "server", "s", envOr("WEEKTIME_SERVER", "http://localhost:8080"), "server base URL (env WEEKTIME_SERVER)")
	root.PersistentFlags().StringVarP(&token, "token", "t", os.Getenv("WEEKTIME_TOKEN"), "your private link, or the token at the end of it (env WEEKTIME_TOKEN)")

	// check turns a resty response into a Go error.
	check := func(resp *resty.Response, err error) error {
		if err != nil {
			return err
		}
		if resp.IsError() {
			if e, ok := resp.Error().(*apiError); ok && e.Error != "" {
				return fmt.Errorf("%s: %s", resp.Status(), e.Error)
			}
			return fmt.Errorf("%s", resp.Status())
		}
		return nil
	}

	register := &cobra.Command{Use: "register NAME", Short: "Start a timesheet and print its private link", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var u struct {
				store.User
				Token string `json:"token"`
				Link  string `json:"link"`
			}
			if err := check(client.R().SetBody(map[string]string{"name": args[0]}).SetResult(&u).Post("/api/users")); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Started a timesheet for %s. Your private link (keep it secret, and somewhere safe):\n\n  %s\n\n", u.Name, u.Link)
			fmt.Fprintf(out, "Open it in a browser, or use it here with:  --token <link>   or   set WEEKTIME_TOKEN=<link>\n")
			return nil
		}}

	whoami := &cobra.Command{Use: "whoami", Short: "Show whose timesheet you are using", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var u store.User
			if err := check(client.R().SetResult(&u).Get("/api/me")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (id %d)\n", u.Name, u.ID)
			return nil
		}}

	week := &cobra.Command{Use: "week [WEEK]", Short: "Show a week's entries and totals", Args: cobra.MaximumNArgs(1),
		Long:    "Show a week's entries and totals: this week, or the one named like 2026-W39.",
		Example: "  weektime week\n  weektime week 2026-W39",
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := weekArg(args)
			if err != nil {
				return err
			}
			var sheet store.Timesheet
			if err := check(client.R().SetResult(&sheet).Get("/api/weeks/" + w)); err != nil {
				return err
			}
			return printSheet(cmd, sheet)
		}}

	var date string
	logCmd := &cobra.Command{Use: "log START END [NOTE...]", Short: "Log a stretch of work (today, unless --date says otherwise)", Args: cobra.MinimumNArgs(2),
		Example: "  weektime log 09:00 11:30 Client call and proposal draft\n  weektime log 13:00 17:00 \"Workshop\" --date 2026-10-14",
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]string{"date": date, "start": args[0], "end": args[1], "note": strings.Join(args[2:], " ")}
			var e store.Entry
			if err := check(client.R().SetBody(body).SetResult(&e).Post("/api/entries")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged entry %d: %s, %s–%s (%s)\n", e.ID, day(e.Date), e.Start, e.End, e.Duration())
			return nil
		}}
	logCmd.Flags().StringVarP(&date, "date", "d", "", "the day, YYYY-MM-DD (default today)")

	// edit changes only the fields whose flags were given.
	edit := &cobra.Command{Use: "edit ENTRY_ID", Short: "Change an entry's date, times or note", Args: cobra.ExactArgs(1),
		Example: "  weektime edit 12 --end 12:15\n  weektime edit 12 --note \"\"",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			body := map[string]string{}
			for _, flag := range []string{"date", "start", "end", "note"} {
				if cmd.Flags().Changed(flag) {
					body[flag], _ = cmd.Flags().GetString(flag)
				}
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change: pass --date, --start, --end and/or --note")
			}
			var e store.Entry
			if err := check(client.R().SetBody(body).SetResult(&e).Patch("/api/entries/" + id)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated entry %d: %s, %s–%s (%s)\n", e.ID, day(e.Date), e.Start, e.End, e.Duration())
			return nil
		}}
	edit.Flags().String("date", "", "the day, YYYY-MM-DD")
	edit.Flags().String("start", "", "start time, HH:MM")
	edit.Flags().String("end", "", "end time, HH:MM")
	edit.Flags().String("note", "", "what it was")

	rm := &cobra.Command{Use: "rm ENTRY_ID", Short: "Delete an entry", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			if err := check(client.R().Delete("/api/entries/" + id)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted entry %s. It can be brought back for a day: weektime restore %s\n", id, id)
			return nil
		}}

	restore := &cobra.Command{Use: "restore ENTRY_ID", Short: "Undo deleting an entry, within a day", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var e store.Entry
			if err := check(client.R().SetResult(&e).Post("/api/entries/" + id + "/restore")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restored entry %d: %s, %s–%s\n", e.ID, day(e.Date), e.Start, e.End)
			return nil
		}}

	shareCmd := &cobra.Command{Use: "share [WEEK]", Short: "Print a read-only link to a week", Args: cobra.MaximumNArgs(1),
		Long: "Print a read-only link to this week, or the one named like 2026-W39. Whoever opens it sees\n" +
			"your name, that week's entries and its totals, and cannot change anything.\n" +
			"Asking again for the same week gives the same link; `weektime revoke` turns it off.",
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := weekArg(args)
			if err != nil {
				return err
			}
			var sh share
			if err := check(client.R().SetResult(&sh).Post("/api/weeks/" + w + "/share")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Read-only link to %s (share %d):\n\n  %s\n", sh.Week, sh.ID, sh.URL)
			return nil
		}}

	shares := &cobra.Command{Use: "shares", Short: "List the read-only links you have out", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ss []share
			if err := check(client.R().SetResult(&ss).Get("/api/shares")); err != nil {
				return err
			}
			if len(ss) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No week is shared.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tWEEK\tLINK")
			for _, sh := range ss {
				fmt.Fprintf(w, "%d\t%s\t%s\n", sh.ID, sh.Week, sh.URL)
			}
			return w.Flush()
		}}

	revoke := &cobra.Command{Use: "revoke SHARE_ID", Short: "Turn a read-only link off", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			if err := check(client.R().Delete("/api/shares/" + id)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked share %s. Nobody can open that link any more.\n", id)
			return nil
		}}

	root.AddCommand(register, whoami)
	root.AddCommand(week, logCmd, edit, rm, restore)
	root.AddCommand(shareCmd, shares, revoke)
	return root
}

// printSheet writes a week as a table, then each day's total and the week's.
func printSheet(cmd *cobra.Command, sheet store.Timesheet) error {
	out := cmd.OutOrStdout()
	from, to := sheet.Week.Monday(), sheet.Week.Monday().AddDate(0, 0, 6)
	fmt.Fprintf(out, "%s · %s · %s to %s\n\n", sheet.Name, sheet.Week, from.Format("Mon 2 Jan"), to.Format("Mon 2 Jan 2006"))
	if len(sheet.Entries()) == 0 {
		fmt.Fprintln(out, "Nothing logged this week.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tDATE\tSTART\tEND\tDURATION\tNOTE")
	for _, d := range sheet.Days {
		for i, e := range d.Entries {
			date := day(e.Date)
			if i > 0 {
				date = ""
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", e.ID, date, e.Start, e.End, e.Duration(), e.Note)
		}
	}
	// A gap before the totals. An empty line would end tabwriter's columns,
	// so the totals would line up only with each other; a row of empty cells
	// keeps them under DATE and DURATION.
	fmt.Fprintln(w, "\t\t\t\t\t")
	for _, d := range sheet.Days {
		if d.Minutes > 0 {
			fmt.Fprintf(w, "\t%s\t\t\t%s\t\n", day(d.Date), store.Duration(d.Minutes))
		}
	}
	fmt.Fprintf(w, "\tWEEK\t\t\t%s\t%s\n", store.Duration(sheet.Minutes), plural(sheet.DaysWorked(), "day"))
	return w.Flush()
}

// weekArg is the week a command was given, checked here so that a typo is
// explained rather than sent to the server; "current" when there is none.
func weekArg(args []string) (string, error) {
	if len(args) == 0 {
		return "current", nil
	}
	w, err := store.ParseWeek(args[0])
	if err != nil {
		return "", fmt.Errorf("invalid week %q: write it like 2026-W39", args[0])
	}
	return w.String(), nil
}

// day writes a YYYY-MM-DD date as "Tue 14 Oct".
func day(date string) string {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return d.Format("Mon 2 Jan")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func parseID(s string) (string, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("invalid id %q", s)
	}
	return s, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
