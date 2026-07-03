# go-botje architecture

Goal: replace hoer (Perl botje) with a single Go binary in a minimal Docker image, functional parity or better for the 17 working active modules, keeping the modular spirit without adopting an external bot framework.

Status: PROPOSAL, not yet debated with BenV. Open questions at the bottom.

## Ground rules

- No external IRC/bot frameworks. IRC protocol layer is ours, stdlib + crypto/tls. Same reasoning as the Perl bot: full control over flood behavior, splitting, quirks.
- Single binary, modules compiled in. A module is a Go package implementing the Module interface, registered in a registry. Enable/disable per module at runtime (config + admin port), no dynamic loading.
- TDD throughout. The protocol/flood/colorize/pager layers are pure functions wherever possible, so they get table-driven unit tests. Integration tests run against irc.benv.junerules.com:6669 TLS, channel #testing, gated behind an env var.

## Concurrency model

The Perl bot is a single-threaded select loop; module code never races. We keep that property where it matters:

- One dispatcher goroutine owns all module event dispatch. Events (IRC lines, timers firing, fetch results, admin commands) arrive on one channel; the dispatcher calls module handlers synchronously, one at a time. Module state needs no locks.
- Blocking work (HTTP fetches, DNS, subprocess) runs in goroutines but results re-enter through the event channel. Modules never block the dispatcher; the fetch API is callback/event based like the Perl Fetcher.
- Each connection (IRC socket, admin client) gets reader/writer goroutines feeding the dispatcher.
- Panic isolation: dispatcher recovers per handler call. A panicking module gets disabled (parity with force-unload), logged loudly.

## Package layout

```
cmd/botje/            main, wiring, signal handling
internal/bus/         event types, dispatcher, module registry, per-handler panic recovery + call stats
internal/module/      Module interface: Info() (name, deps), Load(ctx), Unload(), optional Status(), ReloadSettings()
internal/irc/         connection state machine per network: parser, numerics, event structs,
                      reconnect backoff (3/60/180/300s), nick/channel tracking, netsplit heuristic
internal/irc/flood/   outbound queue: high prio + per-channel round-robin, 1 msg/s token bucket,
                      >80 byte lines count multiple, 510-byte truncate, 448-byte wrap
internal/format/      colorize {x} tags -> mIRC codes, sparkline, table formatter, !more pager
internal/storage/     namespace store, JSON file per namespace, dirty-flag + periodic flush +
                      flush on disable/shutdown, atomic write (tmp + rename)
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
- Storage: GetData/SaveData per (namespace, key) semantics, but with an honest flush policy instead of flush-on-unload-only.
- Fetcher options: method/post/redirects/timeout/sizeLimit/basic auth/streaming/SigV4.
- Admin port: same port 1924, same core commands where sensible. The Perl eval backdoor does NOT come along.

## Deliberate improvements (not bugs to port)

- RSS short-code allocator checks liveness before reuse (the !LI6 cats bug).
- Storage flush: dirty namespaces flushed every 60s and on shutdown/disable. kill -9 loses at most a minute.
- Secrets only via env/file, no hardcoded API keys (UrbanD and Ticker keys move to config).
- LLM modules unified: one module, pluggable backends (OpenAI, Bedrock/Claude, Ollama), shared history/queue logic, per-backend model allow-lists.
- Structured logging (slog), keep the colored human console format.
- ChatGPT module's debug POST to httpbin: dropped.

## Data migration

Karma (1.3 MB), Markov (29 MB), Ego, RSS feeds+subscriptions, Ticker, Kind, Lastseen, Pizza timers must survive. Plan: small Perl dumper (Storable -> JSON, runs once with the old perl image, based on mounts/data/test.pl) + Go importer into the new storage. Verify counts (karma items, markov keys) match before cutover.

## Testing strategy

- Unit: parser (RFC1459 lines, numerics, prefixes), flood queue timing (fake clock), wrap/truncate (UTF-8 edge cases, grapheme boundaries), colorize, pager, storage atomicity, scheduler, each module's command handling with a scripted fake bus.
- Golden transcripts: recorded IRC exchanges as testdata, replayed through the full stack.
- Integration (env-gated, BOTJE_LIVE_TEST=1): connect to irc.benv.junerules.com:6669 TLS, join #testing, exercise join/msg/flood/reconnect against the real ircd. Never in #rss or other live channels. Test nick, not hoer.
- Module parity checks: for RSS/Karma/Ticker, feed the same inputs to Perl and Go and diff outputs (where practical).

## Open questions for BenV

1. Hot reload: Perl reloads modules live via telnet. Go cannot swap compiled code. Proposal: runtime enable/disable + state flush + fast restart under docker (IRC reconnect visible for a few seconds). Alternative is hashicorp go-plugin RPC subprocesses, which buys live swap at the cost of complexity. Take the restart?
2. Storage format: JSON file per namespace (greppable, diffable, matches current model) vs bbolt/sqlite. Proposal: JSON. Markov 29 MB as JSON is fine but could get its own binary/gob format if load time annoys.
3. Admin surface: keep raw telnet on 1924 for muscle memory, or same protocol + optional TLS? Proposal: plain telnet on loopback like now.
4. sdcv/aspell (external binaries): keep shelling out (needs them in the image), pure-Go replacements, or drop? BenV flagged mspell-class stuff as a non-distraction: decide late, stub for now.
5. Nick for the test bot: French translation of hoer. Candidates: putain (the classic), pute (literal), cocotte (politer, double meaning with the cooking pot). Default proposal: putain.
6. Module priority order for parity: proposal below in roadmap, confirm.
7. Google module scrapes google.nl HTML and breaks whenever Google sneezes. Port as-is, switch to an API, or drop?
8. IRC_Kind, Pizza, Remind, WorkHours: Kind and Pizza are active, port. Remind/WorkHours are inactive: port anyway or leave?
