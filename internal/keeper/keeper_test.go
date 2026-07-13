package keeper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIRC is a minimal line server on 127.0.0.1 for the IRC side.
type fakeIRC struct {
	ln    net.Listener
	conns chan net.Conn
}

func newFakeIRC(t *testing.T) *fakeIRC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIRC{ln: ln, conns: make(chan net.Conn, 4)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.conns <- c
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeIRC) accept(t *testing.T) net.Conn {
	t.Helper()
	select {
	case c := <-f.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("IRC side never connected")
		return nil
	}
}

func startKeeper(t *testing.T, ircAddr string) string {
	return startKeeperWith(t, ircAddr, func(*Config) {})
}

func startKeeperWith(t *testing.T, ircAddr string, mod func(*Config)) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "keeper.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := Config{
		Socket: sock,
		Dial:   func() (net.Conn, error) { return net.Dial("tcp", ircAddr) },
	}
	mod(&cfg)
	go func() { done <- Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("keeper did not stop")
		}
	})
	return sock
}

// instantSleep skips reconnect delays and records them.
func instantSleep(delays chan time.Duration) func(context.Context, time.Duration) bool {
	return func(ctx context.Context, d time.Duration) bool {
		select {
		case delays <- d:
		default:
		}
		return ctx.Err() == nil
	}
}

