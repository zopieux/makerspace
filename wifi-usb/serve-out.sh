#!/usr/bin/env nix-shell
#!nix-shell -i bash -p simple-http-server

set -euo pipefail

declare -r PORT="${1:-8080}"
declare -r ADDR="0.0.0.0"

sudo systemctl stop firewall.service || true
simple-http-server -i -p "$PORT" --ip "$ADDR" out
