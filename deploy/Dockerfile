# Daemon image: builds the static suprcow binary, then runs it on a base that
# has the docker CLI (+ compose plugin) and git, since the daemon shells out to
# `docker compose` and `git`. The host's docker socket is mounted in at runtime.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/suprcow ./cmd/suprcow

FROM docker:27-cli
RUN apk add --no-cache git ca-certificates
COPY --from=build /out/suprcow /usr/local/bin/suprcow
ENTRYPOINT ["suprcow"]
