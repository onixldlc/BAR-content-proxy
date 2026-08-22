FROM golang:1.23-alpine AS build

WORKDIR /src

COPY go.mod ./

RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/barproxy .


# Cross compiled release binaries for every published OS/arch. Kept separate
# from the runtime image on purpose: the mac and windows binaries live in no
# image, and the CI `release` job exports this stage's root with
# `--target dist --output type=local`.
FROM golang:1.23-alpine AS distbuild

WORKDIR /src

COPY go.mod ./

RUN go mod download

COPY *.go ./

COPY docker/build-dist.sh /usr/local/bin/build-dist.sh

ARG VERSION=dev

RUN VERSION="${VERSION}" sh /usr/local/bin/build-dist.sh

# Scratch export target: its whole filesystem is the release binaries and
# nothing else, so `output type=local` lands a clean dist/ dir.
FROM scratch AS dist

COPY --from=distbuild /out/ /


FROM alpine:3.20 AS runtime

RUN apk add --no-cache ca-certificates

RUN apk add --no-cache curl

RUN addgroup -S barproxy

RUN adduser -S -G barproxy -h /var/cache/barproxy barproxy

RUN mkdir -p /var/cache/barproxy

RUN chown -R barproxy:barproxy /var/cache/barproxy

COPY --from=build /out/barproxy /usr/local/bin/barproxy

COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh

COPY docker/verify.sh /usr/local/bin/verify.sh

RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/verify.sh

ENV BARPROXY_ADDR=:8080
ENV BARPROXY_CACHE_DIR=/var/cache/barproxy

USER barproxy

EXPOSE 8080

VOLUME ["/var/cache/barproxy"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD /usr/local/bin/verify.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
