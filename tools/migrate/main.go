// Command migrate imports Perl botje Storable data into go-botje
// storage. Usage:
//
//	perl dump.pl IRC_Karma.dat > karma.json
//	go run ./tools/migrate -module karma -in karma.json [-dsn postgres://...]
//
// Without -dsn it dry-runs: transform + verify counts, write nothing.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"go-botje/internal/storage"
)

func main() {
	var (
		mod = flag.String("module", "", "module to migrate (karma)")
		in  = flag.String("in", "", "json dump from dump.pl")
		dsn = flag.String("dsn", "", "postgres dsn; empty for a dry run")
	)
	flag.Parse()
	if *mod == "" || *in == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*mod, *in, *dsn); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(mod, in, dsn string) error {
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var dump map[string]any
	if err := json.Unmarshal(raw, &dump); err != nil {
		return fmt.Errorf("parse %s: %w", in, err)
	}

	var value any
	var report string
	switch mod {
	case "karma":
		data, stats, err := karmaFromPerl(dump)
		if err != nil {
			return err
		}
		value = data
		report = fmt.Sprintf("karma: %d global items, %d servers, %d channels, %d channel items",
			stats.GlobalItems, stats.Servers, stats.Channels, stats.ChannelItems)
	default:
		return fmt.Errorf("no transformer for module %q yet", mod)
	}

	fmt.Println(report)
	if dsn == "" {
		fmt.Println("dry run, nothing written (pass -dsn to import)")
		return nil
	}
	store, err := storage.OpenPostgres(context.Background(), dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Put(mod, mod, value); err != nil {
		return err
	}
	fmt.Println("imported into", mod, "/", mod)
	return nil
}
