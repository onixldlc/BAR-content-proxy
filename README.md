# BAR-proxy

A caching content proxy for [Beyond All Reason](https://www.beyondallreason.info/).

It is a real HTTP server, not a SOCKS/VPN tunnel. Players point two
pr-downloader environment variables at it and every game, map and engine
download flows through the proxy instead of hitting the CDN edge directly.
No third-party client, no SOCKS4/5, no system-wide tunnel.

## Why this exists

BAR content is served from BunnyCDN. Some ISPs route badly to the nearest
edge, and downloads crawl or stall for everyone on that ISP. Running this on
a host with a good path to the CDN gives affected players a working route,
and caches the results so the second peer to download a map gets it at LAN
speed.

## Contents

- [How it works](#how-it-works)
- [Part 1 — starting the server](#part-1--starting-the-server) (one person hosts it)
- [Part 2 — pointing BAR at it](#part-2--pointing-bar-at-it) (every player)
- [Server environment variables](#server-environment-variables)
- [Client environment variables](#client-environment-variables)
- [Security](#security)
- [Limits](#limits)

---

## How it works

The rapid protocol names absolute URLs in exactly one place: the master
`repos.gz`. Everything else — `versions.gz`, `packages/<md5>.sdp`,
`pool/<xx>/<hash>.gz`, `streamer.cgi` — is resolved relative to the repo
base URL found in that file.

So the proxy only has to rewrite two documents:

| Endpoint | What it does |
| --- | --- |
| `/repos.gz` | Fetches the upstream master, rewrites each repo's base URL to `/u/<host>/<path>`, re-gzips it |
| `/find` | Proxies the search API, rewriting the absolute `mirrors[]` URLs |
| `/u/<host>/<path>` | Transparent cached proxy for everything else |
| `/healthz` | Liveness check |
| `/stats` | Cache hit/miss counters |
| `/` | Prints the exact two config lines a player needs |

Because the rewrite happens at the root, the client walks the entire
download tree through the proxy on its own.

Verified against live upstream:

```
main,https://repos-cdn.beyondallreason.dev/main,,
byar,https://repos-cdn.beyondallreason.dev/byar,,
byar-chobby,https://repos-cdn.beyondallreason.dev/byar-chobby,,
```

becomes

```
main,http://your-proxy:8080/u/repos-cdn.beyondallreason.dev/main,,
byar,http://your-proxy:8080/u/repos-cdn.beyondallreason.dev/byar,,
byar-chobby,http://your-proxy:8080/u/repos-cdn.beyondallreason.dev/byar-chobby,,
```

### What makes it faster

- **Parallel range fetching.** Objects over 8 MiB are pulled with 4
  concurrent range requests. This is the main lever when an edge throttles
  per-connection rather than per-client.
- **Progressive streaming.** Clients read the file while it is still being
  downloaded, from the contiguous completed prefix, so a parallel fetch does
  not stall the progress bar.
- **Detached downloads.** Once started, a transfer completes into the cache
  even if the client disconnects. The next peer gets a hit.
- **Single-flight.** Twenty players starting the same map at once cost one
  upstream transfer.
- **Immutable caching.** `pool/`, `packages/` and `/file/` objects are
  content-addressed, so they are cached forever. Control files get a 5 min
  TTL.

---

## Part 1 — starting the server

One person runs this, on a host with a good route to the CDN: a cheap VPS,
or one peer's machine if their ISP is fine. Everyone else just points their
game at it.

Whichever method you pick, **`BARPROXY_PUBLIC_URL` is the setting that
matters**. It gets written *into* the rewritten `repos.gz`, so it must be the
URL your peers can reach. Set it to `localhost` and your friends get sent
back to their own machines. Leave it empty and the proxy derives it
per-request from the `Host` header, which is usually right on a LAN.

### Option A — Docker Compose (recommended)

```sh
git clone https://github.com/onixldlc/BAR-proxy
cd BAR-proxy

cp .env.example .env
$EDITOR .env            # set BARPROXY_PUBLIC_URL

docker compose up -d
docker compose logs -f
```

Stop, update, reset:

```sh
docker compose down                 # stop, keep the cache
docker compose pull                 # or: docker compose build --pull
docker compose up -d --build        # rebuild and restart
docker compose down -v              # stop and wipe the cache volume
```

Podman works the same way with `podman-compose`, or build and run directly:

```sh
podman build --format docker -t bar-proxy:latest -f docker/Dockerfile .
podman run -d --name bar-proxy \
    -p 8080:8080 \
    -e BARPROXY_PUBLIC_URL=http://bar-proxy.example.com:8080 \
    -e BARPROXY_CACHE_MAX_BYTES=53687091200 \
    -v barproxy-cache:/var/cache/barproxy \
    bar-proxy:latest
```

`--format docker` matters: podman defaults to the OCI image format, which
has no healthcheck field, so the Dockerfile's `HEALTHCHECK` is silently
dropped (podman prints a warning mid-build and `podman inspect` then reports
`HealthCheck: <nil>`). Docker's own builder honours it either way. If you
would rather not think about image formats, use Compose — the healthcheck is
declared there too and works on both runtimes regardless.

Verify it took:

```sh
podman inspect bar-proxy:latest --format '{{.HealthCheck.Test}}'
podman inspect bar-proxy --format '{{.State.Health.Status}}'   # -> healthy
```

### Option B — plain Docker

```sh
docker build -t bar-proxy:latest -f docker/Dockerfile .
docker run -d --name bar-proxy --restart unless-stopped \
    -p 8080:8080 \
    -e BARPROXY_PUBLIC_URL=http://bar-proxy.example.com:8080 \
    -v barproxy-cache:/var/cache/barproxy \
    bar-proxy:latest
```

Docker's builder honours the Dockerfile's `HEALTHCHECK` with no extra flags:

```sh
docker inspect --format '{{.State.Health.Status}}' bar-proxy   # -> healthy
```

### Option C — straight from source, no containers

Needs Go 1.23 or newer. No dependencies beyond the standard library.

```sh
git clone https://github.com/onixldlc/BAR-proxy
cd BAR-proxy
go build -o barproxy .

BARPROXY_ADDR=:8080 \
BARPROXY_CACHE_DIR=./cache \
BARPROXY_PUBLIC_URL=http://bar-proxy.example.com:8080 \
./barproxy
```

For a quick LAN test, the defaults are enough — just `./barproxy`.

### Option D — systemd, for a VPS that should survive reboots

A unit file is included at `scripts/barproxy.service`.

```sh
go build -o barproxy .
sudo install -m 0755 barproxy /usr/local/bin/barproxy

sudo useradd --system --home /var/lib/barproxy --shell /usr/sbin/nologin barproxy
sudo mkdir -p /var/lib/barproxy/cache
sudo chown -R barproxy:barproxy /var/lib/barproxy

sudo install -m 0644 scripts/barproxy.service /etc/systemd/system/
sudo $EDITOR /etc/systemd/system/barproxy.service   # set BARPROXY_PUBLIC_URL

sudo systemctl daemon-reload
sudo systemctl enable --now barproxy
sudo systemctl status barproxy
journalctl -u barproxy -f
```

### Check the server is working

```sh
curl http://your-proxy:8080/healthz          # -> ok
curl http://your-proxy:8080/                 # prints the two client lines
curl -s http://your-proxy:8080/repos.gz | gunzip -c
curl http://your-proxy:8080/stats
```

The `repos.gz` output should show your proxy's URL on every line. If it says
`localhost` or `127.0.0.1`, fix `BARPROXY_PUBLIC_URL` before handing the
address to anyone.

Don't forget to open port 8080 in the firewall:

```sh
sudo ufw allow 8080/tcp                                   # Debian/Ubuntu
sudo firewall-cmd --add-port=8080/tcp --permanent         # Fedora/RHEL
sudo firewall-cmd --reload
```

---

## Part 2 — pointing BAR at it

Every player does this. It is two environment variables read by
pr-downloader — no files edited inside the game install, nothing to
uninstall later.

```
PRD_RAPID_REPO_MASTER=http://your-proxy:8080/repos.gz
PRD_HTTP_SEARCH_URL=http://your-proxy:8080/find
```

Replace `your-proxy:8080` with whatever the host handed you. Visiting the
proxy's `/` page in a browser prints these two lines already filled in.

### Windows — launcher script (recommended)

Use `scripts/bar-proxy.bat`. Open it, set `PROXY` on the line near the top,
then run it instead of the normal BAR shortcut:

```bat
set "PROXY=http://your-proxy:8080"
```

It tries the usual install locations; if it can't find the game it will tell
you to set `BAR_EXE` to the full path of `Beyond-All-Reason.exe`.

This is the recommended route because `setlocal` keeps the variables inside
that one launch. Nothing on the system changes, and going back to stock is
just launching the game normally.

The whole script, if you would rather paste it into a new `.bat` yourself:

```bat
@echo off
setlocal
set "PROXY=http://your-proxy:8080"
set "PRD_RAPID_REPO_MASTER=%PROXY%/repos.gz"
set "PRD_HTTP_SEARCH_URL=%PROXY%/find"
start "" "%LOCALAPPDATA%\Programs\Beyond-All-Reason\Beyond-All-Reason.exe" %*
endlocal
```

### Windows — permanent, via setx

If you would rather set it once and launch BAR normally afterwards:

```bat
setx PRD_RAPID_REPO_MASTER "http://your-proxy:8080/repos.gz"
setx PRD_HTTP_SEARCH_URL "http://your-proxy:8080/find"
```

`setx` does **not** affect already-running processes. Open a new shell, or
log out and back in, before launching the game.

To undo it:

```bat
setx PRD_RAPID_REPO_MASTER ""
setx PRD_HTTP_SEARCH_URL ""
```

Or through the GUI: Start → "Edit environment variables for your account".

### Linux / macOS — launcher script

Use `scripts/bar-proxy.sh`:

```sh
chmod +x scripts/bar-proxy.sh
PROXY=http://your-proxy:8080 ./scripts/bar-proxy.sh
```

Or edit the `PROXY` line inside it and just run `./scripts/bar-proxy.sh`. If
it cannot find the launcher, pass it explicitly:

```sh
BAR_EXE=/path/to/Beyond-All-Reason ./scripts/bar-proxy.sh
```

The equivalent minimal script:

```sh
#!/bin/sh
export PRD_RAPID_REPO_MASTER=http://your-proxy:8080/repos.gz
export PRD_HTTP_SEARCH_URL=http://your-proxy:8080/find
exec ./Beyond-All-Reason "$@"
```

For a permanent setting, put those two `export` lines in `~/.profile` (or
`~/.zshrc`) and log back in.

### Steam

Set the two variables in the game's launch options, with `%command%` at the
end:

```
PRD_RAPID_REPO_MASTER=http://your-proxy:8080/repos.gz PRD_HTTP_SEARCH_URL=http://your-proxy:8080/find %command%
```

### Confirm it is actually being used

Start a download in the game, then on the proxy host:

```sh
curl http://your-proxy:8080/stats
```

The counters should move. With `BARPROXY_VERBOSE=1` the server also logs
every request, so you can watch pool files being pulled in real time.

---

## Server environment variables

All server configuration is environment-driven. `.env.example` has the same
list ready to copy.

| Variable | Default | What it does |
| --- | --- | --- |
| `BARPROXY_PUBLIC_URL` | *(empty)* | URL peers reach the proxy on. Baked into rewritten `repos.gz` and `/find`. Empty = derive per-request from the `Host` header. **The one setting you must get right.** |
| `BARPROXY_ADDR` | `:8080` | Listen address. `127.0.0.1:8080` to bind loopback only. |
| `BARPROXY_CACHE_DIR` | `./cache` | Where cached pool files and archives live. `/var/cache/barproxy` in the container. |
| `BARPROXY_CACHE_MAX_BYTES` | `0` | Evict least-recently-written objects above this size. `0` = unlimited. `53687091200` = 50 GiB. |
| `BARPROXY_PARTS` | `4` | Parallel range requests per object. The main speed lever against a throttling edge. |
| `BARPROXY_PART_MIN_SIZE` | `8388608` | Objects smaller than this (8 MiB) are fetched in one stream. |
| `BARPROXY_MUTABLE_TTL` | `5m` | How long mutable control files (`repos.gz`, `versions.gz`) stay cached. Go duration syntax. |
| `BARPROXY_ALLOW` | *(empty)* | Extra upstream hosts to allow, comma separated. A leading dot allows a whole domain. |
| `BARPROXY_UPSTREAM_MASTER` | `https://repos-cdn.beyondallreason.dev/repos.gz` | Upstream rapid master. |
| `BARPROXY_UPSTREAM_FIND` | `https://files-cdn.beyondallreason.dev/find` | Upstream search API. |
| `BARPROXY_VERBOSE` | `0` | `1` enables per-request logging. |

## Client environment variables

Read by pr-downloader, not by this proxy.

| Variable | Stock default | Set it to |
| --- | --- | --- |
| `PRD_RAPID_REPO_MASTER` | `https://repos-cdn.beyondallreason.dev/repos.gz` | `http://your-proxy:8080/repos.gz` |
| `PRD_HTTP_SEARCH_URL` | `https://files-cdn.beyondallreason.dev/find` | `http://your-proxy:8080/find` |

### On the rapid streamer

pr-downloader's streamer mode sends a `POST` to `<repo>/streamer.cgi` with a
bitmap of the pool files it wants, and gets back one concatenated blob. The
proxy relays those POSTs correctly (`X-BAR-Cache: BYPASS`) — the network
path fix still applies — but a per-client blob cannot be cached or shared.

If several people use one proxy, individual pool fetches cache and dedupe
far better. pr-downloader has a knob for this, but it is **not documented in
the upstream README** and I have not verified the exact name against a
build, so check yours before relying on it:

```sh
pr-downloader --help | grep -i stream
```

Leave streamer mode alone if you are the only user — it is fewer round trips.

---

## Security

The proxy will only fetch from an allowlist — `*.beyondallreason.dev` plus
the springrts hosts — and returns `403` for anything else. That is what
stops it becoming an open relay for strangers on the internet. Widen it with
`BARPROXY_ALLOW` only if you know why.

There is no authentication. If you expose it publicly, put it behind a
reverse proxy with whatever access control you want, and set
`BARPROXY_PUBLIC_URL` to the external URL.

## Limits

- A ranged request that misses the cache is passed straight through and not
  cached; only whole-object fetches populate the cache.
- Cache eviction is least-recently-written, swept every 30 minutes, not a
  true LRU.
- `/stats` counts a "miss" per request that was not served from disk, so
  clients that coalesce onto one upstream transfer each count separately.
