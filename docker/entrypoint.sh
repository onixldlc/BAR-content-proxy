#!/bin/sh
set -eu

# The cache directory is normally a volume, so its ownership comes from the
# host and may not match the runtime user. Fail loudly instead of dying on
# the first write.
if [ ! -w "${BARPROXY_CACHE_DIR:-/var/cache/barproxy}" ]; then
    echo "entrypoint: cache dir ${BARPROXY_CACHE_DIR:-/var/cache/barproxy} is not writable by $(id -un)" >&2
    echo "entrypoint: chown it on the host to uid $(id -u), or bind-mount a writable path" >&2
    exit 1
fi

if [ -z "${BARPROXY_PUBLIC_URL:-}" ]; then
    echo "entrypoint: BARPROXY_PUBLIC_URL unset; rewritten URLs follow the client's Host header"
fi

exec /usr/local/bin/barproxy "$@"
