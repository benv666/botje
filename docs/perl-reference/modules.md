# Perl botje: module inventory

All 29 feature modules in reference/src/botje-hg/modules/{extra,user}. ACTIVE = in the live autoload set on Uil.

Shared infra every module leans on: colorize {x} tags, IRC::cmd_privmsg / cmd_eventmsg (+ !more pager), registerCommand / registerDefaultCommand(priority, continue), passive IRC_PRIVMSG/IRC_KICK hooks, Storage get/save per namespace, main::schedule timers, async Fetcher, Conf settings.

## ACTIVE (18)

### IRC_RSS (1417 lines, LARGE)
Per-channel/query RSS + Atom subscriptions with polling and item recall.
- `!rss add <url> [refresh=<min>] [query|#channel] [tag=<tag>] [grep="<regex>"]` (refresh min 5)
- `!rss del|refresh|list [url|key]|last <url>`, `!rss <itemId>` recall; repeat same id to page (3 lines, +N suffix, then "No more!"). Bare `<id>` works via default handler (priority 10, continue 0).
- Item line: `[<id>, <tag>(, <size> GB)(, -Xmin/-X.Xh/-Xd)] <title> <link>` truncated 250 chars. Recall wraps at 450 cols.
- Storage: feeds (url -> subscriptions per server/channel with received guid=>epoch, fetcher state, history guid -> item with _id, tag, grep), maxItemId.
- Polling: per-feed refresh (default 30 min), staggered restore, watchdog +30s, retry 60s x3 then 1h, noReschedule after 100 XML errors.
- Broadcast: oldest first; new subscriber gets last 5; >25 pending items caps at 25. History: drop items older than 24h once feed history > 10 items.
- Short ids: base-32 alphabet A-Z,0,2,4,6,8,9, wraps 32768, skips command words. KNOWN BUG: no liveness check, collisions resolve to first feed alphabetically. Fix in Go.
- Insulting error strings are part of the UX ("learn to read", "go back to regex school, failkut").

### IRC_Markov (898 lines, LARGE)
Markov chatter, learns channel text.
- `!talk [seed]`, two default handlers (responds broadly), passive learner on every non-command line.
- Storage: dictionary_<order>_<name> nested word-count hash, saved every 50 lines. 29 MB live.
- Conf: markov_dictionary (default), markov_order (3), markov_idle_talk (bool), markov_idle_talk_timeout (240 min), markov_idle_talk_channels.
- Idle talker per channel, 1/16 chance "Lala...". Hardcoded bot-filter nicks (X, x, the_baby, hoer, dromertje, calvin, bot, lippy) and nick-capitalization list (benv, lotjuh). Emits "*BLLEUEURRURUHGHG*." when stuck.

### IRC_Ego (294 lines, SMALL)
Counts I/me/my (EN+NL: ik/mijn/mij) per nick. `!ego [nick]`. Auto-report every ~200 hits. Storage: ego per server/nick/channel {egoCount, sentences}, saved every 100 msgs.

### IRC_Karma (366 lines, SMALL)
`!<item>++ / !<item>-- [# reason]`, `!<item>?`, `!wku/!wkd <item>` (reason lists). Per-channel + global karma. Kicker of the bot gets -1 "For kicking defenseless bots". Skips registered command words (avoids capturing passwords). Storage: karma, saved on every change. 1.3 MB live.

### IRC_ChatGPT (425 lines, MEDIUM)
`!gpt <prompt>`, OpenAI chat completions, gpt-3.5-turbo, OPENAI_API_KEY env. Per-channel history (16 entries), in-memory only. Request queue serializes. Jailbreak persona prompts, last one (DAN) wins. Debug POST to http://192.168.178.2/anything on every request (drop in Go). Hardcoded user list.

### IRC_Google (412 lines, MEDIUM)
`!google <q>`, `!next [n|last]` (max 3, per channel+nick memory). Scrapes google.nl HTML (brittle). Parses calculator one-box. 15s spam throttle.

### IRC_TinyURL (320 lines, SMALL)
`!tinyurl <url>` + passive: auto-shortens any URL > 40 chars seen in channel. tinyurl.com API. Dedups concurrent ("Already fetching that url you asshole!").

### IRC_Pacman (215 lines, TRIVIAL)
Replies ASCII pacman art to messages starting with 2+ dots. Exactly 2 dots: 70% chance to ignore. 5 hardcoded art templates, %s -> nick.

### IRC_Nagios (591 lines, MEDIUM)
`!nagios <status.dat url> [user pass]`, `!nagioskill <url>|all`, `!nagiosinfo`. Polls every 5 min, alerts subscribers in query on state changes. Storage: watches.

### IRC_Lastseen (298 lines, SMALL)
`!last <nick>` + passive recorder. Storage: activity {server}{lc nick}{channels}{channel}{msg, time}, saved on unload only. part/quit/join handlers exist but are NOT registered (dormant).

### IRC_Sdcv (315 lines, SMALL)
`!d <word>`, `!d #<n>`. Shells out to sdcv binary (dict data in /usr/local/share/sdcv). 1h result cache per nick. Debate for Go: keep shelling out vs drop vs pure-Go dict reader.

### IRC_WolframAlpha (160 lines, SMALL)
`!wa <query>` via Custom::WolframAlpha wrapper (API key in that lib). Colorized pods, truncated.

