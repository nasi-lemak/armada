#!/bin/sh
# Example Armada rollback hook: point the symlink back at the previous release.
set -eu

APP=/opt/myapp
prev="$APP/releases/$ARMADA_PREVIOUS_VERSION"

[ -n "$ARMADA_PREVIOUS_VERSION" ] && [ -d "$prev" ] || {
  echo "no previous release to roll back to" >&2
  exit 1
}

ln -sfn "$prev" "$APP/current.tmp"
mv -T "$APP/current.tmp" "$APP/current"
echo "$ARMADA_PREVIOUS_VERSION" > "$APP/VERSION"

systemctl restart myapp
