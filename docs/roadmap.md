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
- [ ] rss (the big one; fix the short-code collision)
- [ ] pizza (timers)
- [ ] markov (+ dictionary migration, 29 MB)
- [ ] ticker
- [ ] kind
- [ ] wiki, urband, wolframalpha
- [ ] search (replaces google: SearXNG backend, DDG lite fallback)
- [ ] remind (cron reminders + remember/recall, promoted from inactive)
- [ ] llm (unified: openai + bedrock + ollama)
- [ ] nagios
- [ ] optional/inactive: floodkick, qrcode, urlpeek, nectarine, sdcv/spell (postponed by decision)
- dropped: workhours, todo (was never finished), ps3_proxy

## Phase 5: migration + cutover
- [ ] tools/migrate: Storable -> JSON dump + import, verified counts (dump.pl + karma transformer DONE, proven against live IRC_Karma.dat; other modules as they land)
- [ ] Dockerfile (scratch/distroless, single binary), compose file matching current mounts
- [ ] Parallel run: go-botje as Meretrix on junerules alongside hoer, same channels read-only-ish, compare behavior
- [ ] Cutover: stop hoer, migrate fresh data dump, go-botje takes nick hoer
- [ ] Keep Perl image around for rollback

## Definition of done per module
1. Unit tests for command parsing + output formatting (golden where useful)
2. Storage round-trip test
3. Manual check in #testing on junerules
4. Behavior differences vs Perl documented in the module's doc.go