### IRC_Wiki (269 lines, SMALL)
`!wiki <q>`: en.wikipedia REST search, top 3 titles + excerpts. utf8=0 in URL required or JSON decode crashes (their note). 15s throttle.

### IRC_UrbanD (198 lines, SMALL)
`!ud <term>` via RapidAPI urban dictionary, HARDCODED API key in source. 10s timeout "Boohoo!". Uses cmd_eventmsg pager.

### Pizza (user, 896 lines, LARGE)
`!pizza` / `!timer`: alarms + stopwatch. Timespec: absolute h:m[:s], d-m-yyyy, relative +/-N[smhdwy|mo], weekday names, repeat r{...} (min 1800s "FU."). IDs are Greek-letter names (alpha-0..omega-7). Default msg "QUICK! Pizza <id> is burning!". `when <id>`, `clear [id]` (clear-all needs repeat within 60s "Say again?"), `start/stop` stopwatch. Storage: timers per lc nick. Near-past timers on restore delayed +20s.

### IRC_Kind (540 lines, MEDIUM)
Dutch children registry. `!nieuwkind`/`!kind add`, `!vergeetkind`, `!kind`/`!k` query by name/parent/year. Dutch replies with age in years+months, parents, siblings (parent cache rebuilt on load). Storage: child.

### IRC_Ticker (1222 lines, LARGE)
Financial tickers. `!ticker add SYM [query|#channel] | del | show SYM [height] | list | refresh | setalarm SYM low=/high=/up=/down= | delalarm`. Bare `SYM [height]` via default handler. Sources: blockchain.info (BTC), Kraken (ETH), alphavantage (stocks, HARDCODED key). Refresh 30 min; broadcast max 1/6h unless IQR-based jump detection fires. Unicode sparkline graphs (height cap 8). Alarms to user query. Storage: tickers + tickerdata (96 points, ~24h).

### IRC_Test (autoload entry, BROKEN)
Autoload names IRC_Test; only file is IRC_Test_rrqueue.pm (rr send-queue test spammer) which has a syntax error and cannot load. Effectively dead. Do not port; write proper Go tests for the RR queue instead.

## INACTIVE (11)

- IRC_Bedrock (pkg IRC_Claude, 514): `!claude [model] <prompt>`, AWS Bedrock SigV4, default model anthropic.claude-haiku-4-5, allowed-list in Conf claude_allowed_models. Long replies redirect to query.
- IRC_Ollama (441): `!oi [model] <prompt>` against LAN Ollama 192.168.178.4:11434, Conf oi_allowed_models.
- IRC_Floodkick (223): kick on >= 4 messages / > 600 chars in 12s. Random kick messages.
- IRC_Nectarine (224): `!necta` now-playing from Nectarine demoscene radio (has an undeclared-var bug).
- IRC_QRCode (189): `!qr <text>`: block-char QR whispered in query. Text::QRCode.
- IRC_Spell (236): `!spell [lang] <word>` via aspell bindings, default lang nl.
- IRC_Todo (375): per-user todos, query path NEVER IMPLEMENTED (returns empty). Half-finished, author lotjuh.
- IRC_URLPeek (178): passive URL fetcher, prints Type/Title/Description, 256 KB limit, uses file(1).
- Remind (user, 561): cron-syntax reminders (`!remind m h dom mon dow msg`) + `!remember/!recall/!forget` (3 per key). DateTime::Event::Cron. Profanity easter eggs.
- WorkHours (user, 763): `!hours` timesheet tracking with ASCII table rollups. Requires IRC_Auth login.
- Example (user, 259): template module, documents the event hash shape.
- PS3_Proxy (extra, 561): legacy PS3 HTTP proxy, out of scope.

## Summary table

| module | active | complexity | external deps |
|---|---|---|---|
| IRC_RSS | yes | large | XML feeds over HTTP, basic auth |
| IRC_Markov | yes | large | none |
| IRC_Ego | yes | small | none |
| IRC_Karma | yes | small | none |
| IRC_ChatGPT | yes | medium | OpenAI API (env key) |
| IRC_Google | yes | medium | google.nl HTML scrape |
| IRC_TinyURL | yes | small | tinyurl.com API |
| IRC_Pacman | yes | trivial | none |
| IRC_Nagios | yes | medium | Nagios status.dat HTTP |
| IRC_Lastseen | yes | small | none |
| IRC_Sdcv | yes | small | sdcv binary + dicts |
| IRC_WolframAlpha | yes | small | WolframAlpha API |
| IRC_Wiki | yes | small | Wikipedia REST |
| IRC_UrbanD | yes | small | RapidAPI (hardcoded key) |
| Pizza | yes | large | none |
| IRC_Kind | yes | medium | none |
| IRC_Ticker | yes | large | blockchain.info, Kraken, alphavantage |
| IRC_Test | yes* | broken | none |
| IRC_Bedrock | no | medium | AWS Bedrock SigV4 |
| IRC_Ollama | no | medium | LAN Ollama |
| IRC_Floodkick | no | small | none |
| IRC_Nectarine | no | small | Nectarine API |
| IRC_QRCode | no | small | qrcode lib |
| IRC_Spell | no | small | aspell |
| IRC_Todo | no | incomplete | none |
| IRC_URLPeek | no | small | file(1) |
| Remind | no | medium | cron parser |
| WorkHours | no | large | none |
| Example | no | template | none |
