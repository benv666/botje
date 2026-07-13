// Package keeper is the connection-keeper process: it owns the TCP/TLS
// connection to IRC and relays raw bytes to and from the core over a
// unix socket, buffering inbound data while the core is away. This is
// what makes core upgrades reconnect-free: the core can restart while
// the IRC session stays up.
//
// The keeper is deliberately dumb. It does no parsing, no flood
// control (that lives in the core), no session logic. It is a
// buffering byte relay with one safety net: a bounded inbound buffer so
// a core that stays away does not grow memory without limit.
package keeper

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"go-botje/internal/irc"
)

// maxBuffer caps inbound bytes held while no core is connected.
const maxBuffer = 4 << 20 // 4 MiB

// maxOutBuffer caps outbound bytes held while IRC is away. Small on
// purpose: it exists to carry a re-attaching core's registration (and
// its first joins) across an IRC reconnect, not minutes of module
// chatter. On overflow the head is kept and the tail dropped.
const maxOutBuffer = 16 << 10 // 16 KiB

// defaultReadTimeout declares an IRC connection dead when nothing at
// all arrives for this long. Servers ping idle clients every couple of
// minutes, so a silent socket this long is gone (NAT drop, hard hang).
const defaultReadTimeout = 5 * time.Minute

// Config configures the keeper.
type Config struct {
	Addr   string // IRC host:port
	TLS    bool
	Socket string // unix socket path for the core
	// CertFile/KeyFile: TLS client certificate, presented so the ircd
	// can identify the bot by fingerprint (oper certfp autologin)
	CertFile, KeyFile string
	// ReadTimeout is the inbound-silence window after which the IRC
	// connection is considered dead; 0 means the 5 minute default.
	ReadTimeout time.Duration
	Dial        func() (net.Conn, error) // test hook; nil dials Addr

	sleep func(context.Context, time.Duration) bool // test hook; nil sleeps for real
}

// Run connects to IRC, serves the core socket, and relays until ctx is
// cancelled.
func (Config) unused() {}

type keeper struct {
	cfg     Config
	backoff irc.Backoff

	mu         sync.Mutex
	irc        net.Conn // current IRC connection, nil while reconnecting
	core       net.Conn // current core connection, nil while away
	buffer     []byte   // inbound held for an absent core
	outBuf     []byte   // outbound held while IRC is away
	outDropped bool     // outBuf overflowed; dropping until the next flush
	closed     bool     // shutting down; refuse fresh connections
}

// Run connects to IRC, serves the core socket, and relays until ctx is
// cancelled.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Dial == nil {
		tlsConf, err := irc.ClientTLS(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return fmt.Errorf("keeper: %w", err)
		}
		cfg.Dial = func() (net.Conn, error) {
			if cfg.TLS {
				return tls.Dial("tcp", cfg.Addr, tlsConf)
			}
			return net.Dial("tcp", cfg.Addr)
		}
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}
	if cfg.sleep == nil {
		cfg.sleep = sleep
	}
	os.Remove(cfg.Socket) // stale socket from a previous run
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return fmt.Errorf("keeper: listen %s: %w", cfg.Socket, err)
	}
	defer ln.Close()
	defer os.Remove(cfg.Socket)

	k := &keeper{cfg: cfg}
	go func() {
		<-ctx.Done()
		ln.Close()
		k.goodbye()    // the keeper owns the real QUIT
		k.closeConns() // unblock the blocking reads
	}()
	go k.serveCore(ctx, ln)
	k.ircLoop(ctx)
	return ctx.Err()
}

// goodbye sends a QUIT to IRC on keeper shutdown: this is the real
// stop, so leave the channel cleanly instead of a ping timeout. Best
// effort with a short flush window.
func (k *keeper) goodbye() {
	k.mu.Lock()
	conn := k.irc
	k.mu.Unlock()
	if conn == nil {
		return
	}
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	conn.Write([]byte("QUIT :keeper shutting down\r\n"))
	time.Sleep(300 * time.Millisecond)
}

// closeConns closes the live IRC and core connections so the pump
// goroutines' Read calls return; used on shutdown. It also marks the
// keeper closed: a dial that was in flight when shutdown hit would
// otherwise install a connection nobody is left to close (setIRC
// refuses it instead).
func (k *keeper) closeConns() {
	k.mu.Lock()
	k.closed = true
	irc, core := k.irc, k.core
	k.mu.Unlock()
	if irc != nil {
		irc.Close()
	}
	if core != nil {
		core.Close()
	}
}

// ircLoop keeps the IRC connection up, reconnecting with backoff, and
// pumps inbound bytes to the core or the buffer.
func (k *keeper) ircLoop(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := k.cfg.Dial()
		if err != nil {
			delay := k.backoff.Next(time.Now())
			slog.Warn("keeper: IRC connect failed, retrying", "err", err, "delay", delay)
			if !k.cfg.sleep(ctx, delay) {
				return
			}
			continue
		}
		slog.Info("keeper: connected to IRC", "addr", k.cfg.Addr)
		k.setIRC(conn)
		k.pumpIRC(conn) // returns when the IRC connection dies
		k.setIRC(nil)
		if ctx.Err() != nil {
			return
		}
		// The session died with the socket. Drop the core so it
		// reconnects and re-registers (NICK/USER belong to the core;
		// the keeper stays dumb), and back off before redialing: an
		// instant redial after every short-lived connection is what
		// connectban z-lined us for on 2026-07-13.
		k.dropSession()
		delay := k.backoff.Next(time.Now())
		slog.Warn("keeper: IRC connection lost, reconnecting", "delay", delay)
		if !k.cfg.sleep(ctx, delay) {
			return
		}
	}
}

