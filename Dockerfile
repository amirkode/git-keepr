# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine3.23 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags "-s -w -X github.com/amirkode/git-keepr/internal/cli.Version=${VERSION} -X github.com/amirkode/git-keepr/internal/cli.Commit=${COMMIT}" \
      -o /out/git-keepr ./cmd/git-keepr

# ---- runtime stage ---------------------------------------------------------
FROM alpine:3.23
# git: the only runtime dependency. ca-certificates: TLS to GitHub and S3.
RUN apk add --no-cache git ca-certificates \
 && addgroup -g 10001 -S git-keepr \
 && adduser -u 10001 -S -G git-keepr -h /data -H -s /sbin/nologin git-keepr \
 && install -d -m 0700 -o 10001 -g 10001 /data
COPY --from=build /out/git-keepr /usr/local/bin/git-keepr
# Non-root. Named volumes are initialised from /data above, so UID 10001 can write them.
USER 10001:10001
WORKDIR /data
# No secrets are baked in: PATs and S3 keys arrive via environment variables at run time.
ENTRYPOINT ["/usr/local/bin/git-keepr"]
CMD ["serve", "--config", "/etc/git-keepr/config.yaml"]
