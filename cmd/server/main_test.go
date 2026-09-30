package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Shutting down must let requests already in progress finish, and serve must
// not return until they have, because main closes the database when it does.
func TestShutdownFinishesRequestsInProgress(t *testing.T) {
	started := make(chan struct{})
	var finished time.Time
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(400 * time.Millisecond) // a slow query, say
		finished = time.Now()
		io.WriteString(w, "all done")
	})}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan time.Time, 1)
	go func() {
		if err := serve(ctx, srv, ln); err != nil {
			t.Errorf("serve: %v", err)
		}
		returned <- time.Now()
	}()

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		got <- result{string(b), err}
	}()

	<-started
	cancel() // shut down while the request is still running

	r := <-got
	if r.err != nil || r.body != "all done" {
		t.Fatalf("the request in progress was cut off: body %q, error %v", r.body, r.err)
	}
	select {
	case at := <-returned:
		if at.Before(finished) {
			t.Error("serve returned before the request in progress had finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve never returned after shutting down")
	}

	// And nothing new is accepted afterwards.
	if _, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
		t.Error("a new request was served after shutdown")
	}
}

// A server that starts on a new, empty database where it used to have one
// has lost every timesheet, and nothing else would say so until people
// found their links no longer worked. The log says it the moment it starts.
func TestANewDatabaseIsAnnounced(t *testing.T) {
	fresh := describeDB("/data/weektime.db", false, 0)
	for _, want := range []string{"created a new, empty one at /data/weektime.db", "every private link", "-v weektime:/data"} {
		if !strings.Contains(fresh, want) {
			t.Errorf("the start-up log for a new database does not say %q:\n%s", want, fresh)
		}
	}
	for users, want := range map[int]string{0: "0 timesheets", 1: "1 timesheet", 7: "7 timesheets"} {
		got := describeDB("/data/weektime.db", true, users)
		if got != "database: /data/weektime.db, "+want {
			t.Errorf("an existing database with %d: %q", users, got)
		}
	}
}
