# go-botje architecture

Goal: replace hoer (Perl botje) with a single Go binary in a minimal Docker image, functional parity or better for the 17 working active modules, keeping the modular spirit without adopting an external bot framework.

Status: PROPOSAL, not yet debated with BenV. Open questions at the bottom.

## Ground rules

- No external IRC/bot frameworks. IRC protocol layer is ours, stdlib + crypto/tls. Same reasoning as the Perl bot: full control over flood behavior, splitting, quirks.
- Single binary, modules compiled in. A module is a Go package implementing the Module interface, registered in a registry. Enable/disable per module at runtime (config + admin port), no dynamic loading.
- TDD throughout. The protocol/flood/colorize/pager layers are pure functions wherever possible, so they get table-driven unit tests. Integration tests run against the junerules ircd (BOTJE_IRC_ADDR) TLS, channel #testing, gated behind an env var.

## Process model: connection keeper + core (DECIDED 2026-07-03)

BenV wants a reconnect-free upgrade path; plain restart-per-upgrade is acceptable only as fallback. So the bot splits in two cooperating processes from the same binary:

- keeper (`botje keeper`): owns the TCP/TLS connections to IRC servers, nothing else. Speaks a simple framed protocol (IRC lines + control messages: connect/join/status) over a unix socket to the core. Buffers inbound and queues outbound while the core is away. Small, boring, changes almost never.
- core (`botje core`): dispatcher, modules, storage, admin port. Restarts freely for upgrades and bugfixes; the IRC session never drops, nick stays online, no join/part noise.
- Single-process mode (`botje standalone`) for dev and tests: core connects directly.
- Compose runs keeper + core + postgres; unix socket on a shared volume. Keeper upgrades are the rare visible reconnect.
- Flood control lives in the core (it owns message semantics); keeper just writes what it gets, with a dumb hard rate cap as a safety net.

## Concurrency model

The Perl bot is a single-threaded select loop; module code never races. We keep that property where it matters:

- One dispatcher goroutine owns all module event dispatch. Events (IRC lines, timers firing, fetch results, admin commands) arrive on one channel; the dispatcher calls module handlers synchronously, one at a time. Module state needs no locks.
- Blocking work (HTTP fetches, DNS, subprocess) runs in goroutines but results re-enter through the event channel. Modules never block the dispatcher; the fetch API is callback/event based like the Perl Fetcher.
- Each connection (IRC socket, admin client) gets reader/writer goroutines feeding the dispatcher.
- Panic isolation: dispatcher recovers per handler call. A panicking module gets disabled (parity with force-unload), logged loudly.

## Package layout

```
cmd/botje/            main; subcommands: keeper, core, standalone
internal/keeper/      connection-keeper process + framed unix-socket protocol (client side in core)
internal/bus/         event types, dispatcher, module registry, per-handler panic recovery + call stats
internal/module/      Module interface: Info() (name, deps), Load(ctx), Unload(), optional Status(), ReloadSettings()
internal/irc/         connection state machine per network: parser, numerics, event structs,
                      reconnect backoff (3/60/180/300s), nick/channel tracking, netsplit heuristic
internal/irc/flood/   outbound queue: high prio + per-channel round-robin, 1 msg/s token bucket,
                      >80 byte lines count multiple, 510-byte truncate, 448-byte wrap
internal/format/      colorize {x} tags -> mIRC codes, sparkline, table formatter, !more pager
internal/storage/     Storage interface; postgres backend (prod), in-memory backend (tests).
                      Generic kv table (namespace, name, value jsonb) mirroring Perl semantics;
                      modules may own real tables (markov) behind the same interface
internal/conf/        typed settings (int/float/string/bool), defaults, config_changed events
internal/fetch/       HTTP client wrapper: timeouts, size limits, redirect cap 8, basic auth,
                      single-flight per URL, streaming, SigV4 (for Bedrock)
internal/admin/       control port 1924: auth, module enable/disable/status, conf get/set, irc admin
internal/sched/       one-shot timer heap feeding the bus (or thin wrapper over time.AfterFunc into the bus)
modules/rss/          ... one package per feature module
modules/karma/
modules/markov/
...
tools/migrate/        Storable .dat -> JSON migration (Perl dump script + Go importer)
```

## Contracts carried over from Perl (see docs/perl-reference/)

