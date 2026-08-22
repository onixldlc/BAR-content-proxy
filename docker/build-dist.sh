#!/bin/sh
# Cross compile a release binary for every published target into /out.
#
# Runs inside the golang builder stage, once, on the build host's arch. Go's
# own cross compiler needs no C toolchain here: the proxy is CGO-free, so
# CGO_ENABLED=0 lets one builder emit every GOOS/GOARCH below.
#
# Naming is barproxy-<os>-<arch>[.exe]. The workflow repackages these into
# archives whose single member is a plain `barproxy`, so an extract drops the
# binary on PATH, not a long triple-suffixed name.
set -eu

VERSION="${VERSION:-dev}"
OUT=/out
mkdir -p "$OUT"

# GOOS/GOARCH pairs, space separated. Mirrors the reference repo's asset set:
# linux amd64/arm64/386, darwin amd64/arm64, windows amd64.
targets="linux/amd64 linux/arm64 linux/386 darwin/amd64 darwin/arm64 windows/amd64"

for t in $targets; do
	os="${t%/*}"
	arch="${t#*/}"
	name="barproxy-${os}-${arch}"
	ext=""
	if [ "$os" = "windows" ]; then
		ext=".exe"
	fi
	echo "building ${name}${ext} (version ${VERSION})"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		go build -trimpath -ldflags "-s -w" -o "${OUT}/${name}${ext}" .
done

echo "built:"
ls -l "$OUT"
