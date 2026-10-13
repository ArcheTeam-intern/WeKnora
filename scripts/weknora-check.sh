#!/usr/bin/env bash
# weknora-check: Linux verification entry point for the WeKnora-Agent prototype.
#
# The plan requires this runner to cover the new MCP tools, the user-identity
# middleware, the endpoint service, the routes and the related types, with the
# sqlite_fts5 build tag enabled (the prototype's keyword index depends on it).
#
# Usage (inside the weknora-check container):
#   /app/scripts/weknora-check.sh
#
# Environment:
#   WEKNORA_CHECK_PKGS  space-separated package list override
#   WEKNORA_CHECK_TAGS  build tags (default: sqlite_fts5)
#   WEKNORA_CHECK_EXTRA extra `go test` flags
set -uo pipefail

cd /app

tags="${WEKNORA_CHECK_TAGS:-sqlite_fts5}"
extra="${WEKNORA_CHECK_EXTRA:-}"

default_pkgs=(
  ./internal/mcpserver/...
  ./internal/middleware/...
  ./internal/application/service/...
  ./internal/application/repository/retriever/sqlite/...
  ./internal/router/...
  ./internal/types/...
)
# shellcheck disable=SC2206
pkgs=(${WEKNORA_CHECK_PKGS:-${default_pkgs[@]}})

echo "== weknora-check =="
echo "build tags : ${tags}"
echo "packages   : ${pkgs[*]}"
echo

echo "-- building all packages with tags=${tags} (this also proves the tree compiles) --"
if ! go build -tags "$tags" ./...; then
  echo "FAIL: go build -tags $tags ./..."
  exit 1
fi
echo "ok: full build"

failures=0
# shellcheck disable=SC2086
for pkg in "${pkgs[@]}"; do
  echo
  echo "-- go test -tags ${tags} -count=1 ${pkg} --"
  if ! go test -tags "$tags" -count=1 $extra "$pkg"; then
    echo "FAIL: $pkg"
    failures=$((failures + 1))
  fi
done

echo
if [ "$failures" -ne 0 ]; then
  echo "weknora-check FAILED: ${failures} package(s) failed"
  exit 1
fi
echo "weknora-check PASSED"
