#!/bin/sh
# testusb — test different USB device presentation methods
# Usage: testusb <usb|cdrom|mtp>
set -eu

GADGET=/sys/kernel/config/usb_gadget/g1
TESTDIR=/tmp/testusb
MTP_CONF=/etc/umtprd/umtprd.conf

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
    # Kill any running umtprd
    killall umtprd 2>/dev/null || true
    # Unmount ffs
    umount /dev/ffs-mtp 2>/dev/null || true
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

mode_mtp() {
    echo "=== MTP (Media Transfer Protocol) ==="
    prepare_test_files

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
    echo 0x0100 > idProduct   # PTP Gadget
    echo 0x0100 > bcdDevice
    echo 0x0200 > bcdUSB
    echo 0x06   > bDeviceClass     # Image
    echo 0x01   > bDeviceSubClass  # Still Imaging
    echo 0x01   > bDeviceProtocol
    mkdir -p strings/0x409
    echo "0123456789"  > strings/0x409/serialnumber
    echo "TestUSB"     > strings/0x409/manufacturer
    echo "PiZero2"     > strings/0x409/product
    mkdir -p configs/c.1/strings/0x409
    echo "MTP"  > configs/c.1/strings/0x409/configuration
    echo 250     > configs/c.1/MaxPower

    # Create FunctionFS instance for MTP
    mkdir -p functions/ffs.mtp
    ln -sf functions/ffs.mtp configs/c.1/

    # Mount FunctionFS
    mkdir -p /dev/ffs-mtp
    mount -t functionfs mtp /dev/ffs-mtp

    # Write umtprd config
    mkdir -p "$(dirname "$MTP_CONF")"
    cat > "$MTP_CONF" <<EOF
loop_on_disconnect 1
storage "$TESTDIR" "Test Files" "ro"
manufacturer "TestUSB"
product "PiZero2 MTP"
serial "0123456789"
firmware_version "1.0"
mtp_extensions "microsoft.com: 1.0; android.com: 1.0;"
interface "MTP"
usb_vendor_id  0x1D6B
usb_product_id 0x0100
usb_class 0x6
usb_subclass 0x1
usb_protocol 0x1
usb_dev_version 0x3008
usb_functionfs_mode 0x1
usb_dev_path   "/dev/ffs-mtp/ep0"
usb_epin_path  "/dev/ffs-mtp/ep1"
usb_epout_path "/dev/ffs-mtp/ep2"
usb_epint_path "/dev/ffs-mtp/ep3"
usb_max_packet_size 0x200
EOF

    # Start umtprd in background — it will write USB descriptors to ep0
    umtprd -conf "$MTP_CONF" &
    sleep 1

    # Now bind to UDC (must happen AFTER umtprd has written descriptors)
    ls /sys/class/udc > "$GADGET/UDC"
    echo "Gadget bound to $(cat "$GADGET/UDC")"

    echo "MTP active. Files in $TESTDIR are visible via MTP."
    echo "Ctrl+C to stop."
    trap gadget_teardown INT TERM
    wait
}

# ── main ─────────────────────────────────────────────────────────────────────

case "${1:-}" in
    usb)   gadget_teardown; mode_usb   ;;
    cdrom) gadget_teardown; mode_cdrom ;;
    mtp)   gadget_teardown; mode_mtp   ;;
    *)     echo "Usage: testusb <usb|cdrom|mtp>" >&2; exit 1 ;;
esac
