# go-botje

Ground-up Go rewrite of botje, BenV's Perl IRC bot from ~2007 that runs as nick **hoer** on irc.benv.junerules.com (container Botje-Owl on Uil). End goal: a single Go binary in a minimal Docker image that replaces hoer with functional parity or better.

## Read these before doing anything

1. `docs/architecture.md`: the design + OPEN QUESTIONS. If a decision there is still marked proposal, debate it with BenV before building on it.
2. `docs/roadmap.md`: phase plan and per-module parity checklist. Work top to bottom.
3. `docs/perl-reference/`: the behavioral spec extracted from the Perl source (core-framework.md, modules.md, deployment.md). When in doubt about behavior, this plus the actual Perl source is the truth.

The original Perl tree + live data snapshot is in `reference/` (gitignored, extracted from /docker/botje on Uil). Read it freely; never commit it. The .env and .dat files in there contain live secrets and IRC auth. Do not paste their contents anywhere.

## Hard rules

- **TDD.** Test first, then code. No component lands without tests. Protocol, flood control, formatting, and storage are pure enough for table-driven tests; write them that way.
- **No external IRC/bot frameworks.** The IRC layer is ours (stdlib + crypto/tls). Deliberate choice, same as the Perl bot. Small focused libs for peripheral stuff (sigv4, qr) are fine; discuss anything bigger.
- **Never test against live channels.** Testing happens on irc.benv.junerules.com port 6669 (TLS), channel **#testing** only. Test nick: **putain** (French for hoer; if taken, ask BenV). Never connect as hoer, never join #rss or other live channels from test code. Live integration tests are gated behind `BOTJE_LIVE_TEST=1`.
- **Don't touch the running hoer.** No writes to /docker/botje on Uil, no telnet to its 1924, no restarts, unless BenV explicitly asks.
- **No em-dashes** in any output: code comments, docs, commit messages. Plain hyphens, commas, or colons. This is a BenV-wide rule.
- **Commit style:** lowercase imperative, short, like the Perl repo ("fix utf-8 boundary handling in reader"). BenV reviews and pushes; do not push to remotes.

## What botje is (30-second version)

Single-process event-driven IRC bot. Custom select loop, hot-reloadable modules, event bus with per-module hooks, `!` commands with Levenshtein did-you-mean, per-channel round-robin flood control (1 msg/s), 4-line reply pager with `!more`, `{x}` color-tag formatting, per-module key-value storage (Storable .dat files), async HTTP fetcher (incl. AWS SigV4 for Bedrock), telnet admin port 1924. Details in docs/perl-reference/core-framework.md.

17 working active modules to port. The big four: RSS (feed subscriptions per channel, item recall by short id), Markov (learns channel chatter, 29 MB dictionary), Karma (`item++`/`item--`), Ticker (BTC/ETH/stocks with sparklines). Full inventory: docs/perl-reference/modules.md.

## Architecture (locked once debated)

Single dispatcher goroutine owns module event dispatch (modules stay race-free, like the Perl select loop). Blocking work in goroutines, results re-enter via the event channel. Modules are compiled-in Go packages behind a registry with runtime enable/disable; no dynamic loading. Storage: JSON per namespace, dirty-flag flush every 60s + on shutdown. Package layout in docs/architecture.md.

Carry over verbatim so Perl module logic ports 1:1: event struct shape, command registration semantics (priority/continue default handlers), colorize tag syntax, pager behavior, fetcher option set.

Fix, do not port: RSS short-code collision (the !LI6 incident, see docs/perl-reference/core-framework.md "Known bugs"), hardcoded API keys, flush-on-unload-only persistence, the ChatGPT httpbin debug POST, the telnet eval backdoor.

## Current status (update this section every session)

- 2026-07-03: investigation done, reference docs written, architecture PROPOSED (open questions in docs/architecture.md not yet debated with BenV). No Go code yet. Next: debate open questions, lock decisions, then Phase 0/1 of docs/roadmap.md.

## Environment notes

- Go toolchain: check `go version` locally before assuming.
- The junerules ircd is BenV's own (192.168.178.2). TLS on 6669 verified working (TLS 1.3, wildcard cert *.benv.junerules.com).
- Data migration source: reference/mounts/data/*.dat (Perl Storable). Migration plan in docs/architecture.md.
- Prod deployment target mirrors current: compose on Uil, /data volume, loopback admin port. See docs/perl-reference/deployment.md.
- Editing this file from a benv-brain session: the vault's protect-system-files hook blocks any Write to a file whose basename is CLAUDE.md (over-broad basename match; it means to protect only the vault dispatcher). Run go-botje work in its own session from ~/code/go-botje.
