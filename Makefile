GO ?= go

# Site-specific values (BOTJE_IRC_ADDR etc.) live in the gitignored
# .env; run/livetest source it in-shell (make itself would eat the #
# in values like BOTJE_CHANNELS=#testing as a comment).
LOADENV = if [ -f .env ]; then set -a; . ./.env; set +a; fi

.PHONY: test build vet cover run livetest keeper-image

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build -o bin/botje ./cmd/botje

cover:
	$(GO) test -coverprofile=cover.out ./...
	$(GO) tool cover -func=cover.out | tail -1

# live integration tests against junerules #testing, see docs/architecture.md
# (needs BOTJE_IRC_ADDR from .env)
livetest:
	@$(LOADENV); BOTJE_LIVE_TEST=1 $(GO) test -count=1 -run Live ./...

# run the bot against junerules #testing as Meretrix (ctrl-c to quit).
# Needs BOTJE_IRC_ADDR from .env. BOTJE_PG_DSN=postgres://... for
# persistent storage, else in-memory.
# telnet admin on 127.0.0.1:1924 needs a user:
#   quick dev:  BOTJE_SUPERUSER=benv:somepass make run
#   persistent: BOTJE_PG_DSN=... ./bin/botje adduser benv somepass
run:
	@$(LOADENV); $(GO) run ./cmd/botje standalone

# build + tag a pinned keeper image; put the printed tag in .env as
# BOTJE_KEEPER_IMAGE. The keeper is not rebuilt by compose on purpose:
# recreating it drops the IRC session, so its image only changes when
# you decide it should.
keeper-image:
	docker build -t go-botje:keeper-$$(date +%Y%m%d) .
	@echo ""
	@echo "Built go-botje:keeper-$$(date +%Y%m%d)"
	@echo "Set in .env:  BOTJE_KEEPER_IMAGE=go-botje:keeper-$$(date +%Y%m%d)"
	@echo "Then:         docker compose up -d"