// gatedSleep signals that the keeper reached the reconnect sleep (the
// dead session is fully torn down by then) and holds it until release.
func gatedSleep(sleeping chan struct{}, release chan struct{}) func(context.Context, time.Duration) bool {
	return func(ctx context.Context, d time.Duration) bool {
		select {
		case sleeping <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

func dialCore(t *testing.T, sock string) net.Conn {
	t.Helper()
	var c net.Conn
	var err error
	for range 50 { // wait for the socket to appear
		c, err = net.Dial("unix", sock)
		if err == nil {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dial core: %v", err)
	return nil
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return line
}

func TestRelayIRCToCore(t *testing.T) {
	irc := newFakeIRC(t)
	sock := startKeeper(t, irc.ln.Addr().String())
	ircConn := irc.accept(t)

	core := dialCore(t, sock)
	defer core.Close()
	coreR := bufio.NewReader(core)

	fmt.Fprint(ircConn, "PING :hello\r\n")
	if got := readLine(t, coreR); got != "PING :hello\r\n" {
		t.Fatalf("core got %q", got)
	}
}

func TestRelayCoreToIRC(t *testing.T) {
	irc := newFakeIRC(t)
	sock := startKeeper(t, irc.ln.Addr().String())
	ircConn := irc.accept(t)
	ircR := bufio.NewReader(ircConn)

	core := dialCore(t, sock)
	defer core.Close()

	fmt.Fprint(core, "NICK Meretrix\r\n")
	if got := readLine(t, ircR); got != "NICK Meretrix\r\n" {
		t.Fatalf("irc got %q", got)
	}
}

func TestBuffersWhileCoreAway(t *testing.T) {
	irc := newFakeIRC(t)
	sock := startKeeper(t, irc.ln.Addr().String())
	ircConn := irc.accept(t)

	// no core yet: these must buffer, not be lost
	fmt.Fprint(ircConn, ":srv 001 x :welcome\r\nPING :tok\r\n")
	time.Sleep(200 * time.Millisecond)

	core := dialCore(t, sock)
	defer core.Close()
	coreR := bufio.NewReader(core)
	if got := readLine(t, coreR); got != ":srv 001 x :welcome\r\n" {
		t.Fatalf("first buffered = %q", got)
	}
	if got := readLine(t, coreR); got != "PING :tok\r\n" {
		t.Fatalf("second buffered = %q", got)
	}
}

func TestCoreReconnectKeepsIRC(t *testing.T) {
	irc := newFakeIRC(t)
	sock := startKeeper(t, irc.ln.Addr().String())
	ircConn := irc.accept(t)

	core1 := dialCore(t, sock)
	fmt.Fprint(ircConn, "PING :one\r\n")
	if got := readLine(t, bufio.NewReader(core1)); got != "PING :one\r\n" {
		t.Fatalf("core1 got %q", got)
	}
	core1.Close() // core "restarts"

	// IRC connection is untouched: no new accept happens, and lines
	// sent now buffer for the next core
	fmt.Fprint(ircConn, "PING :two\r\n")
	time.Sleep(150 * time.Millisecond)

	core2 := dialCore(t, sock)
	defer core2.Close()
	if got := readLine(t, bufio.NewReader(core2)); got != "PING :two\r\n" {
		t.Fatalf("core2 got %q", got)
	}
	// still the same single IRC connection (no reconnect)
	select {
	case <-irc.conns:
		t.Fatal("keeper made a second IRC connection on core restart")
	case <-time.After(200 * time.Millisecond):
	}
}

// The 2026-07-13 outage: the ircd restarted, the keeper reconnected the
// socket, but the core was never told, so nothing re-registered and the
// ircd dropped the unregistered connection every ~11s until connectban
// z-lined us. The keeper must drop the core when the IRC session dies
// so the core reconnects and re-registers.
func TestIRCDropDisconnectsCore(t *testing.T) {
	irc := newFakeIRC(t)
	sock := startKeeperWith(t, irc.ln.Addr().String(), func(c *Config) {
		c.sleep = instantSleep(make(chan time.Duration, 8))
	})
	ircConn := irc.accept(t)

	core := dialCore(t, sock)
	defer core.Close()
	coreR := bufio.NewReader(core)
	fmt.Fprint(ircConn, "PING :attached\r\n")
	readLine(t, coreR) // core is attached for sure

	ircConn.Close() // the ircd went away
	core.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := coreR.ReadString('\n')
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("core connection survived the IRC drop (err=%v)", err)
	}
	irc.accept(t) // and the keeper reconnected on its own
}

// Inbound bytes buffered from a dead IRC connection must not leak into
// the next core session: they belong to a session that no longer exists
// and can splice into fresh lines.
func TestStaleInboundDroppedOnIRCLoss(t *testing.T) {
	irc := newFakeIRC(t)
	sock := startKeeperWith(t, irc.ln.Addr().String(), func(c *Config) {
		c.sleep = instantSleep(make(chan time.Duration, 8))
	})
	ircConn := irc.accept(t)

	// no core attached: this buffers, then the connection dies
	fmt.Fprint(ircConn, ":srv 001 x :STALE") // no newline: a torn line
	time.Sleep(100 * time.Millisecond)
	ircConn.Close()
	irc2 := irc.accept(t)

	core := dialCore(t, sock)
	defer core.Close()
	fmt.Fprint(irc2, "PING :fresh\r\n")
	if got := readLine(t, bufio.NewReader(core)); got != "PING :fresh\r\n" {
		t.Fatalf("core got %q, stale buffer leaked", got)
	}
}

// A core that re-attaches while IRC is still down sends its NICK/USER
// into the void: the keeper must hold those bytes and flush them the
// moment the new IRC connection is up, or registration is lost again.
func TestOutboundBufferedWhileIRCDown(t *testing.T) {
	irc := newFakeIRC(t)
	release := make(chan struct{})
	sleeping := make(chan struct{}, 4)
	var gate sync.Once
	sock := startKeeperWith(t, irc.ln.Addr().String(), func(c *Config) {
		c.sleep = gatedSleep(sleeping, release)
	})
	ircConn := irc.accept(t)
	ircConn.Close() // IRC dies; redial is gated
	<-sleeping      // the dead session is fully torn down

	core := dialCore(t, sock)
	defer core.Close()
	fmt.Fprint(core, "NICK Meretrix\r\nUSER botje 8 * :botje\r\n")
	time.Sleep(100 * time.Millisecond) // let the bytes reach the keeper
	gate.Do(func() { close(release) })

	irc2 := irc.accept(t)
	irc2.SetReadDeadline(time.Now().Add(5 * time.Second))
	ircR := bufio.NewReader(irc2)
	if got := readLine(t, ircR); got != "NICK Meretrix\r\n" {
		t.Fatalf("irc got %q, want buffered NICK", got)
	}
	if got := readLine(t, ircR); got != "USER botje 8 * :botje\r\n" {
		t.Fatalf("irc got %q, want buffered USER", got)
	}
}

// When the outbound buffer overflows, the head (registration) is kept
// and the tail dropped at a line boundary so the stream stays framed.
func TestOutboundOverflowKeepsHead(t *testing.T) {
	irc := newFakeIRC(t)
	release := make(chan struct{})
	sleeping := make(chan struct{}, 4)
	sock := startKeeperWith(t, irc.ln.Addr().String(), func(c *Config) {
		c.sleep = gatedSleep(sleeping, release)
	})
	ircConn := irc.accept(t)
	ircConn.Close()
	<-sleeping // the dead session is fully torn down

	core := dialCore(t, sock)
	defer core.Close()
	fmt.Fprint(core, "NICK Meretrix\r\n")
	junk := strings.Repeat("x", 400) // fill way past the buffer cap
	for range 2 * maxOutBuffer / 401 {
		fmt.Fprintf(core, "%s\r\n", junk)
	}
	time.Sleep(200 * time.Millisecond)
	close(release)

	irc2 := irc.accept(t)
	irc2.SetReadDeadline(time.Now().Add(5 * time.Second))
	ircR := bufio.NewReader(irc2)
	if got := readLine(t, ircR); got != "NICK Meretrix\r\n" {
		t.Fatalf("irc got %q, want NICK kept at the head", got)
	}
	// everything else written after the flush must arrive un-spliced
	fmt.Fprint(core, "PRIVMSG #x :after\r\n")
	for {
		got := readLine(t, ircR)
		if got == "PRIVMSG #x :after\r\n" {
			return
		}
		if len(got) != 402 { // a complete junk line incl. CRLF
			t.Fatalf("framing broken, got %d-byte line %q", len(got), got[:min(40, len(got))])
		}
	}
}

// Losing an established connection consults the backoff too: dialing
// instantly after every ~11s drop is what got us connectban z-lined.
func TestBackoffAfterConnectionLoss(t *testing.T) {
	irc := newFakeIRC(t)
	delays := make(chan time.Duration, 8)
	sock := startKeeperWith(t, irc.ln.Addr().String(), func(c *Config) {
		c.sleep = instantSleep(delays)
	})
	_ = sock

	irc.accept(t).Close()
	if d := <-delays; d != 3*time.Second {
		t.Fatalf("first loss delay = %v, want 3s", d)
	}
	irc.accept(t).Close()
	if d := <-delays; d != 60*time.Second {
		t.Fatalf("second quick loss delay = %v, want 60s", d)
	}
	irc.accept(t)
}

// A connection that goes silent is dead (NAT drop, hard hang): servers
// ping every couple of minutes, so a long inbound silence means the
// keeper must give up on the socket and reconnect.
func TestSilentConnectionTreatedAsDead(t *testing.T) {
	irc := newFakeIRC(t)
	sock := startKeeperWith(t, irc.ln.Addr().String(), func(c *Config) {
		c.ReadTimeout = 300 * time.Millisecond
		c.sleep = instantSleep(make(chan time.Duration, 8))
	})
	_ = sock

	ircConn := irc.accept(t)
	// traffic keeps it alive: each byte refreshes the deadline
	for range 4 {
		time.Sleep(150 * time.Millisecond)
		fmt.Fprint(ircConn, "PING :alive\r\n")
	}
	select {
	case <-irc.conns:
		t.Fatal("keeper reconnected despite live traffic")
	default:
	}
	// now silence: the keeper must declare it dead and redial
	select {
	case <-irc.conns:
	case <-time.After(3 * time.Second):
		t.Fatal("keeper never gave up on the silent connection")
	}
}
