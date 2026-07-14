// Command botje is the Go rewrite of BenV's Perl IRC bot.
//
// Subcommands:
//
//	keeper      owns the IRC TCP/TLS connections, survives core restarts
//	core        dispatcher, modules, storage, admin port
//	standalone  keeper+core in one process, for dev and tests
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/crypto/bcrypt"

	"go-botje/internal/auth"
	"go-botje/internal/core"
	"go-botje/internal/keeper"
	"go-botje/internal/metrics"
	"go-botje/internal/module"
	"go-botje/internal/storage"
	"go-botje/internal/teelog"
	"go-botje/modules/ego"
	"go-botje/modules/guard"
	"go-botje/modules/karma"
	"go-botje/modules/lastseen"
	"go-botje/modules/llm"
	"go-botje/modules/logger"
	"go-botje/modules/markov"
	"go-botje/modules/pacman"
	"go-botje/modules/pizza"
	"go-botje/modules/remind"
	"go-botje/modules/rss"
	"go-botje/modules/rvf"
	"go-botje/modules/stats"
	"go-botje/modules/ticker"
	"go-botje/modules/tinyurl"
	"go-botje/modules/urband"
	"go-botje/modules/weather"
	"go-botje/modules/wiki"
	"go-botje/modules/wolframalpha"
)

const usage = `usage: botje <standalone|keeper|core|adduser|hash> [flags]

  standalone            run the whole bot in one process (dev, tests)
  keeper                own the IRC connection, relay to core over a unix socket
  core                  run dispatcher+modules, connect via a keeper (-socket)
  adduser <user> <pass> insert or update an admin user in storage (needs BOTJE_PG_DSN)
  hash <password>       print a bcrypt hash, e.g. for BOTJE_SUPERUSER=name:<hash>`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "standalone":
		os.Exit(standalone(os.Args[2:]))
	case "adduser":
		os.Exit(adduser(os.Args[2:]))
	case "hash":
		os.Exit(hashCmd(os.Args[2:]))
	case "keeper":
		os.Exit(keeperMode(os.Args[2:]))
	case "core":
		os.Exit(coreMode(os.Args[2:]))
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

// hashCmd prints a bcrypt hash for BOTJE_SUPERUSER.
func hashCmd(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: botje hash <password>")
		return 2
	}
	h, err := bcrypt.GenerateFromPassword([]byte(args[0]), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(string(h))
	return 0
}

// adduser inserts or updates an admin user directly in storage: the
// bootstrap for the telnet port. Needs BOTJE_PG_DSN; the in-memory
// store dies with the process, so there is nothing to add users to.
func adduser(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: botje adduser <username> <password>")
		return 2
	}
	if os.Getenv("BOTJE_PG_DSN") == "" {
		fmt.Fprintln(os.Stderr, "adduser needs BOTJE_PG_DSN (in-memory storage has no users to manage);")
		fmt.Fprintln(os.Stderr, "for a quick dev run use BOTJE_SUPERUSER=name:password with standalone instead")
		return 2
	}
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer store.Close()
	a, err := auth.New(store)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	name, pass := args[0], args[1]
	if err := a.AddUser(name, pass); err != nil {
		if err := a.SetPassword(name, pass); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("password updated for %s\n", name)
		return 0
	}
	fmt.Printf("user %s added\n", name)
	return 0
}

// envOr reads an environment default for a flag: flags always win,
// .env/compose environment fills the gaps, code carries no
// installation-specific values.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	switch os.Getenv(key) {
	case "":
		return fallback
	case "0", "false", "no":
		return false
	}
	return true
}

