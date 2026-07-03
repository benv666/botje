# Perl botje: core framework reference

Source of truth: `reference/src/botje-hg/` (extracted from /docker/botje on Uil, gitignored).
This documents the BEHAVIOR the Go rewrite must reproduce, not the Perl idioms.

## Shape

- Single process, single thread, custom select(2) event loop (lib/BenV/Select.pm). All I/O (IRC sockets, telnet command port, HTTP fetches) multiplexed through one select call.
- Everything is a hot-reloadable module. ModuleLoader loads .pm files at runtime, dispatches an event bus, can unload/reload live via telnet without restart.
- Persistence: Storage abstraction, Perl Storable file backend (one .dat per namespace). MySQL backend exists but is read-only/unfinished, ignore it.
- Runtime control via telnet command port (TCP 1924) with login + superuser. IRC is treated as low-trust: no privileged ops via ! commands.

## Boot

parseConfig -> init -> main -> cleanup. Hardcoded core load order: Functions, Conf, Storage, Conf::reload (chicken/egg fix), then modules.autoload line by line. Conf autosave rescheduled every 600s.

Shutdown (INT/TERM): save config, emit QUIT event, ~5s grace loop so modules can say goodbye on IRC, save again, unload all modules in reverse load order (unload() flushes state).

## Scheduler

- Sorted list of one-shot timers {time, cb, args, tag}. schedule($time, $cb, $args): time < now means relative offset. Returns unique tag. unschedule($tag).
- Loop: timeout for select = time to next timer; after select, run all due callbacks, each eval-guarded (a crashing timer is logged and dropped, loop continues).
- Go: min-heap of one-shot timers, panic-isolated callbacks. Used everywhere (flood control, reconnect, autosave, fetch timeouts, !more expiry).

## Module API contract

Each module implements:
- getModuleInfo() -> {name, version, required (botje module deps, load refused if unmet), modules (CPAN deps), extra}
- load() (register hooks/commands, load Storage data)
- unload() (cleanup + PERSIST STATE, this is where most modules flush)
- reload_settings() (re-init after `module reconfigure`)
- getStatus() optional (telnet `module status`)

Event bus (ModuleLoader):
- register_event(name) declares; register_hook(module, event, cb): ONE hook per (module,event) pair.
- submit_event(event, args): calls every hook, eval-guarded. A crashing handler gets its module force-unloaded. Return values collected into arrayref (used by COMMAND event).
- Loop prevention: callchain tracking refuses re-entry of same (module,event) already on the chain.
- Per-call timing stats (min/avg/max) exposed via telnet `module callstats`.

Events: signal, QUIT, config_changed, COMMAND, and IRC_ERROR/INVITE/JOIN/KICK/MODE/NOTICE/PART/PRIVMSG/TOPIC/QUIT (each carries one event hashref).

Handler return protocol (partially enforced): 0 handled/nothing, 1 handled + produced message (2nd value importance 0-100, reply via cmd_eventmsg), 2 stop propagation.

Inter-module calls: direct package calls (IRC::cmd_privmsg etc) or safe indirect ModuleLoader::cf(module, function, args) when target may be absent.

## The event hashref (contract for all IRC hooks)

```
botnick, server,
sender => { nick, user, host [, userId] },
sender_me,                  # bool, sender is the bot
rawcmd => { cmd, params, prefix },
channel,                    # channel, or sender nick for queries
target_me,                  # bool
extra => { msg, query, topic, mode, reason, target, netsplit, unparsed[], ... }
```
Queries: target == botnick sets target_me=1, extra{query}=1, channel rewritten to sender nick.

## IRC layer (IRC.pm, 2515 lines)

- Multi-network: %servers keyed by network name, one socket per network. Persisted serverlist (Storage ns IRC): network -> host -> {host, port, nick, ssl, autoconnect, reconnect, channels}.
- Connect: NICK + USER, channel JOINs scheduled 10s after connect. Autoconnect at module load.
- SSL via Net::SSLeay with SNI, single shared CTX.
- Reconnect backoff: 3, 60, 180, 300s (next larger than last delay); reconnect state resets if last reconnect > 300s ago.
- Netsplit detection heuristic on QUIT (Irssi-style), sets extra{netsplit}.
- Nick handler plugin: one module (IRC_Auth in practice) can register getNickFromId/addNickInfo to attach stable userIds to senders.

### Outbound flood control (must replicate)

- Per-server queues: high prio (PONG etc, bypasses to front) + normal prio bucketed PER CHANNEL, drained round-robin across channels so one busy channel cannot starve others.
- Rate: 1 event per second, max wait 3s. Lines > 80 bytes count as multiple events (1 per 80 bytes).
- Do not send while the socket write buffer is non-empty (backs off incrementally).
- writeServer: colorize translation -> truncate to 510 bytes grapheme-safe -> ensure CRLF.
- Message wrapping at 448 bytes on word boundaries (byte-aware, UTF-8 safe); single words longer than max split by character. cmd_privmsg splits on newlines first.

### cmd_eventmsg + !more pager (replicate this UX)

Replies through cmd_eventmsg send at most anti_flood_max_lines (default 4) lines; the last visible line gets a {W}(+N) suffix; remainder stashed per (channel, nick, command) with 600s expiry. `!more [command]` pages the rest.

### UTF-8 (fiddly, load-bearing)

- Sockets are raw; UTF-8 handled manually. Read path: decode with FB_QUIET so partial multibyte chars at chunk boundaries stay in the buffer. Write path: encode, drop invalid UTF-8 with a warning, tolerate partial writes (EAGAIN/EINTR/ENOBUFS).

