package keeper

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"path/filepath"
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
	t.Helper()
	sock := filepath.Join(t.TempDir(), "keeper.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Socket: sock,
			Dial:   func() (net.Conn, error) { return net.Dial("tcp", ircAddr) },
		})
	}()
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
