#!/usr/bin/env nix-shell
#!nix-shell -i bash -p nbd cdrkit inotify-tools

set -euo pipefail

declare -r PORT="${1:-10809}"
declare -r DIR="usbnbd"
declare -r IMG="$(pwd)/.usbnbd.iso"

mkdir -p "$DIR"

rebuild_image() {
    genisoimage -quiet -V PIUSB -r -J -o "$IMG" "$DIR"
    echo "[$(date +%H:%M:%S)] ISO rebuilt ($(du -sh "$IMG" | cut -f1))"
}

# Initial build
rebuild_image

# Start nbd-server in background
nbd-server -d "$PORT" "$IMG" -r &
NBD_PID=$!
trap 'kill $NBD_PID 2>/dev/null; exit' INT TERM

echo "Serving $DIR/ as ISO on NBD port $PORT (read-only, pid $NBD_PID)"
echo "Drop files into ./$DIR/ — ISO rebuilds automatically."

# Watch directory and rebuild on changes
while inotifywait -r -q -e modify,create,delete,move "$DIR"; do
    sleep 0.5
    rebuild_image
done
