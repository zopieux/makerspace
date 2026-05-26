#!/bin/sh
# testusb — test different USB device presentation methods
# Usage: testusb <usb|cdrom>
set -eu

GADGET=/sys/kernel/config/usb_gadget/g1
TESTDIR=/tmp/testusb

die() { echo "ERROR: $*" >&2; exit 1; }

# ── helpers ──────────────────────────────────────────────────────────────────

gadget_teardown() {
    # Unbind UDC
    echo "" > "$GADGET/UDC" 2>/dev/null || true
    # Remove function links from config
    for l in "$GADGET"/configs/c.1/*; do
        [ -L "$l" ] && rm -f "$l"
    done
    # Remove functions
    for d in "$GADGET"/functions/*; do
        [ -d "$d" ] && rmdir "$d" 2>/dev/null || true
    done
    # Remove config strings, config
    rmdir "$GADGET"/configs/c.1/strings/0x409 2>/dev/null || true
    rmdir "$GADGET"/configs/c.1 2>/dev/null || true
    rmdir "$GADGET"/strings/0x409 2>/dev/null || true
    rmdir "$GADGET" 2>/dev/null || true
}

gadget_base() {
    modprobe dwc2 2>/dev/null || true
    modprobe libcomposite 2>/dev/null || true
    mount -t configfs none /sys/kernel/config 2>/dev/null || true

    # Wait for UDC
    local i=0
    while [ -z "$(ls /sys/class/udc 2>/dev/null)" ] && [ $i -lt 20 ]; do
        sleep 0.25; i=$((i + 1))
    done
    [ -z "$(ls /sys/class/udc 2>/dev/null)" ] && die "No UDC found"

    mkdir -p "$GADGET"
    cd "$GADGET"
    echo 0x1d6b > idVendor
    echo 0x0104 > idProduct
    echo 0x0100 > bcdDevice
    echo 0x0200 > bcdUSB
    echo 0xEF   > bDeviceClass
    echo 0x02   > bDeviceSubClass
    echo 0x01   > bDeviceProtocol
    mkdir -p strings/0x409
    echo "0123456789"  > strings/0x409/serialnumber
    echo "TestUSB"     > strings/0x409/manufacturer
    echo "PiZero2"     > strings/0x409/product
    mkdir -p configs/c.1/strings/0x409
    echo "Test Config" > configs/c.1/strings/0x409/configuration
    echo 250            > configs/c.1/MaxPower
}

gadget_bind() {
    ls /sys/class/udc > "$GADGET/UDC"
    echo "Gadget bound to $(cat "$GADGET/UDC")"
}

prepare_test_files() {
    rm -rf "$TESTDIR"
    mkdir -p "$TESTDIR"
    echo "Hello from PiZero2!" > "$TESTDIR/hello.txt"
    cat > "$TESTDIR/test.nc" <<'NC'
G21
G90
G0 X0 Y0
G1 X10 Y0 F300
G1 X10 Y10
G1 X0 Y10
G1 X0 Y0
M2
NC
}

# ── methods ──────────────────────────────────────────────────────────────────

mode_usb() {
    echo "=== USB Mass Storage (FAT) ==="
    prepare_test_files
    local img=/tmp/testusb.img
    truncate -s 4M "$img"
    mkfs.vfat -n TESTUSB "$img" >/dev/null
    mcopy -s -i "$img" "$TESTDIR"/* ::/

    gadget_base
    mkdir -p functions/mass_storage.usb0
    echo 1      > functions/mass_storage.usb0/stall
    echo 1      > functions/mass_storage.usb0/lun.0/removable
    echo 1      > functions/mass_storage.usb0/lun.0/ro
    echo "$img" > functions/mass_storage.usb0/lun.0/file
    ln -sf functions/mass_storage.usb0 configs/c.1/
    gadget_bind
    echo "USB mass storage active (read-only FAT). Ctrl+C to stop."
    trap gadget_teardown INT TERM
    echo "Waiting 10s then forcing media eject/re-insert..."
    sleep 10
    echo "Ejecting..."
    echo 1 > functions/mass_storage.usb0/lun.0/forced_eject
    sleep 1
    echo "Re-inserting..."
    echo "$img" > functions/mass_storage.usb0/lun.0/file
    echo "Media re-inserted. Waiting 30s..."
    sleep 30
    echo "Done. Ctrl+C to stop."
    while true; do sleep 60; done
}

mode_cdrom() {
    echo "=== USB CD-ROM (ISO9660) ==="
    prepare_test_files
    local iso=/tmp/testusb.iso
    genisoimage -quiet -V TESTUSB -r -J -o "$iso" "$TESTDIR"

    gadget_base
    mkdir -p functions/mass_storage.usb0
    echo 1      > functions/mass_storage.usb0/stall
    echo 1      > functions/mass_storage.usb0/lun.0/cdrom
    echo 1      > functions/mass_storage.usb0/lun.0/ro
    echo 1      > functions/mass_storage.usb0/lun.0/removable
    echo "$iso" > functions/mass_storage.usb0/lun.0/file
    ln -sf functions/mass_storage.usb0 configs/c.1/
    gadget_bind
    echo "USB CD-ROM active. Ctrl+C to stop."
    trap gadget_teardown INT TERM
    echo "Waiting 10s then forcing media eject/re-insert..."
    sleep 10
    echo "Ejecting..."
    echo 1 > functions/mass_storage.usb0/lun.0/forced_eject
    sleep 1
    echo "Re-inserting..."
    echo "$iso" > functions/mass_storage.usb0/lun.0/file
    echo "Media re-inserted. Waiting 30s..."
    sleep 30
    echo "Done. Ctrl+C to stop."
    while true; do sleep 60; done
}

# ── main ─────────────────────────────────────────────────────────────────────

case "${1:-}" in
    usb)   gadget_teardown; mode_usb   ;;
    cdrom) gadget_teardown; mode_cdrom ;;
    *)     echo "Usage: testusb <usb|cdrom>" >&2; exit 1 ;;
esac
