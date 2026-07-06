# go-botje

Ground-up Go rewrite of botje, the Perl IRC bot from ~2007 that runs as
nick **hoer** on irc.benv.junerules.com. Goal: single Go binary,
functional parity or better. Design and progress live in
`docs/architecture.md` and `docs/roadmap.md`; per-session status in
`CLAUDE.md`.

## Build and test

```
make build      # bin/botje
make test       # unit tests; postgres conformance spins a throwaway docker container
make vet
make cover
make livetest   # gated integration tests against junerules #testing (BOTJE_LIVE_TEST=1)
make run        # run the bot against #testing as Meretrix
```

Testing happens on **#testing** as **Meretrix**, never on live channels,
never as hoer.

## Running

```
make run
# or with flags:
go run ./cmd/botje standalone -addr irc.benv.junerules.com:6669 -tls \
    -nick Meretrix -channels "#testing" -admin 127.0.0.1:1924
```

Environment:

| var | meaning |
|-----|---------|
| `BOTJE_PG_DSN` | postgres storage (`postgres://user:pass@host:port/db`); unset = in-memory, gone at exit |
| `BOTJE_SUPERUSER` | admin superuser bootstrap, `name:password` (plaintext, dev) or `name:bcrypt-hash` |
| `BOTJE_LIVE_TEST` | `1` enables the live integration tests |
| `BOTJE_PG_TEST_DSN` | reuse an existing postgres for the storage conformance tests instead of docker |

Storage schema is created automatically at boot (embedded migrations).

## Telnet admin port

Plain telnet on `127.0.0.1:1924` (`-admin`, empty disables). There is
**no default password**; put a user in first:

```
# quick dev run (in-memory storage):
BOTJE_SUPERUSER=benv:somepass make run

# persistent (postgres):
BOTJE_PG_DSN=... ./bin/botje adduser benv somepass   # insert or update
./bin/botje hash somepass                            # bcrypt hash for BOTJE_SUPERUSER
```

Then `telnet 127.0.0.1 1924`, log in, `help` lists commands. Builtins:
`help`, `conf [setting[=value]]`, `status`, `callstats`, `adduser`,
`passwd`, `users`, `quit`. Superuser-only commands stay hidden from
regular users. Three failed logins and it hangs up on you
("H-h-h-h-HACKER!!!"). The Perl eval backdoor does not exist here.

Note: the `login:` prompt has no trailing newline; line-buffered
viewers (nc in a pipe) show nothing until you type.

## IRC commands (modules so far)

| module | commands |
|--------|----------|
| karma | `!<item>++` / `!<item>--` (optional `# reason`), `!<item>?`, `!wku <item>`, `!wkd <item>` |
| ego | `!ego <nick>`, auto-report every 200 self-references |
| lastseen | `!last <nick>` |
| markov | `!talk [seed]`, any unknown `!command`, `talk` in query; learns all channel chatter; idle talker via `markov_idle_talk*` settings |
| pacman | messages starting with 2+ dots (exactly 2: 70% ignored) |
| pizza | `!pizza`/`!timer <timespec> [r{...}] [msg]`, `when <id>`, `clear [id]`, `start`/`stop` stopwatch; timespec: `18:30`, `24-12-2026`, `+2h30m`, weekdays, repeats `r{1d}` (min 30 min) |
| rss | `!rss add <url> [refresh=N] [#chan\|query] [tag=X] [grep="re"]`, `del`, `list [url]`, `last <key>`, `refresh <key>`, `!rss <id>` / bare `!<id>` recalls (repeat to page) |
| ticker | `!ticker add\|del\|show SYM [height]`, `setalarm SYM high= low= up= down=`, `delalarm`, `refresh`, `list`, bare `!SYM [height]`; BTC/ETH builtin, stocks need `conf ticker_alphavantage_key=...` |
| tinyurl | `!tinyurl <url>`, auto-shortens channel URLs over 40 chars |
| wiki | `!wiki <query>`, top 3 Wikipedia hits (15s per-channel spam brake) |
| urband | `!ud <term>` via RapidAPI Urban Dictionary; needs `conf urband_rapidapi_key=...` |
| wolframalpha | `!wa <query>`; needs `conf wolframalpha_appid=...` |
| core | `!more [command]` pages long replies |

Unknown `!commands` get a Levenshtein did-you-mean.

## Data migration (Perl Storable -> go-botje)

```
perl tools/migrate/dump.pl IRC_Karma.dat > karma.json
go run ./tools/migrate -module karma -in karma.json            # dry run, prints counts
go run ./tools/migrate -module karma -in karma.json -dsn ...   # import into postgres
```

`dump.pl` needs only core Perl (Storable, JSON::PP). Transformers exist
per module (karma so far); each new module port adds one. The karma path
is verified against the live 1.3 MB IRC_Karma.dat snapshot.

## Development notes

- TDD, no exceptions. Behavioral spec is `docs/perl-reference/` plus the
  actual Perl source in `reference/` (gitignored, contains live data and
  secrets, never commit).
- `internal/format` is golden-tested against the real Perl code:
  `internal/format/testdata/golden-gen.pl` regenerates `golden.json`
  (needs the `reference/` tree).
- Perl bugs found during porting are fixed, not ported, and documented
  in the code near the fix. Known list: `docs/perl-reference/core-framework.md`
  plus doc comments in the packages.
- Dependencies are deliberately few: pgx (postgres), rivo/uniseg
  (grapheme-safe truncation), x/crypto (bcrypt). The IRC layer is stdlib.