// coreFlags registers the flags every core-running mode (standalone,
// core) shares and returns a collector that builds the coreOpts after
// Parse. Nick/channel defaults are the safe test setup (Meretrix in
// #testing), never live channels.
func coreFlags(fs *flag.FlagSet) func() coreOpts {
	var (
		network  = fs.String("network", envOr("BOTJE_NETWORK", "junerules"), "irc network name")
		nick     = fs.String("nick", envOr("BOTJE_NICK", "Meretrix"), "bot nick")
		channels = fs.String("channels", envOr("BOTJE_CHANNELS", "#testing"), "comma-separated channels to join")
		adminOn  = fs.String("admin", envOr("BOTJE_ADMIN", "127.0.0.1:1924"), "telnet admin address, empty to disable")
		metrics  = fs.String("metrics", envOr("BOTJE_METRICS", ""), "prometheus listen addr, e.g. 127.0.0.1:9095 (or BOTJE_METRICS)")
	)
	return func() coreOpts {
		return coreOpts{
			network: *network, nick: *nick, channels: *channels,
			admin: *adminOn, metrics: *metrics,
		}
	}
}

// ircConn is the dial side collected by ircFlags.
type ircConn struct {
	addr      string
	tls       bool
	cert, key string
}

// ircFlags registers the IRC dial flags shared by the modes that own
// the socket (standalone, keeper) and returns a collector.
func ircFlags(fs *flag.FlagSet) func() ircConn {
	var (
		addr   = fs.String("addr", envOr("BOTJE_IRC_ADDR", ""), "server host:port (or BOTJE_IRC_ADDR)")
		useTLS = fs.Bool("tls", envBool("BOTJE_IRC_TLS", true), "connect with TLS")
		cert   = fs.String("tls-cert", envOr("BOTJE_TLS_CERT", ""), "TLS client cert for oper certfp (or BOTJE_TLS_CERT)")
		key    = fs.String("tls-key", envOr("BOTJE_TLS_KEY", ""), "TLS client key (or BOTJE_TLS_KEY)")
	)
	return func() ircConn {
		return ircConn{addr: *addr, tls: *useTLS, cert: *cert, key: *key}
	}
}

// requireAddr enforces the no-in-code-server-default rule (the repo is
// public): an empty address refuses with a pointer to the env var.
func requireAddr(addr string) bool {
	if addr == "" {
		fmt.Fprintln(os.Stderr, "no IRC server: set -addr or BOTJE_IRC_ADDR")
		return false
	}
	return true
}

// standalone runs a single-process bot (core connects to IRC directly).
func standalone(args []string) int {
	fs := flag.NewFlagSet("standalone", flag.ExitOnError)
	getOpts, getIRC := coreFlags(fs), ircFlags(fs)
	fs.Parse(args)
	irc := getIRC()
	if !requireAddr(irc.addr) {
		return 2
	}
	o := getOpts()
	o.addr, o.tls, o.cert, o.key = irc.addr, irc.tls, irc.cert, irc.key
	return runCore(o)
}

// coreMode runs just the core, connecting to a keeper's unix socket
// instead of dialing IRC. The keeper owns the IRC connection, so the
// core restarts freely without dropping the IRC session.
func coreMode(args []string) int {
	fs := flag.NewFlagSet("core", flag.ExitOnError)
	getOpts := coreFlags(fs)
	socket := fs.String("socket", envOr("BOTJE_SOCKET", "/run/keeper/keeper.sock"), "keeper unix socket")
	fs.Parse(args)
	o := getOpts()
	o.socket = *socket
	return runCore(o)
}

