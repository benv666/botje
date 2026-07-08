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
	"go-botje/internal/module"
	"go-botje/internal/storage"
	"go-botje/internal/teelog"
	"go-botje/modules/ego"
	"go-botje/modules/karma"
	"go-botje/modules/lastseen"
	"go-botje/modules/llm"
	"go-botje/modules/logger"
	"go-botje/modules/markov"
	"go-botje/modules/pacman"
	"go-botje/modules/pizza"
	"go-botje/modules/remind"
	"go-botje/modules/rss"
	"go-botje/modules/ticker"
	"go-botje/modules/tinyurl"
	"go-botje/modules/urband"
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

// standalone runs a single-process bot (core connects to IRC directly).
// The server address has no built-in default: set -addr or
// BOTJE_IRC_ADDR. Nick/channel defaults are the safe test setup
// (Meretrix in #testing), never live channels.
func standalone(args []string) int {
	fs := flag.NewFlagSet("standalone", flag.ExitOnError)
	var (
		network  = fs.String("network", envOr("BOTJE_NETWORK", "junerules"), "irc network name")
		addr     = fs.String("addr", envOr("BOTJE_IRC_ADDR", ""), "server host:port (or BOTJE_IRC_ADDR)")
		useTLS   = fs.Bool("tls", envBool("BOTJE_IRC_TLS", true), "connect with TLS")
		nick     = fs.String("nick", envOr("BOTJE_NICK", "Meretrix"), "bot nick")
		channels = fs.String("channels", envOr("BOTJE_CHANNELS", "#testing"), "comma-separated channels to join")
		adminOn  = fs.String("admin", envOr("BOTJE_ADMIN", "127.0.0.1:1924"), "telnet admin address, empty to disable")
	)
	fs.Parse(args)
	if *addr == "" {
		fmt.Fprintln(os.Stderr, "no IRC server: set -addr or BOTJE_IRC_ADDR")
		return 2
	}
	return runCore(coreOpts{
		network: *network, addr: *addr, tls: *useTLS, nick: *nick,
		channels: *channels, admin: *adminOn,
	})
}

// coreMode runs just the core, connecting to a keeper's unix socket
// instead of dialing IRC. The keeper owns the IRC connection, so the
// core restarts freely without dropping the IRC session.
func coreMode(args []string) int {
	fs := flag.NewFlagSet("core", flag.ExitOnError)
	var (
		network  = fs.String("network", envOr("BOTJE_NETWORK", "junerules"), "irc network name")
		socket   = fs.String("socket", envOr("BOTJE_SOCKET", "/run/keeper/keeper.sock"), "keeper unix socket")
		nick     = fs.String("nick", envOr("BOTJE_NICK", "Meretrix"), "bot nick")
		channels = fs.String("channels", envOr("BOTJE_CHANNELS", "#testing"), "comma-separated channels to join")
		adminOn  = fs.String("admin", envOr("BOTJE_ADMIN", "127.0.0.1:1924"), "telnet admin address, empty to disable")
	)
	fs.Parse(args)
	return runCore(coreOpts{
		network: *network, socket: *socket, nick: *nick,
		channels: *channels, admin: *adminOn,
	})
}

// keeperMode runs just the connection keeper.
func keeperMode(args []string) int {
	fs := flag.NewFlagSet("keeper", flag.ExitOnError)
	var (
		addr   = fs.String("addr", envOr("BOTJE_IRC_ADDR", ""), "server host:port (or BOTJE_IRC_ADDR)")
		useTLS = fs.Bool("tls", envBool("BOTJE_IRC_TLS", true), "connect with TLS")
		socket = fs.String("socket", envOr("BOTJE_SOCKET", "/run/keeper/keeper.sock"), "unix socket for the core")
	)
	fs.Parse(args)
	if *addr == "" {
		fmt.Fprintln(os.Stderr, "no IRC server: set -addr or BOTJE_IRC_ADDR")
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := keeper.Run(ctx, keeper.Config{Addr: *addr, TLS: *useTLS, Socket: *socket}); err != nil {
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
		Nick:      o.nick,
		Channels:  strings.Split(o.channels, ","),
		Store:     store,
		Modules:   modules(),
		Auth:      a,
		AdminAddr: o.admin,
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
		karma.New(),
		lastseen.New(),
		llm.New(),
		logger.New(),
		markov.New(),
		pacman.New(),
		pizza.New(),
		remind.New(),
		rss.New(),
		ticker.New(),
		tinyurl.New(),
		urband.New(),
		wiki.New(),
		wolframalpha.New(),
	}
}
