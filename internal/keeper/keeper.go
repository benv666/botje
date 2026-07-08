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

// Config configures the keeper.
type Config struct {
	Addr   string // IRC host:port
	TLS    bool
	Socket string // unix socket path for the core
	// CertFile/KeyFile: TLS client certificate, presented so the ircd
	// can identify the bot by fingerprint (oper certfp autologin)
	CertFile, KeyFile string
	Dial              func() (net.Conn, error) // test hook; nil dials Addr
}

// Run connects to IRC, serves the core socket, and relays until ctx is
// cancelled.
func (Config) unused() {}

type keeper struct {
	cfg     Config
	backoff irc.Backoff

	mu     sync.Mutex
	irc    net.Conn // current IRC connection, nil while reconnecting
	core   net.Conn // current core connection, nil while away
	buffer []byte   // inbound held for an absent core
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
// goroutines' Read calls return; used on shutdown.
func (k *keeper) closeConns() {
	k.mu.Lock()
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
			if !sleep(ctx, delay) {
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
		slog.Warn("keeper: IRC connection lost, reconnecting")
	}
}

// pumpIRC reads from the IRC connection and forwards to the core (or
// buffers). Returns on read error.
func (k *keeper) pumpIRC(conn net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			k.toCore(buf[:n])
		}
		if err != nil {
			return
		}
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
	old := k.irc
	k.irc = conn
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
			k.mu.Lock()
			ircConn := k.irc
			k.mu.Unlock()
			if ircConn != nil {
				if _, werr := ircConn.Write(buf[:n]); werr != nil {
					slog.Warn("keeper: IRC write failed", "err", werr)
				}
			}
			// no IRC connection: drop (core will resync on reconnect)
		}
		if err != nil {
			return
		}
	}
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
