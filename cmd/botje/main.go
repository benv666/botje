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
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/crypto/bcrypt"

	"go-botje/internal/auth"
	"go-botje/internal/core"
	"go-botje/internal/module"
	"go-botje/internal/storage"
	"go-botje/modules/ego"
	"go-botje/modules/karma"
	"go-botje/modules/lastseen"
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

const usage = `usage: botje <standalone|adduser|hash|keeper|core> [flags]

  standalone            run the bot (defaults: junerules #testing as Meretrix)
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
	case "keeper", "core":
		fmt.Fprintf(os.Stderr, "botje %s: not implemented yet, use standalone\n", os.Args[1])
		os.Exit(1)
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

// standalone runs a single-process bot. Defaults point at the junerules
// test setup: #testing as Meretrix, never the live channels.
func standalone(args []string) int {
	fs := flag.NewFlagSet("standalone", flag.ExitOnError)
	var (
		network  = fs.String("network", "junerules", "irc network name")
		addr     = fs.String("addr", "irc.benv.junerules.com:6669", "server host:port")
		useTLS   = fs.Bool("tls", true, "connect with TLS")
		nick     = fs.String("nick", "Meretrix", "bot nick")
		channels = fs.String("channels", "#testing", "comma-separated channels to join")
		adminOn  = fs.String("admin", "127.0.0.1:1924", "telnet admin address, empty to disable")
	)
	fs.Parse(args)

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))

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

	err = core.Run(ctx, core.Config{
		Network:   *network,
		Addr:      *addr,
		TLS:       *useTLS,
		Nick:      *nick,
		Channels:  strings.Split(*channels, ","),
		Store:     store,
		Modules:   modules(),
		Auth:      a,
		AdminAddr: *adminOn,
	})
	if err != nil {
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
