#!/usr/bin/env bash
# Run the real installer test against the plugin directory. No files are written to the repo.
set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: install-smoke.sh <plugin-dir>" >&2
  exit 2
fi
DIR="$(cd "$1" && pwd)"
# Find the repo that contains desktop/bridge. The skill may be linked/copied outside it;
# in that case fail with an actionable message instead of running in the wrong cwd.
ROOT="${HAI_REPO:-}"
if [ -z "$ROOT" ]; then
  ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  while [ "$ROOT" != "/" ] && [ ! -d "$ROOT/desktop/bridge" ]; do
    ROOT="$(dirname "$ROOT")"
  done
fi
if [ ! -d "$ROOT/desktop/bridge" ]; then
  echo "install-smoke: cannot find the hai-harness repo (set HAI_REPO=/path/to/repo)" >&2
  exit 2
fi
cd "$ROOT"
go test ./desktop/bridge -run TestSurfaceInstallExternal -count=1 -v -args -dir="$DIR"
