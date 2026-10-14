#!/bin/sh
set -eu
# Go creates this random token in the shared data volume before becoming ready.
for attempt in $(seq 1 60); do
  [ -s /data/control.token ] && break
  sleep 1
done
[ -s /data/control.token ] || { echo "Missing local control token" >&2; exit 1; }
token=$(cat /data/control.token)
case "$token" in *[!0-9a-f]*|'') echo "Invalid control token" >&2; exit 1;; esac
sed -i "s/__CONTROL_TOKEN__/$token/g" /etc/nginx/nginx.conf
