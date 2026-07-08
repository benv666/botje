package admin

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"go-botje/internal/auth"
	"go-botje/internal/storage"
)

// promptMark is how the "> " prompt tail looks with ANSI colors in it.
const promptMark = ">\x1b[0m "

type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func newServer(t *testing.T, specs ...Spec) (*Server, *client) {
	t.Helper()
	a, err := auth.New(storage.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	a.AddUser("benv", "geheim")
	hash, _ := bcrypt.GenerateFromPassword([]byte("supergeheim"), bcrypt.MinCost)
	a.SetSuperuser("root", string(hash))

	s := &Server{
		Auth:     a,
		Exec:     func(fn func()) { fn() },
		Commands: func() []Spec { return specs },
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { ln.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	return s, &client{t: t, conn: conn, r: bufio.NewReader(conn)}
}

// readUntil reads until the marker shows up, returning everything read.
func (c *client) readUntil(marker string) string {
	c.t.Helper()
	var buf strings.Builder
	tmp := make([]byte, 1)
	for {
		if _, err := c.conn.Read(tmp); err != nil {
			c.t.Fatalf("waiting for %q, got so far %q: %v", marker, buf.String(), err)
		}
		buf.WriteByte(tmp[0])
		if strings.Contains(buf.String(), marker) {
			return buf.String()
		}
	}
}

func (c *client) send(line string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) login(user, pass string) {
	c.t.Helper()
	c.readUntil("login: ")
	c.send(user)
	c.readUntil("password: ")
	c.send(pass)
	c.readUntil("Welcome to botje!")
	c.readUntil(promptMark)
}

func echoSpec() Spec {
	return Spec{
		Name:  "echo",
		Match: regexp.MustCompile(`^echo\s*`),
		Help:  "Echoes back what you type",
		Run:   func(args, line string) string { return "you said: " + args },
	}
}

func suSpec(called *bool) Spec {
	return Spec{
		Name:  "secret",
		Match: regexp.MustCompile(`^secret$`),
		Help:  "Superuser only",
		Su:    true,
		Run: func(args, line string) string {
			*called = true
			return "the secret"
		},
	}
}

func TestLoginAndCommand(t *testing.T) {
	_, c := newServer(t, echoSpec())
	c.login("benv", "geheim")
	c.send("echo hallo daar")
	out := c.readUntil(promptMark)
	if !strings.Contains(out, "you said: hallo daar") {
		t.Fatalf("out = %q", out)
	}
}

func TestPasswordEchoSuppressed(t *testing.T) {
	_, c := newServer(t)
	c.readUntil("login: ")
	c.send("benv")
	got := c.readUntil("password: ")
	if !strings.Contains(got, "\xff\xfb\x01") {
		t.Fatalf("no IAC WILL ECHO before password, got %q", got)
	}
	c.send("geheim")
	got = c.readUntil("Welcome")
	if !strings.Contains(got, "\xff\xfc\x01") {
		t.Fatalf("no IAC WONT ECHO after password, got %q", got)
	}
}

func TestThreeStrikesDisconnects(t *testing.T) {
	_, c := newServer(t)
	for i := range 3 {
		c.readUntil("login: ")
		c.send("benv")
		c.readUntil("password: ")
		c.send("fout")
		if i < 2 {
			c.readUntil("Invalid username or password. Try again!")
		}
	}
	c.readUntil("H-h-h-h-HACKER!!!")
	// connection is gone now
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	for {
		if _, err := c.conn.Read(buf); err != nil {
			return
		}
	}
}

func TestSuCommandHiddenFromUsers(t *testing.T) {
	called := false
	_, c := newServer(t, suSpec(&called))
	c.login("benv", "geheim")
	c.send("secret")
	out := c.readUntil(promptMark)
	if called || !strings.Contains(out, "Sorry, that command is unknown.") {
		t.Fatalf("called=%v out=%q", called, out)
	}
}

func TestSuCommandForSuperuser(t *testing.T) {
	called := false
	_, c := newServer(t, suSpec(&called))
	c.login("root", "supergeheim")
	c.send("secret")
	out := c.readUntil(promptMark)
	if !called || !strings.Contains(out, "the secret") {
		t.Fatalf("called=%v out=%q", called, out)
	}
}

func TestHelpListsCommands(t *testing.T) {
	called := false
	_, c := newServer(t, echoSpec(), suSpec(&called))
	c.login("benv", "geheim")
	c.send("help")
	out := c.readUntil(promptMark)
	if !strings.Contains(out, "echo") || !strings.Contains(out, "Echoes back what you type") {
		t.Fatalf("help = %q", out)
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("help = %q, su commands must stay hidden from users", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	_, c := newServer(t)
	c.login("benv", "geheim")
	c.send("frobnicate")
	if out := c.readUntil(promptMark); !strings.Contains(out, "Sorry, that command is unknown.") {
		t.Fatalf("out = %q", out)
	}
}

func TestQuit(t *testing.T) {
	_, c := newServer(t)
	c.login("benv", "geheim")
	c.send("quit")
	c.readUntil("Bye!")
}

func TestAddUserAndPasswd(t *testing.T) {
	s, c := newServer(t)
	c.login("root", "supergeheim")
	c.send("adduser nieuwe wachtwoord")
	c.readUntil(promptMark)
	if got := s.Auth.Check("nieuwe", "wachtwoord"); got != auth.Valid {
		t.Fatalf("Check after adduser = %v", got)
	}
	c.send("passwd nieuwe anders")
	c.readUntil(promptMark)
	if got := s.Auth.Check("nieuwe", "anders"); got != auth.Valid {
		t.Fatalf("Check after passwd = %v", got)
	}
}

func TestPromptShowsUser(t *testing.T) {
	_, c := newServer(t)
	c.readUntil("login: ")
	c.send("benv")
	c.readUntil("password: ")
	c.send("geheim")
	out := c.readUntil(promptMark)
	if !strings.Contains(out, "benv") || !strings.Contains(out, "Botje") {
		t.Fatalf("prompt = %q, want user@Botje>", out)
	}
}

// the admin port must leave an audit trail: connections, login
// success/failure with source address, and executed commands (by spec
// name only, never the raw line - it may contain passwords).
func TestAuditLog(t *testing.T) {
	var mu sync.Mutex
	var logbuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&syncWriter{mu: &mu, w: &logbuf},
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	_, c := newServer(t, Spec{
		Name:  "noop",
		Match: regexp.MustCompile(`^noop`),
		Run:   func(_, _ string) string { return "ok" },
	})
	c.readUntil("login: ")
	c.send("benv")
	c.readUntil("password: ")
	c.send("wrong")
	c.readUntil("login: ")
	c.send("benv")
	c.readUntil("password: ")
	c.send("geheim")
	c.readUntil(promptMark)
	c.send("noop secret-argument")
	c.readUntil(promptMark)

	mu.Lock()
	logs := logbuf.String()
	mu.Unlock()
	for _, want := range []string{
		"admin: connection", "addr=",
		"admin: login failed", "user=benv",
		"admin: login ok",
		"admin: command", "cmd=noop",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("audit log missing %q in:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "secret-argument") || strings.Contains(logs, "geheim") {
		t.Errorf("audit log leaks command arguments or passwords:\n%s", logs)
	}
}

type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
