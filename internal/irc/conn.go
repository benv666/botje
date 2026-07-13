package irc

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"go-botje/internal/format"
	"go-botje/internal/irc/flood"
)

// ConnConfig configures one connection attempt.
type ConnConfig struct {
	Network      string
	Addr         string                   // host:port
	TLS          bool                     // TLS with SNI, like the Perl SSL path
	OnLine       func(line string)        // complete inbound lines, reader goroutine
	OnDisconnect func(err error)          // fired once when the read side dies
	Dial         func() (net.Conn, error) // test override; nil dials Addr
	// CertFile/KeyFile: optional TLS client certificate (oper certfp);
	// in the keeper/core split the KEEPER presents it, this is for
	// standalone mode
	CertFile, KeyFile string
}

// Conn is one live IRC transport: framing in, flood-queued writes out
// (colorized, truncated to 510 bytes, CRLF-terminated, like the Perl
// writeServer). Single-use: when it dies the owner makes a new one
// (reconnect policy lives with the owner, see Backoff).
type Conn struct {
	cfg  ConnConfig
	sock net.Conn

	mu    sync.Mutex
	queue *flood.Queue
	wake  chan struct{}
	done  chan struct{}
	once  sync.Once
}

// Connect dials and starts the reader/writer goroutines.
func Connect(cfg ConnConfig) (*Conn, error) {
	dial := cfg.Dial
	if dial == nil {
		tlsConf, err := ClientTLS(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		dial = func() (net.Conn, error) {
			if cfg.TLS {
				return tls.Dial("tcp", cfg.Addr, tlsConf)
			}
			return net.Dial("tcp", cfg.Addr)
		}
	}
	sock, err := dial()
	if err != nil {
		return nil, fmt.Errorf("irc: connect %s: %w", cfg.Addr, err)
	}
	c := &Conn{
		cfg:   cfg,
		sock:  sock,
		queue: flood.New(time.Now),
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	go c.readLoop()
	go c.writeLoop()
	return c, nil
}

// Write queues a normal-priority line. The line is colorized ({x} tags
// to mIRC codes), truncated grapheme-safe to 510 bytes, and CRLF
// terminated before it hits the wire.
func (c *Conn) Write(line string) {
	c.mu.Lock()
	c.queue.Push(prepare(line))
	c.mu.Unlock()
	c.kick()
}

// WriteHigh queues a high-priority line (PONG).
func (c *Conn) WriteHigh(line string) {
	c.mu.Lock()
	c.queue.PushHigh(prepare(line))
	c.mu.Unlock()
	c.kick()
}

// QueueDepths reports the flood queue depth per bucket (channel or the
// generic bucket). Safe from any goroutine; metrics food.
func (c *Conn) QueueDepths() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queue.Depths()
}

// prepare is the Perl writeServer transform, minus CRLF (added at wire
// time).
func prepare(line string) string {
	return format.TruncateIRC(format.ToIRC(line), 510)
}

func (c *Conn) kick() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Close tears the connection down.
func (c *Conn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.sock.Close()
}

func (c *Conn) readLoop() {
	var lb LineBuffer
	buf := make([]byte, 4096)
	for {
		n, err := c.sock.Read(buf)
		if n > 0 {
			for _, line := range lb.Feed(buf[:n]) {
				c.cfg.OnLine(line)
			}
		}
		if err != nil {
			select {
			case <-c.done: // deliberate Close, no callback
			default:
				c.once.Do(func() { close(c.done) })
				c.cfg.OnDisconnect(err)
			}
			return
		}
	}
}

func (c *Conn) writeLoop() {
	for {
		c.mu.Lock()
		line, wait, ok := c.queue.Next()
		c.mu.Unlock()

		switch {
		case ok:
			if _, err := c.sock.Write([]byte(line + "\r\n")); err != nil {
				return // read side reports the disconnect
			}
		case wait > 0:
			select {
			case <-time.After(wait):
			case <-c.done:
				return
			}
		default:
			select {
			case <-c.wake:
			case <-c.done:
				return
			}
		}
	}
}
