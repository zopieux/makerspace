#!/usr/bin/env nix-shell
#!nix-shell -i bash -p podman

podman run --rm -p 1883:1883 -v "$PWD:/mosquitto/config:ro" eclipse-mosquitto:alpine
