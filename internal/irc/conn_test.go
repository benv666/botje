package irc

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// startConn wires a Conn to an in-memory pipe; returns the server side.
func startConn(t *testing.T, onLine func(string), onDisc func(error)) (*Conn, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	if onLine == nil {
		onLine = func(string) {}
	}
	if onDisc == nil {
		onDisc = func(error) {}
	}
	c, err := Connect(ConnConfig{
		Network:      "test",
		OnLine:       onLine,
		OnDisconnect: onDisc,
		Dial:         func() (net.Conn, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); server.Close() })
	return c, server
}

func TestConnDeliversInboundLines(t *testing.T) {
	lines := make(chan string, 10)
	_, server := startConn(t, func(l string) { lines <- l }, nil)

	go server.Write([]byte(":srv 001 x :welcome\r\nPING :tok\r\n"))
	for _, want := range []string{":srv 001 x :welcome", "PING :tok"} {
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("line = %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for inbound line")
		}
	}
}

func TestConnWriteColorizesTruncatesAndTerminates(t *testing.T) {
	c, server := startConn(t, nil, nil)
	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(server)

	c.Write("PRIVMSG #a :{r}hi{/}")
	got, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got != "PRIVMSG #a :\x0305hi\x0f\r\n" {
		t.Fatalf("wire = %q", got)
	}

	c.Write("PRIVMSG #a :" + strings.Repeat("x", 600))
	got, err = r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 512 || !strings.HasSuffix(got, "\r\n") {
		t.Fatalf("wire len = %d, want 510+CRLF", len(got))
	}
}

func TestConnDisconnectCallback(t *testing.T) {
	disc := make(chan error, 1)
	_, server := startConn(t, nil, func(err error) { disc <- err })
	server.Close()
	select {
	case <-disc:
	case <-time.After(5 * time.Second):
		t.Fatal("OnDisconnect never fired")
	}
}
