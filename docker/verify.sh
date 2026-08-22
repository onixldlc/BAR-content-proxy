#!/bin/sh
set -eu

ADDR="${BARPROXY_ADDR:-:8080}"
PORT="${ADDR##*:}"

curl -fsS --max-time 4 "http://127.0.0.1:${PORT}/healthz" > /dev/null