## Command systems (two)

### IRC ! commands
- Command char `!` hardcoded (TODO in source to make configurable).
- registerCommand(word, cb): multiple modules may register the same word, all fire.
- registerDefaultCommand(cb, {priority, continue}): catch-alls for non-command text. continue=0 handlers run first by priority desc, stop at first true return; then continue=1 handlers.
- Unknown !command: Levenshtein "did you mean" suggestion (distance <= 2, <= 1 for short commands).
- Callback arg: {command, data, event} (see Example.pm for the documented shape).
- No permissions on ! commands by design.

### Telnet command port (1924)
- Line-based with telnet IAC negotiation (echo off during passwords). Login: 3 failures -> disconnect ("H-h-h-HACKER!!!").
- Modules contribute commands via COMMAND event returning {cb, match (regex with named captures), help, args, su} entries.
- Built-ins: help, conf [setting[=value]], eval <perl> (su, arbitrary code, deliberate backdoor), module load/reload/unload/reconfigure/status/info/callstats, adduser, passwd, irc addserver/delserver/connect/reconnect/disconnect/join/list.

## Auth

- Users: {username -> lc md5_hex(password)}, persisted ONLY on unload. Superuser from Conf (superuser/supasswd, supasswd already md5).
- authUser -> 2 superuser, 1 valid, 0 bad pass, undef no such user. Binary privilege model, no ACLs.
- IRC_Auth: `login <user> <pass>` in query only; superusers REFUSED over IRC. Session dies on user QUIT or bot disconnect. getUser(event) is how IRC modules check auth.

## Storage

- API: getData(namespace, dataname), saveData(namespace, dataname, ref). Namespace == module name by convention. One file per namespace: storage_dir/<ns>.dat (Storable, pst0 magic).
- Lazy load on first getData. saveData writes immediately and synchronously (backup <ns>.dat.$$ during write).
- THE GOTCHA: saveData only fires when a module decides. Conf every 600s; most modules flush only on unload/graceful exit. Live .dat files are stale; kill -9 loses state since last save. Go rewrite needs an explicit flush policy (dirty flag + interval + shutdown).
- hashfilter guard: XML::LibXML blessed refs are stripped on load (deserialized ones segfault). Lesson: never persist opaque native handles.

## Conf

- Two layers: read-only botje.cfg file (only storage backend + superuser bootstrap) and typed registered settings persisted via Storage (createSetting(name, type, default), types int/float/string/bool; setSetting validates and emits config_changed; file config wins over stored value).
- Runtime editing via telnet `conf x=y`.

## Fetcher (async HTTP, ~937 lines)

- Non-blocking, integrated into the select loop. Single-flight per exact URL (duplicate request while pending returns undef).
- fetch(cb, url, options, headers, cbArg). Options: method, postdata (hash -> urlencoded or JSON per Content-Type; string verbatim; presence implies POST), autoredirect (default on, max 8 hops), streamCb/streamFinalCb, authuser/authpass (basic), timeout (default 30s inactivity, cbTimeout optional), sizeLimit (kill socket when over), aws => {} (SigV4 signing).
- Callback: cb(url, body, \%headers, cbArg). Injected pseudo-headers: __HTTP_RESULT__, __HTTP_RESULT_CODE__, __HTTP_RESULT_STATUS__, __SIZE_LIMIT_REACHED.
- HTTP/1.1, Connection: close, hand-rolled chunked transfer decoding.
- AWS: creds from env or Conf fetcher_aws_credentials (base64 "KEYID|SECRET"). AWS::Signature4 locally patched for Bedrock (service scope bedrock, Connection header removed before signing).

## Functions + Log (output formatting, load-bearing)

- colorize(msg): converts {x} color tags. Tags: {/} reset, {r}{g}{y}{b}{m}{c}{w} colors, {R}{G}{Y}{M}{C}{W} bright, {B} bold, {_} underline, {n} deterministic per-word color, {i}..{u} ignore range, {I} ignore to EOL. Pipeline: {x} tags -> ANSI -> mIRC color codes (translateColors in IRC.pm, with bold state machine). All user-visible output flows through this.
- tfcprint: table/box formatter with color-aware padding (telnet help/conf/irc list).
- sparkline(values, rows): UTF-8 block chars with red/green coloring (Ticker).
- Log: STDERR, colored "DD-MM-YYYY HH:MM:SS Caller:" prefix, width-aware wrapping, stable per-name colors. Loglevel gated by -v count.

## Known bugs / TODOs in the Perl (fix in Go, do not port)

- RSS short-code collision: getNextUniqueID wraps at 32768 with no liveness check, while feeds keep newest 10 items forever. Duplicate _id resolves to first feed alphabetically (diagnosed live 2026-07-03, the !LI6 cats incident).
- IRC_Test in modules.autoload has no matching loadable file (IRC_Test_rrqueue.pm has a syntax error: `'modules' => [].`).
- efMode mode parsing incomplete. Command char hardcoded. MySQL backend writes unimplemented. Log::log is a stub. Fetcher chunk/UTF-8 boundary bug documented in its header.

## Core dependency graph

Functions(none), Conf(none), Storage(Conf), Log(Conf), Auth(Conf,Storage), Command(Auth,Conf,Log), IRC(Conf,Storage), IRC_Auth(IRC,Auth), Fetcher(none). Boot order fixed in botje.pl + modules.autoload.
