# go-botje roadmap

Phases are sequential, each lands with tests green. Within a phase, TDD per component.

## Phase 0: skeleton (this repo)
- [x] Investigation of Perl botje (docs/perl-reference/)
- [x] Architecture proposal (docs/architecture.md)
- [x] Architecture debated + decisions locked with BenV (2026-07-03, see architecture.md decisions log)
- [x] go.mod, cmd/botje stub, CI-less local test loop (make test)

## Phase 1: core plumbing
- [x] internal/sched: timer heap + bus feed (fake clock tests)
- [x] internal/bus: dispatcher, registry, panic isolation, call stats
- [x] internal/conf: typed settings + config_changed
- [x] internal/storage: Storage interface, in-memory backend, postgres backend + embedded migrations
- [x] internal/format: colorize, wrap/truncate (448/510, UTF-8), sparkline, tables

## Phase 2: IRC layer
- [x] internal/irc: line parser + numerics table (golden tests)
- [x] connection state machine, TLS, reconnect backoff, channel tracking
- [x] internal/irc/flood: token bucket + per-channel RR queue (fake clock)
- [x] command dispatch (!commands, default handlers, Levenshtein suggest)
- [x] pager (!more)
- [x] live smoke test against junerules #testing (env-gated)

## Phase 3: support systems
- [x] internal/fetch (single-flight, timeout, sizeLimit, redirects, basic auth, streaming)
- [x] internal/admin: port 1924, auth, conf/status/callstats/adduser/passwd commands (module load/unload + irc admin: when module manager lands)
- [x] auth: users + superuser, proper hashing, no md5 migration (BenV resets at cutover)

## Phase 4: modules, in parity order
Order by daily-use value, simplest first within tiers:
- [x] karma (small, heavily used, data migration proof case)
- [x] ego
- [x] lastseen
- [x] pacman (trivial, fun early win)
- [x] tinyurl
- [x] rss (the big one; fix the short-code collision)
- [x] pizza (timers)
- [x] markov (+ dictionary migration, 29 MB)
- [x] ticker
- dropped by BenV 2026-07-06: kind, search, nagios
- [x] wiki, urband, wolframalpha
- [x] remind (cron reminders + remember/recall, promoted from inactive)
- [x] llm (unified: openai + bedrock + ollama; sigv4 signer offline-verified)
- [ ] optional/inactive: floodkick, qrcode, urlpeek, nectarine, sdcv/spell (postponed by decision)
- dropped: workhours, todo (was never finished), ps3_proxy

## Phase 5: migration + cutover
- [x] tools/migrate: Storable -> JSON dump + import, verified counts. DONE for karma (3889 global items), markov (27614 words/29MB), ego (873 nicks), rss (25 feeds/2959 items) - all proven against live .dat + readable back through a real module. dump.pl aliases dropped Europe/Amsterdam tz -> Brussels for DateTime thaw. Remaining modules (ticker alarms, remind, pizza, lastseen) are small-or-stale: recreate at cutover rather than migrate (pizza .dat empty, lastseen 2017-stale). Add transformers if BenV wants them.
- [x] Dockerfile (alpine, 21 MB single static binary), compose file (bot + postgres sidecar, admin on host loopback, junerules host pin); verified live: stack up, Meretrix in #testing, migrations auto-ran, health green
- [x] Parallel run: go-botje as Meretrix on junerules alongside hoer (2026-07)
- keeper/core split DONE: botje keeper + core subcommands, keeper as transparent buffering relay over unix socket, core restart leaves IRC session up (SkipGoodbye), verified live against junerules (one IRC connection across a core restart, no reconnect). compose "split" profile added.
- [x] Cutover (2026-07-13): hoer stopped, data migrated; the nick stays Meretrix by decision
- [x] Keep Perl image around for rollback

The rewrite is DONE and in production. Post-parity work (new modules,
refactors, observability) is tracked in the CLAUDE.md backlogs, not
here.

## Definition of done per module
1. Unit tests for command parsing + output formatting (golden where useful)
2. Storage round-trip test
3. Manual check in #testing on junerules
4. Behavior differences vs Perl documented in the module's doc.go
