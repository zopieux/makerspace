#!/usr/bin/env bash

set -euo pipefail

IMG="./out/sdcard.img"
if [ ! -f "$IMG" ]; then
    echo "Error: $IMG not found. Run 'nix build' first."
    exit 1
fi

# 1. Identify target device
USB_DEVICES=$(lsblk -dno NAME,SIZE,LABEL,FSTYPE,MODEL,TRAN | grep "usb" || true)

TARGET=""
if [ -n "${1:-}" ]; then
    TARGET="$1"
    # Even if provided, verify it's a USB device
    TRAN=$(lsblk -dno TRAN "$TARGET" 2>/dev/null || echo "unknown")
    if [[ "$TRAN" != *"usb"* ]]; then
        echo "WARNING: Target device '$TARGET' does not appear to be a USB device (Transport: $TRAN)."
        echo "This might be your INTERNAL DRIVE if you are not careful."
        printf "Are you ABSOLUTELY sure this is the intended target? [Type 'CONFIRM' to proceed] "
        read -r REALLY_SURE
        if [ "$REALLY_SURE" != "CONFIRM" ]; then
            echo "Aborted for safety."
            exit 1
        fi
    fi
elif [ -z "$USB_DEVICES" ]; then
    echo "No USB mass storage devices found automatically."
    echo "Please provide the device path (e.g., /dev/sda) as the first argument."
    exit 1
else
    COUNT=$(echo "$USB_DEVICES" | wc -l)
    if [ "$COUNT" -eq 1 ]; then
        NAME=$(echo "$USB_DEVICES" | awk '{print $1}')
        TARGET="/dev/$NAME"
    else
        echo "Multiple USB devices found:"
        echo "$USB_DEVICES" | awk '{printf "/dev/%-8s %-8s %-12s %-8s %s\n", $1, $2, $3, $4, $5}'
        echo ""
        echo "Please specify one (e.g., flash-sdcard /dev/sdb)"
        exit 1
    fi
fi

# 2. Show confirmation with details
INFO=$(lsblk -dno NAME,SIZE,LABEL,FSTYPE,MODEL "$TARGET" 2>/dev/null || echo "Unknown Device")
echo "---------------------------------------------------"
echo "Target Device: $TARGET"
echo "Device Info  : $INFO"
echo "Source Image : $IMG"
echo "---------------------------------------------------"
echo "WARNING: ALL DATA ON $TARGET WILL BE PERMANENTLY ERASED!"
printf "Are you sure you want to proceed? [y/N] "
read -r CONFIRM

if [[ ! "$CONFIRM" =~ ^[yY]$ ]]; then
    echo "Aborted."
    exit 1
fi

# 3. Flash
echo "Flashing..."
set -x
sudo dd if="$IMG" of="$TARGET" bs=4M status=progress conv=fsync
set +x
echo "Flashing complete. You can now safely remove the SD card."
