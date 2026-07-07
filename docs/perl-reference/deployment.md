# Perl botje: deployment + runtime reference

## Production (Uil, /docker/botje)

- Container Botje-Owl, image botje:<IMAGE_TAG> (date-tagged, e.g. 04_06_2026), built via Makefile `botje` target.
- Compose: /docker/botje/docker-compose.yml. Volume mounts/data -> /data (all persistent state). Port 127.0.0.1:1924:1924 (telnet control, loopback only). extra_hosts pins the ircd FQDN to its LAN address. restart unless-stopped. Healthcheck: nc -vnz localhost 1924.
- Env: COLUMNS=280, TZ=Europe/Amsterdam, VERBOSE=-v, secrets from .env: OPENAI_API_KEY, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY. NOTE: .env holds plaintext keys and is inside the reference tarball copy too.
- Update flow: build date-tagged image, bump IMAGE_TAG in .env, ./restart (= ./stop && ./run). Hot module fixes: Makefile syncmod docker-cps modules into the container, reload via telnet 1924.
- Test stack: src/botje-hg/docker-compose.yaml, container Botje-Test, mounts ./testdata:/data, VERBOSE='-v -v -v'.

## Image

Alpine two-stage. Runtime: perl + libs + sdcv (+ dict data in docker/root/usr/local/share/sdcv). CPAN via cpm from docker/root/build/cpanfile (~55 modules; notable pins: DateTime::TimeZone ==2.52 for the dropped Europe/Amsterdam zone, hacked around by copying Brussels.pm; AWS::Signature4 locally patched for Bedrock). Runs as unprivileged botje user, WORKDIR /botje, entrypoint symlinks /data/botje.cfg and execs perl botje.pl $VERBOSE.

## Config and state

- botje.cfg: only storage backend (storage_method=file, storage_dir=/data/) + superuser bootstrap. Everything else lives in Storage .dat files, edited at runtime via telnet conf command.
- IRC connection details live in IRC.dat (serverlist): host = the junerules ircd FQDN, nick hoer. Auth state in Auth.dat / IRC_Auth.dat. Runtime settings in Conf.dat (command_port 1924, markov settings, allowed model lists, fetcher_aws_credentials).

## Live .dat inventory (2026-07-03 snapshot in reference/mounts/data/)

| file | size | note |
|---|---|---|
| IRC_Markov.dat | 28.9 MB | dictionary, actively written (save per 50 learned lines) |
| IRC_RSS.dat | 2.15 MB | feeds + history; mtime Jun 5 = stale on disk, state in memory |
| IRC_Karma.dat | 1.33 MB | saved on every karma change |
| IRC_Ticker.dat | 333 KB | |
| IRC_Ego.dat | 137 KB | |
| IRC_Lastseen.dat | 21 KB | mtime 2017, only saved on unload |
| IRC_Kind.dat | 2.8 KB | |
| Conf.dat / IRC.dat / IRC_Auth.dat / Auth.dat / Pizza.dat / IRC_Nagios.dat / Remind.dat | < 1 KB each | |

Helper scripts in mounts/data: feeddumper.pl (dump RSS feeds structure), test.pl (Data::Dumper any Storable file). Useful for writing the Go migration tool.

## History

- Project ~2007 (SVN-era $Id$ markers), via Mercurial ("botje-hg") to git. Git slice: 80 commits, last 2024-06-12. Recent years mostly LLM integrations (Bedrock/Claude, ChatGPT, Ollama).
- rust/ dir: abandoned 2018 PoC by Bram (ZeroMQ hub + module processes). No constraints on us, ignore.
- Remote: gitlab.com/benv666/botje.
