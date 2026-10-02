package reclaimd

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// The regression: an open event stream held http.Server.Shutdown open for its
// entire grace period, because Shutdown waits for connections to go idle and a
// stream that is dutifully holding one open never is. Every restart of the
// daemon burned the full 15s, logged a failure, and left the reverse proxy in
// front of it serving 502s for the duration.
func TestShutdownIsNotHeldOpenByAStream(t *testing.T) {
	hub := NewHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		hub.ServeSSE(w, r, map[string]any{"hello": true})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read the hello so the handler is known to be parked in its select loop.
	// If it were still setting up, this would pass for the wrong reason.
	br := bufio.NewReader(resp.Body)
	for i := 0; i < 2; i++ {
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("stream did not start: %v", err)
		}
	}

	hub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown blocked by an open stream after %v: %v", time.Since(start), err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("shutdown took %v; the stream was still holding it", d)
	}
}

// The regression: a page that had only just loaded was replayed the oldest
// frames in the ring, eight of them, and reloaded itself once for each. The
// loads overlapped and the healing waterfall drew every pass eight times.
func TestOnlyAResumingStreamIsReplayed(t *testing.T) {
	hub := NewHub()
	for i := 0; i < 20; i++ {
		hub.Publish("state", map[string]any{"change": "SCAN_END"})
	}

	drain := func(lastID uint64) []Frame {
		ch, cancel, ok := hub.Subscribe(lastID)
		if !ok {
			t.Fatal("subscribe was refused")
		}
		defer cancel()
		var got []Frame
		for len(ch) > 0 {
			got = append(got, <-ch)
		}
		return got
	}

	if got := drain(0); len(got) != 0 {
		t.Errorf("a fresh stream was replayed %d frames", len(got))
	}

	got := drain(17)
	if len(got) != 3 || got[0].ID != 18 || got[2].ID != 20 {
		t.Errorf("resuming after 17 replayed %+v, want frames 18 to 20", got)
	}

	// Further behind than the buffer holds: one resync, not the oldest few.
	got = drain(2)
	if len(got) != 1 || got[0].Event != "resync" || got[0].ID != 20 {
		t.Errorf("resuming after 2 replayed %+v, want a single resync", got)
	}
}

// A request that arrives while the hub is closing must be refused a stream.
// The alternative is a channel nothing will ever close.
func TestClosedHubRefusesNewStreams(t *testing.T) {
	hub := NewHub()

	ch, cancel, ok := hub.Subscribe(0)
	if !ok {
		t.Fatal("first subscribe was refused")
	}
	defer cancel()

	hub.Close()

	if _, ok := <-ch; ok {
		t.Error("an existing subscriber's channel was not closed")
	}
	if _, _, ok := hub.Subscribe(0); ok {
		t.Error("a closed hub handed out a new stream")
	}

	// Idempotent, and publishing afterwards must not panic on a closed channel.
	hub.Close()
	hub.Publish("event", map[string]any{"x": 1})
}
