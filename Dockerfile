# Single static binary in a minimal image. Two stages: build with the
# Go toolchain, run on alpine with just CA certs (HTTPS to feeds/APIs
# and the TLS ircd) and tzdata (Europe/Amsterdam formatting).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /botje ./cmd/botje

FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata \
	&& adduser -D -u 1000 botje
COPY --from=build /botje /usr/local/bin/botje
USER botje
ENV TZ=Europe/Amsterdam
ENTRYPOINT ["/usr/local/bin/botje"]
# default is the safe parallel-run config; compose overrides as needed
CMD ["standalone"]