// keeperMode runs just the connection keeper.
func keeperMode(args []string) int {
	fs := flag.NewFlagSet("keeper", flag.ExitOnError)
	getIRC := ircFlags(fs)
	socket := fs.String("socket", envOr("BOTJE_SOCKET", "/run/keeper/keeper.sock"), "unix socket for the core")
	fs.Parse(args)
	irc := getIRC()
	if !requireAddr(irc.addr) {
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := keeper.Run(ctx, keeper.Config{Addr: irc.addr, TLS: irc.tls, Socket: *socket,
		CertFile: irc.cert, KeyFile: irc.key}); err != nil {
		if ctx.Err() == nil {
			slog.Error("keeper", "err", err)
			return 1
		}
	}
	return 0
}

type coreOpts struct {
	network, addr, nick, channels, admin string
	tls                                  bool
	socket                               string // set = connect via keeper
	cert, key                            string // client cert, standalone only
	metrics                              string // prometheus listen addr, empty = off
}

func runCore(o coreOpts) int {
	handler := slog.Handler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// BOTJE_LOG_DIR: tee everything (admin audit, conf changes,
	// reconnects, module errors) into an ops.log next to the channel
	// logs, on top of stderr/docker logs
	if dir := os.Getenv("BOTJE_LOG_DIR"); dir != "" {
		h, closeOps, err := teelog.OpsLog(handler, dir)
		if err != nil {
			slog.Error("ops log unavailable, continuing on stderr only", "dir", dir, "err", err)
		} else {
			handler = h
			defer closeOps()
		}
	}
	slog.SetDefault(slog.New(handler))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := openStore(ctx)
	if err != nil {
		slog.Error("storage", "err", err)
		return 1
	}
	defer store.Close()

	a, err := auth.New(store)
	if err != nil {
		slog.Error("auth", "err", err)
		return 1
	}
	// superuser bootstrap: BOTJE_SUPERUSER=name:password (plaintext,
	// dev) or name:bcrypt-hash (from 'botje hash')
	if su := os.Getenv("BOTJE_SUPERUSER"); su != "" {
		name, hash, err := auth.ParseSuperuser(su)
		if err != nil {
			slog.Error("BOTJE_SUPERUSER", "err", err)
			return 1
		}
		a.SetSuperuser(name, hash)
		slog.Info("admin: superuser bootstrapped from env", "name", name)
	}

	cfg := core.Config{
		Network:   o.network,
		Addr:      o.addr,
		TLS:       o.tls,
		CertFile:  o.cert,
		KeyFile:   o.key,
		Nick:      o.nick,
		Channels:  strings.Split(o.channels, ","),
		Store:     store,
		Modules:   modules(),
		Auth:      a,
		AdminAddr: o.admin,
	}
	if o.metrics != "" {
		cfg.Metrics = metrics.New()
		cfg.MetricsAddr = o.metrics
	}
	if o.socket != "" {
		// connect to the keeper instead of IRC; the keeper frames and
		// relays raw bytes, so the transport is byte-identical. Suppress
		// the goodbye QUIT so a core restart leaves the session up.
		sock := o.socket
		cfg.Dial = func() (net.Conn, error) { return net.Dial("unix", sock) }
		cfg.SkipGoodbye = true
	}

	if err := core.Run(ctx, cfg); err != nil {
		slog.Error("core", "err", err)
		return 1
	}
	return 0
}

// openStore uses postgres when BOTJE_PG_DSN is set, else volatile
// in-memory storage (fine for #testing runs).
func openStore(ctx context.Context) (storage.Store, error) {
	if dsn := os.Getenv("BOTJE_PG_DSN"); dsn != "" {
		slog.Info("storage: postgres")
		return storage.OpenPostgres(ctx, dsn)
	}
	slog.Info("storage: in-memory (set BOTJE_PG_DSN for persistence)")
	return storage.NewMemory(), nil
}

// modules is the standalone autoload list, the Go modules.autoload.
func modules() []module.Module {
	return []module.Module{
		ego.New(),
		guard.New(),
		karma.New(),
		lastseen.New(),
		llm.New(),
		logger.New(),
		// rvf loads before markov on purpose: bus hooks run in
		// registration order, and rvf returns Stop for live-game input
		// so markov neither learns letter spam nor query-talks at a
		// player mid-game
		rvf.New(),
		markov.New(),
		pacman.New(),
		pizza.New(),
		remind.New(),
		rss.New(),
		stats.New(),
		ticker.New(),
		tinyurl.New(),
		urband.New(),
		weather.New(),
		wiki.New(),
		wolframalpha.New(),
	}
}
