#!/bin/sh
# ----------------------------------------------------------------------
# Launch Beyond All Reason with downloads routed through BAR-proxy.
#
# The exports live and die with this script, so nothing about your normal
# BAR install changes. Run the game directly to go back to stock.
#
# EDIT THE TWO LINES BELOW.
# ----------------------------------------------------------------------
set -eu

# 1. Where your proxy lives. Scheme and port included, no trailing slash.
PROXY="${PROXY:-http://your-proxy:8080}"

# 2. Path to the BAR launcher. Leave empty to try the usual locations.
BAR_EXE="${BAR_EXE:-}"

# ----------------------------------------------------------------------

export PRD_RAPID_REPO_MASTER="$PROXY/repos.gz"
export PRD_HTTP_SEARCH_URL="$PROXY/find"

if [ -z "$BAR_EXE" ]; then
    for candidate in \
        "$HOME/.local/share/Beyond-All-Reason/Beyond-All-Reason" \
        "$HOME/Beyond-All-Reason/Beyond-All-Reason" \
        "$(dirname "$0")/Beyond-All-Reason" \
        "$(command -v Beyond-All-Reason 2>/dev/null || true)"
    do
        if [ -n "$candidate" ] && [ -x "$candidate" ]; then
            BAR_EXE="$candidate"
            break
        fi
    done
fi

if [ -z "$BAR_EXE" ]; then
    echo "Could not find the Beyond All Reason launcher." >&2
    echo "Run again with: BAR_EXE=/path/to/Beyond-All-Reason $0" >&2
    exit 1
fi

echo "Routing BAR downloads through $PROXY"
echo "Launching $BAR_EXE"
exec "$BAR_EXE" "$@"