// pumpIRC reads from the IRC connection and forwards to the core (or
// buffers). Returns on read error, including the silence watchdog: a
// connection with no inbound bytes for ReadTimeout is dead (servers
// ping idle clients, so something always arrives on a live one).
func (k *keeper) pumpIRC(conn net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		conn.SetReadDeadline(time.Now().Add(k.cfg.ReadTimeout))
		n, err := conn.Read(buf)
		if n > 0 {
			k.toCore(buf[:n])
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				slog.Warn("keeper: no traffic from IRC, declaring the connection dead", "timeout", k.cfg.ReadTimeout)
			}
			return
		}
	}
}

// dropSession cuts the core loose and discards buffered inbound after
// the IRC connection died: both belong to a session that no longer
// exists. The core's own reconnect logic re-attaches and re-registers.
func (k *keeper) dropSession() {
	k.mu.Lock()
	core := k.core
	k.buffer = nil
	k.mu.Unlock()
	if core != nil {
		core.Close() // detachCore does the bookkeeping
	}
}

// toCore writes inbound bytes to the current core, or buffers them.
func (k *keeper) toCore(b []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.core != nil {
		if _, err := k.core.Write(b); err != nil {
			k.core = nil // core went away mid-write; buffer the rest
			k.bufferLocked(b)
		}
		return
	}
	k.bufferLocked(b)
}

func (k *keeper) bufferLocked(b []byte) {
	if len(k.buffer)+len(b) > maxBuffer {
		// drop oldest to stay bounded; a core this far behind will get
		// a truncated but still-framed tail (partial first line dropped
		// by the core's line buffer)
		over := len(k.buffer) + len(b) - maxBuffer
		if over >= len(k.buffer) {
			k.buffer = k.buffer[:0]
		} else {
			k.buffer = k.buffer[over:]
		}
	}
	k.buffer = append(k.buffer, b...)
}

func (k *keeper) setIRC(conn net.Conn) {
	k.mu.Lock()
	if conn != nil && k.closed {
		k.mu.Unlock()
		conn.Close() // shutdown won the race; pumpIRC exits right away
		return
	}
	old := k.irc
	k.irc = conn
	if conn != nil && len(k.outBuf) > 0 {
		// the attached core registered while IRC was down: deliver
		conn.Write(k.outBuf)
	}
	k.outBuf = nil
	k.outDropped = false
	k.mu.Unlock()
	if old != nil {
		old.Close()
	}
}

// serveCore accepts core connections (one at a time) and pumps their
// outbound bytes to IRC. On accept it flushes the inbound buffer.
func (k *keeper) serveCore(ctx context.Context, ln net.Listener) {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		k.attachCore(conn)
		k.pumpCore(conn) // returns when this core disconnects
		k.detachCore(conn)
	}
}

func (k *keeper) attachCore(conn net.Conn) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.core != nil {
		k.core.Close() // only one core; newest wins
	}
	k.core = conn
	if len(k.buffer) > 0 {
		conn.Write(k.buffer)
		k.buffer = k.buffer[:0]
	}
	slog.Info("keeper: core attached")
}

func (k *keeper) detachCore(conn net.Conn) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.core == conn {
		k.core = nil
		// whatever this core had queued for IRC is from an abandoned
		// session; the next core starts its own registration
		k.outBuf = nil
		k.outDropped = false
		slog.Info("keeper: core detached")
	}
	conn.Close()
}

// pumpCore reads outbound bytes from the core and writes them to the
// current IRC connection. Returns on core read error.
func (k *keeper) pumpCore(conn net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			k.toIRC(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// toIRC writes outbound bytes to the current IRC connection, or holds
// them while it is down so a re-attaching core's registration survives
// the reconnect (setIRC flushes the held bytes).
func (k *keeper) toIRC(b []byte) {
	k.mu.Lock()
	if k.irc == nil {
		k.bufferOutLocked(b)
		k.mu.Unlock()
		return
	}
	conn := k.irc
	k.mu.Unlock()
	if _, err := conn.Write(b); err != nil {
		slog.Warn("keeper: IRC write failed", "err", err)
	}
}

func (k *keeper) bufferOutLocked(b []byte) {
	if k.outDropped {
		return
	}
	if len(k.outBuf)+len(b) > maxOutBuffer {
		// keep the head (registration and first joins): append what
		// fits, then cut back to a line boundary so the stream stays
		// framed, and drop everything after
		if room := maxOutBuffer - len(k.outBuf); room > 0 {
			k.outBuf = append(k.outBuf, b[:room]...)
		}
		if i := bytes.LastIndexByte(k.outBuf, '\n'); i >= 0 {
			k.outBuf = k.outBuf[:i+1]
		} else {
			k.outBuf = k.outBuf[:0]
		}
		k.outDropped = true
		slog.Warn("keeper: outbound buffer full while IRC is away, dropping")
		return
	}
	k.outBuf = append(k.outBuf, b...)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

var errClosed = errors.New("keeper: closed")
