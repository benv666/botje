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

	"go-botje/internal/auth"
	"go-botje/internal/core"
	"go-botje/internal/module"
	"go-botje/internal/storage"
	"go-botje/modules/ego"
	"go-botje/modules/karma"
	"go-botje/modules/lastseen"
	"go-botje/modules/pacman"
	"go-botje/modules/tinyurl"
)

const usage = `usage: botje <keeper|core|standalone> [flags]`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "standalone":
		os.Exit(standalone(os.Args[2:]))
	case "keeper", "core":
		fmt.Fprintf(os.Stderr, "botje %s: not implemented yet, use standalone\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
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
	// superuser bootstrap from env: BOTJE_SUPERUSER=name:bcrypt-hash
	if su := os.Getenv("BOTJE_SUPERUSER"); su != "" {
		if name, hash, ok := strings.Cut(su, ":"); ok {
			a.SetSuperuser(name, hash)
		}
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
		pacman.New(),
		tinyurl.New(),
	}
}
