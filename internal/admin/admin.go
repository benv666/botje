// Package admin is the telnet control port (1924 on loopback in
// prod), the Go counterpart of the Perl Command.pm: login with three
// strikes ("H-h-h-h-HACKER!!!"), telnet IAC echo suppression around
// passwords, and prefix-regex command dispatch with superuser gating.
// The Perl eval backdoor does not exist here, by decision. Output
// {x} tags become ANSI colors (telnet is a terminal, not IRC).
package admin

import (
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"

	"go-botje/internal/auth"
	"go-botje/internal/format"
)

// telnet IAC bits we use
const (
	iac      = "\xff"
	iacWill  = "\xff\xfb"
	iacWont  = "\xff\xfc"
	optEcho  = "\x01"
	doEcho   = "\xff\xfd\x01" // provoke telnet clients to identify themselves
	maxTries = 3
)

// Spec is one admin command (the Perl COMMAND event entry).
type Spec struct {
	Name  string         // command name for help
	Match *regexp.Regexp // matched against the start of the line
	Help  string
	Args  []string // argument placeholders for help
	Su    bool     // superuser only
	// Run gets the remainder after the match and the full line, and
	// returns the output. Runs on the dispatcher via Exec.
	Run func(args, line string) string
}

// Server is the admin port. Wire Exec to the dispatcher and Commands
// to the COMMAND event collection plus core builtins.
type Server struct {
	Auth     *auth.Auth
	Exec     func(fn func())
	Commands func() []Spec
}

// login states, Perl values kept for familiarity
const (
	stateUser = 2 // waiting for username
	statePass = 3 // waiting for password
	stateIn   = 1 // logged in
)

type session struct {
	srv      *Server
	conn     net.Conn
	remote   string // for the audit log
	state    int
	username string
	su       bool
	tries    int
}

// Serve accepts connections until the listener closes.
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	sess := &session{srv: s, conn: conn, state: stateUser,
		remote: conn.RemoteAddr().String()}
	// debug, not info: the docker health check dials this port every 30s
	// and would fill ops.log; anyone who actually tries to log in shows
	// up via the login ok/failed lines
	slog.Debug("admin: connection", "addr", sess.remote)
	sess.write(doEcho)
	sess.write("login: ")

	lines := lineReader(conn)
	for line := range lines {
		done := false
		s.Exec(func() { done = sess.handleLine(line) })
		if done {
			return
		}
	}
}

// lineReader yields telnet-IAC-stripped lines.
func lineReader(conn net.Conn) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		buf := make([]byte, 1024)
		pending := ""
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				pending += stripIAC(string(buf[:n]))
				for {
					i := strings.IndexByte(pending, '\n')
					if i < 0 {
						break
					}
					out <- strings.TrimRight(pending[:i], "\r")
					pending = pending[i+1:]
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}

// stripIAC removes telnet command sequences: IAC + option verb (3
// bytes) or IAC + single command (2 bytes).
func stripIAC(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0xff {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] >= 0xfb && s[i+1] <= 0xfe {
			i += 2 // IAC WILL/WONT/DO/DONT <option>
		} else {
			i++ // IAC <command>
		}
	}
	return b.String()
}

func (se *session) write(s string) {
	se.conn.Write([]byte(format.ToANSI(s)))
}

// handleLine processes one input line; true means disconnect.
func (se *session) handleLine(line string) bool {
	switch se.state {
	case stateUser:
		se.username = line
		se.state = statePass
		se.write(iacWill + optEcho) // echo off, if the client plays telnet
		se.write("password: ")
		return false
	case statePass:
		se.write(iacWont + optEcho)
		se.write("\n")
		return se.checkLogin(line)
	}

	line = strings.TrimSpace(line)
	if regexp.MustCompile(`^(?i)(q(uit)?|exit)$`).MatchString(line) {
		se.write("Bye!\n")
		return true
	}
	se.dispatch(line)
	se.prompt()
	return false
}

