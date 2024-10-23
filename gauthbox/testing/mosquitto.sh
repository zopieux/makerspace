#! /usr/bin/env nix-shell
#! nix-shell -i bash -p mosquitto

set -eu

CONF=$(mktemp)
ACL=$(mktemp)

function cleanup () {
    rm -f $CONF $ACL
}

trap cleanup EXIT

cat >$CONF <<EOF
listener 1883 0.0.0.0
persistence false

acl_file $ACL
allow_anonymous true

log_dest stderr
log_type all
EOF

cat >$ACL <<EOF
topic readwrite #
EOF

mosquitto -c $CONF
