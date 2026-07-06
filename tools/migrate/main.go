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
	"go-botje/modules/ego"
	"go-botje/modules/rss"
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

	// puts maps storage key -> value within the module's namespace
	puts := make(map[string]any)
	var report string
	switch mod {
	case "karma":
		data, stats, err := karmaFromPerl(dump)
		if err != nil {
			return err
		}
		puts["karma"] = data
		report = fmt.Sprintf("karma: %d global items, %d servers, %d channels, %d channel items",
			stats.GlobalItems, stats.Servers, stats.Channels, stats.ChannelItems)
	case "markov":
		dicts, stats, err := markovFromPerl(dump)
		if err != nil {
			return err
		}
		for key, chains := range dicts {
			puts[key] = chains
		}
		report = fmt.Sprintf("markov: dictionaries %v, %d top words, total count %d",
			stats.Dictionaries, stats.TopWords, stats.TotalCount)
	case "ego":
		data, n, err := ego.MigrateFromPerl(dump)
		if err != nil {
			return err
		}
		puts["ego"] = data
		report = fmt.Sprintf("ego: %d nicks migrated", n)
	case "rss":
		data, n, err := rss.MigrateFromPerl(dump)
		if err != nil {
			return err
		}
		puts["feeds"] = data
		report = fmt.Sprintf("rss: %d feeds migrated", n)
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
	for key, value := range puts {
		if err := store.Put(mod, key, value); err != nil {
			return err
		}
		fmt.Println("imported into", mod, "/", key)
	}
	return nil
}
