#!/bin/sh
# Example Armada install hook for a tarball-packaged service.
#
# Each version is unpacked into /opt/myapp/releases/<version> and the
# /opt/myapp/current symlink is switched atomically, so rollback is just
# pointing the symlink back.
#
# Armada provides: ARMADA_PACKAGE, ARMADA_VERSION, ARMADA_ARTIFACT,
#                  ARMADA_PREVIOUS_VERSION, ARMADA_PREVIOUS_ARTIFACT
set -eu

APP=/opt/myapp
dest="$APP/releases/$ARMADA_VERSION"

mkdir -p "$dest"
tar -xzf "$ARMADA_ARTIFACT" -C "$dest"

ln -sfn "$dest" "$APP/current.tmp"
mv -T "$APP/current.tmp" "$APP/current"
echo "$ARMADA_VERSION" > "$APP/VERSION"

systemctl restart myapp