- Event shape: keep the Perl event hash as a Go struct (Sender{Nick,User,Host}, Channel, TargetMe, SenderMe, Query, Msg, raw). Modules written against it port 1:1.
- Command system: registerCommand(word) with multi-registration, default handlers with (priority, continue), Levenshtein did-you-mean on unknown commands.
- cmd_eventmsg pager: max N lines (default 4), (+N) suffix, !more with 600s expiry.
- Colorize tags: identical {x} syntax so module output code ports verbatim.
- Storage: GetData/SaveData per (namespace, key) semantics against Postgres, so persistence is immediate and honest instead of flush-on-unload-only.
- Fetcher options: method/post/redirects/timeout/sizeLimit/basic auth/streaming/SigV4.
- Admin port: same port 1924, same core commands where sensible. The Perl eval backdoor does NOT come along.

## Deliberate improvements (not bugs to port)

- RSS short-code allocator checks liveness before reuse (the !LI6 cats bug).
- Storage: Postgres sidecar (DECIDED), writes land immediately, kill -9 loses nothing committed. Embedded migrations run at core boot; compose makes setup automagic.
- Secrets only via env/file, no hardcoded API keys (UrbanD and Ticker keys move to config).
- LLM modules unified: one module, pluggable backends (OpenAI, Bedrock/Claude, Ollama), shared history/queue logic, per-backend model allow-lists.
- Structured logging (slog), keep the colored human console format.
- ChatGPT module's debug POST to httpbin: dropped.

## Storage details (DECIDED: Postgres)

- Sidecar postgres in docker compose (alpine image, volume, healthcheck), core waits for it and auto-migrates (embedded migrations).
- Base table: kv(namespace text, name text, value jsonb, updated_at) primary key (namespace, name). This is literally the unfinished Storage_MySQL design from 2007, delivered.
- Greppability answered by psql + a `botje kv dump [ns]` subcommand.
- Unit tests run against the in-memory backend; integration tests against a throwaway pg (compose).

## Data migration

Karma (1.3 MB), Markov (29 MB), Ego, RSS feeds+subscriptions, Ticker, Kind, Lastseen, Pizza timers must survive. Plan: small Perl dumper (Storable -> JSON, runs once with the old perl image, based on mounts/data/test.pl) + Go importer into Postgres. Verify counts (karma items, markov keys) match before cutover. Bonus: no more Storable-version upgrade breakage.

## Testing strategy

- Unit: parser (RFC1459 lines, numerics, prefixes), flood queue timing (fake clock), wrap/truncate (UTF-8 edge cases, grapheme boundaries), colorize, pager, storage atomicity, scheduler, each module's command handling with a scripted fake bus.
- Golden transcripts: recorded IRC exchanges as testdata, replayed through the full stack.
- Integration (env-gated, BOTJE_LIVE_TEST=1): connect to the junerules ircd (BOTJE_IRC_ADDR) TLS, join #testing, exercise join/msg/flood/reconnect against the real ircd. Never in #rss or other live channels. Test nick, not hoer.
- Module parity checks: for RSS/Karma/Ticker, feed the same inputs to Perl and Go and diff outputs (where practical).

## Decisions log

- 2026-07-03: keeper/core process split for reconnect-free core upgrades (BenV: disconnects are the crux).
- 2026-07-03: Postgres storage, compose sidecar, automagic setup (BenV preference, finishes the 2007 SQL intent).
- 2026-07-03: admin stays plain telnet on loopback 1924, no eval backdoor.
- 2026-07-03: test/parallel-run nick is Meretrix (Latin; BenV's pick over putain/pute/cocotte).
- 2026-07-03: google module reborn as a generic search module: free + low-hoops backend. Plan: self-hosted SearXNG (JSON API, no keys, fits the Uil docker fleet), DDG lite as fallback. No google.nl scraping.
- 2026-07-03: sdcv/aspell postponed, stub the commands, decide near the end.
- 2026-07-03: Remind gets ported (was inactive), WorkHours dropped.
- 2026-07-03: auth: no md5 migration, BenV resets the admin password at cutover. Passwords hashed properly (bcrypt or similar) in Go.

## Open questions

None. All decisions locked 2026-07-03; see the decisions log above. Module parity order in docs/roadmap.md is the working default and can be reshuffled anytime.
