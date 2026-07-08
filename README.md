# go-botje

Ground-up Go rewrite of botje, the Perl IRC bot from ~2007 that runs as
nick **hoer** on the Junerules irc server. Goal: single Go binary,
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
go run ./cmd/botje standalone -addr irc.example.com:6669 -tls \
    -nick Meretrix -channels "#testing" -admin 127.0.0.1:1924
```

Two process models:

- **standalone** (above): one process, connects to IRC directly. Simplest; a
  restart reconnects to IRC.
- **keeper + core**: `botje keeper` owns the IRC connection and relays it to
  `botje core` over a unix socket. The core (dispatcher + modules) can restart
  without dropping the IRC session, so module/bugfix upgrades are
  reconnect-free. This is what the compose stack runs.

All flags read their default from a `BOTJE_*` environment variable
(flags win). Site specifics live in the gitignored `.env`; `make run`
sources it.

| var | meaning |
|-----|---------|
| `BOTJE_IRC_ADDR` | IRC server `host:port`. Required, no built-in default |
| `BOTJE_IRC_TLS` | `false`/`no`/`0` disables TLS (default on) |
| `BOTJE_NETWORK` | network name (default `junerules`) |
| `BOTJE_NICK` | bot nick (default `Meretrix`) |
| `BOTJE_CHANNELS` | comma-separated channels (default `#testing`); seeds the autojoin set on the FIRST boot only, after that storage wins (manage via telnet `join`/`part` or /invite) |
| `BOTJE_ADMIN` | telnet admin address (default `127.0.0.1:1924`), empty disables |
| `BOTJE_SOCKET` | keeper unix socket (default `/run/keeper/keeper.sock`) |
| `BOTJE_LOG_DIR` | file logging root: per-channel IRC logs + ops.log audit trail; empty disables |
| `BOTJE_PG_DSN` | postgres storage (`postgres://user:pass@host:port/db`); unset = in-memory, gone at exit |
| `BOTJE_SUPERUSER` | admin superuser bootstrap, `name:password` (plaintext, dev) or `name:bcrypt-hash` |
| `BOTJE_LIVE_TEST` | `1` enables the live integration tests |
| `BOTJE_PG_TEST_DSN` | reuse an existing postgres for the storage conformance tests instead of docker |

Storage schema is created automatically at boot (embedded migrations).

## Docker deployment

`docker-compose.yml` runs keeper + core + postgres; it is static, all
site config comes from `.env` (copy `.env.example`). First run:

```
cp .env.example .env        # fill in BOTJE_IRC_ADDR, POSTGRES_PASSWORD, ...
make keeper-image           # builds go-botje:keeper-<date>; set the tag in .env
docker compose up -d
```

The keeper runs a pinned image tag (`BOTJE_KEEPER_IMAGE`) with no build
section, so `docker compose build && docker compose up -d` upgrades the
core reconnect-free and never recreates the keeper. To upgrade the
keeper itself (rare, drops the IRC session for one reconnect): `make
keeper-image`, bump the tag in `.env`, `docker compose up -d`.

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
`help`, `conf [setting[=value]]`, `join <chan>`, `part <chan>`,
`status`, `callstats`, `adduser`, `passwd`, `users`, `quit`.
Superuser-only commands stay hidden from regular users. Three failed
logins and it hangs up on you ("H-h-h-h-HACKER!!!"). The Perl eval
backdoor does not exist here.

Channels and `conf` changes persist in storage: `join`/`part` manage
the autojoin set at runtime, a `/invite` makes the bot join and adds
the channel to the set, and settings changed via `conf x=y` survive
restarts. `BOTJE_CHANNELS` only seeds the set on the very first boot
(with in-memory storage every boot is a first boot).

Note: the `login:` prompt has no trailing newline; line-buffered
viewers (nc in a pipe) show nothing until you type.

## Logging

With `BOTJE_LOG_DIR` set (the compose stack mounts `./mounts/logs` and
sets it automatically), the bot writes:

- `<network>/<#channel>/YYYY-MM-DD.log`: plain-text channel logs
  (messages incl. the bot's own, joins/parts/kicks/modes/topics),
  daily files, colors stripped
- `<network>/queries/<nick>/`: private messages
- `<network>/server/`: quits and other channel-less events
- `ops.log`: audit trail - telnet connections, login success/failure
  with source address, executed admin commands (by name, never
  arguments), conf changes, reconnects, module errors

The host dir must be writable by the container user:
`mkdir -p mounts/logs && sudo chown 1000 mounts/logs`. Channel logging
lives in `modules/logger` (disable at runtime with
`conf logger_dir=`); the ops log is core, always on when the dir is set.

## Writing a module

Start at `modules/example`: a working, heavily commented skeleton that
exercises the whole module API (commands, default handlers, bus hooks,
conf, storage, timers, async fetch, pager, telnet commands). It is not
autoloaded; add it to `modules()` in cmd/botje/main.go to play with it
live.

## IRC commands (modules so far)

| module | commands |
|--------|----------|
| karma | `!<item>++` / `!<item>--` (optional `# reason`), `!<item>?`, `!wku <item>`, `!wkd <item>` |
| ego | `!ego <nick>`, auto-report every 200 self-references |
| guard | spam gatekeeper, stage 1: telnet `guard on\|off\|status`; learns resident user@hosts while off, freezes the set and logs non-resident joins while on. Enforcement is the next stage |
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
| remind | `!remind <min> <hour> <day> <mon> <dow> <msg>` (cron, day/month names ok), `!remind show\|clear <ids>`; `!remember <name> <val>` / `!recall <name> [n]` / `!forget <name>` notepad (last 3 per name) |
| llm | `!gpt <q>` (OpenAI), `!claude [model] <q>` (Bedrock), `!oi [model] <q>` (Ollama); per-channel history; keys in `conf llm_openai_key` / `llm_aws_key`+`llm_aws_secret` / `llm_ollama_url` |
| core | `!more [command]` pages long replies |

Unknown `!commands` get a Levenshtein did-you-mean.

## Data migration (Perl Storable -> go-botje)

```
perl tools/migrate/dump.pl IRC_Karma.dat > karma.json
go run ./tools/migrate -module karma -in karma.json            # dry run, prints counts
go run ./tools/migrate -module karma -in karma.json -dsn ...   # import into postgres
```

`dump.pl` needs only core Perl (Storable, JSON::PP). Transformers exist
for karma, markov, ego, rss and ticker, all verified against the live
.dat snapshots (karma 3889 items, markov 27614 words, ego 873 nicks,
rss 25 feeds, ticker 2 symbols + alarms). Remind/pizza/lastseen data is
trivially recreated at cutover instead.

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
  (grapheme-safe truncation), x/crypto (bcrypt), robfig/cron (remind
  schedule math). The IRC layer is stdlib.
