#!/bin/bash

set -eux

# Cleanup etc
rm -rf "${TARGET_DIR}/etc/init.d"
rm -rf "${TARGET_DIR}/etc/network"
rm -rf "${TARGET_DIR}/etc/X11"
rm -rf "${TARGET_DIR}/etc/xdg"

# Cleanup root
rm -rf "${TARGET_DIR}/media"
rm -rf "${TARGET_DIR}/srv"
rm -rf "${TARGET_DIR}/opt"

# Cleanup misc
rm -rf "${TARGET_DIR}/usr/lib/modules-load.d"
