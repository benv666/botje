package irc

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go-botje/internal/bus"
)

// TestLiveSmoke connects to the junerules ircd over TLS, registers as
// Meretrix, joins #testing, says one line, and quits. Gated behind
// BOTJE_LIVE_TEST=1; never touches other channels or the hoer nick.
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("BOTJE_LIVE_TEST") != "1" {
		t.Skip("set BOTJE_LIVE_TEST=1 for live tests against junerules #testing")
	}
	addr := os.Getenv("BOTJE_IRC_ADDR")
	if addr == "" {
		t.Skip("set BOTJE_IRC_ADDR for live tests (host:port, TLS)")
	}

	var mu sync.Mutex
	joined := make(chan struct{})
	var joinOnce sync.Once
	var events []string

	sess := NewSession("junerules", "Meretrix", time.Now)
	sess.Emit = func(ev *bus.Event) {
		mu.Lock()
		events = append(events, ev.Name)
		mu.Unlock()
		if ev.Name == "IRC_JOIN" && ev.SenderMe && ev.Channel == "#testing" {
			joinOnce.Do(func() { close(joined) })
		}
	}

	lines := make(chan string, 256)
	conn, err := Connect(ConnConfig{
		Network: "junerules",
		Addr:    addr,
		TLS:     true,
		OnLine:  func(l string) { lines <- l },
		OnDisconnect: func(err error) {
			t.Logf("disconnected: %v", err)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sess.Send = conn.Write
	sess.SendHigh = conn.WriteHigh

	// single-goroutine dispatch, like the real core loop
	done := make(chan struct{})
	go func() {
		defer close(done)
		for l := range lines {
			sess.HandleLine(l)
		}
	}()

	sess.Register()
	time.Sleep(3 * time.Second) // registration settle, then join (no 10s wait in tests)
	sess.JoinChannels([]string{"#testing"})

	select {
	case <-joined:
	case <-time.After(30 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("never saw own JOIN #testing; events: %v", events)
	}

	conn.Write(fmt.Sprintf("PRIVMSG #testing :go-botje live smoke test %s: {g}sched/bus/conf/storage/format/irc{/} all green", time.Now().Format("15:04:05")))
	time.Sleep(2 * time.Second)
	conn.Write("QUIT :smoke test done")
	time.Sleep(1 * time.Second)
	conn.Close()
	close(lines)
	<-done

	if got := sess.Channels(); len(got) != 1 || got[0] != "#testing" {
		t.Errorf("Channels = %v, want [#testing]", got)
	}
	if sess.Motd() == "" {
		t.Error("no MOTD received")
	}
	// NAMES tracking against the real ircd: we are always in our own
	// member list (as Meretrix, or underscored after a 433 retry)
	members := sess.Members("#testing")
	t.Logf("members of #testing: %v", members)
	ourselves := false
	for _, m := range members {
		if len(m) >= len("Meretrix") && strings.EqualFold(m[:len("Meretrix")], "Meretrix") {
			ourselves = true
		}
	}
	if !ourselves {
		t.Errorf("Members(#testing) = %v, missing ourselves", members)
	}
}