func (se *session) checkLogin(password string) bool {
	res := se.srv.Auth.Check(se.username, password)
	if res != auth.Valid && res != auth.Super {
		se.tries++
		slog.Warn("admin: login failed", "user", se.username, "addr", se.remote, "attempt", se.tries)
		if se.tries >= maxTries {
			slog.Warn("admin: disconnected after failed logins", "addr", se.remote)
			se.write("H-h-h-h-HACKER!!!\n")
			return true
		}
		se.write("Invalid username or password. Try again!\n\n")
		se.state = stateUser
		se.write("login: ")
		return false
	}
	se.su = res == auth.Super
	slog.Info("admin: login ok", "user", se.username, "su", se.su, "addr", se.remote)
	se.state = stateIn
	se.write("\nWelcome to botje! Enter '{y}?{/}' or '{y}help{/}' for help.\n\n")
	se.prompt()
	return false
}

func (se *session) prompt() {
	color := "{g}"
	if se.su {
		color = "{r}"
	}
	se.write(color + se.username + "{w}@{/}{c}Botje{w}>{/} ")
}

func (se *session) specs() []Spec {
	specs := se.srv.Commands()
	specs = append(specs, se.builtins()...)
	return specs
}

func (se *session) dispatch(line string) {
	for _, spec := range se.specs() {
		if spec.Su && !se.su {
			continue
		}
		loc := spec.Match.FindStringIndex(line)
		if loc == nil || loc[0] != 0 {
			continue
		}
		args := strings.TrimLeft(line[loc[1]:], " \t")
		// audit by spec name only: the raw line may contain passwords
		// (passwd, adduser)
		slog.Info("admin: command", "user", se.username, "cmd", spec.Name, "addr", se.remote)
		out := se.run(spec, args, line)
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		se.write(out)
		return
	}
	se.write("Sorry, that command is unknown.\n\n")
}

func (se *session) run(spec Spec, args, line string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("{R}ERROR{/}: command %s crashed: %v", spec.Name, r)
		}
	}()
	return spec.Run(args, line)
}

// builtins are the commands every botje has: help and user management.
// The Perl eval is deliberately absent.
func (se *session) builtins() []Spec {
	return []Spec{
		{
			Name:  "help",
			Match: regexp.MustCompile(`^(help|\?)$`),
			Help:  "Print this help text",
			Run:   func(_, _ string) string { return se.helpText() },
		},
		{
			Name:  "adduser",
			Match: regexp.MustCompile(`^(?i)adduser\s+(\S+)\s+(\S+)$`),
			Help:  "Add user to botje",
			Args:  []string{"<newuser>", "<newpass>"},
			Su:    true,
			Run: func(_, line string) string {
				g := regexp.MustCompile(`^(?i)adduser\s+(\S+)\s+(\S+)$`).FindStringSubmatch(line)
				if err := se.srv.Auth.AddUser(g[1], g[2]); err != nil {
					return fmt.Sprintf("{r}Error:{/} %v", err)
				}
				return fmt.Sprintf("{g}User %s added.{/}", g[1])
			},
		},
		{
			Name:  "passwd",
			Match: regexp.MustCompile(`^(?i)passwd\s+(\S+)\s+(.+)$`),
			Help:  "Password change for specified user",
			Args:  []string{"<user>", "<new pass>"},
			Su:    true,
			Run: func(_, line string) string {
				g := regexp.MustCompile(`^(?i)passwd\s+(\S+)\s+(.+)$`).FindStringSubmatch(line)
				if err := se.srv.Auth.SetPassword(g[1], g[2]); err != nil {
					return fmt.Sprintf("{r}Error:{/} %v", err)
				}
				return "{g}Password changed.{/}"
			},
		},
		{
			Name:  "users",
			Match: regexp.MustCompile(`^users$`),
			Help:  "List known users",
			Su:    true,
			Run: func(_, _ string) string {
				return strings.Join(se.srv.Auth.Users(), "\n")
			},
		},
	}
}

func (se *session) helpText() string {
	var b strings.Builder
	b.WriteString("Available commands:\n")
	for _, spec := range se.specs() {
		if spec.Su && !se.su {
			continue
		}
		name := spec.Name
		if len(spec.Args) > 0 {
			name += " " + strings.Join(spec.Args, " ")
		}
		fmt.Fprintf(&b, "  {y}%-40s{/} %s\n", name, spec.Help)
	}
	return b.String()
}
