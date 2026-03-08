#!/sbin/openrc-run

name="usb-gadget"
description="Setup USB composite gadget (Mass Storage via NBD + Network)"

NBD_SERVER="${NBD_SERVER:-192.168.1.30}"
NBD_PORT="${NBD_PORT:-10809}"
NBD_DEVICE="/dev/nbd0"

depend() {
    need localmount
    need networking
    after bootmisc
}

start() {
    ebegin "Setting up USB Gadget"

    # Load required kernel modules
    modprobe dwc2 || true
    modprobe nbd || true
    modprobe libcomposite || true
    mount -t configfs none /sys/kernel/config 2>/dev/null || true

    # Wait for UDC to appear (dwc2 needs a moment to register)
    local i=0
    while [ -z "$(ls /sys/class/udc 2>/dev/null)" ] && [ $i -lt 20 ]; do
        sleep 0.25
        i=$((i + 1))
    done
    if [ -z "$(ls /sys/class/udc 2>/dev/null)" ]; then
        eerror "No UDC controller found — is dtoverlay=dwc2 in config.txt?"
        eend 1
        return 1
    fi

    # Connect to NBD server
    einfo "Connecting to NBD server ${NBD_SERVER}:${NBD_PORT}..."
    nbd-client "$NBD_SERVER" "$NBD_PORT" "$NBD_DEVICE" -persist
    if [ $? -ne 0 ]; then
        eerror "Failed to connect to NBD server"
        eend 1
        return 1
    fi

    mkdir -p /sys/kernel/config/usb_gadget/g1
    cd /sys/kernel/config/usb_gadget/g1

    echo 0x1d6b > idVendor  # Linux Foundation
    echo 0x0104 > idProduct # Multifunction Composite Gadget
    echo 0x0100 > bcdDevice
    echo 0x0200 > bcdUSB

    echo 0xEF > bDeviceClass
    echo 0x02 > bDeviceSubClass
    echo 0x01 > bDeviceProtocol

    mkdir -p strings/0x409
    echo "0123456789" > strings/0x409/serialnumber
    echo "Alpine" > strings/0x409/manufacturer
    echo "PiZero2" > strings/0x409/product

    mkdir -p configs/c.1/strings/0x409
    echo "Config 1: ECM + Mass Storage" > configs/c.1/strings/0x409/configuration
    echo 250 > configs/c.1/MaxPower

    # Networking (ECM)
    mkdir -p functions/ecm.usb0
    ln -sf functions/ecm.usb0 configs/c.1/

    # Mass Storage as CD-ROM backed by NBD
    mkdir -p functions/mass_storage.usb0
    echo 1 > functions/mass_storage.usb0/stall
    echo 1 > functions/mass_storage.usb0/lun.0/cdrom
    echo 1 > functions/mass_storage.usb0/lun.0/ro
    echo 1 > functions/mass_storage.usb0/lun.0/removable
    echo "$NBD_DEVICE" > functions/mass_storage.usb0/lun.0/file
    ln -sf functions/mass_storage.usb0 configs/c.1/

    # Bind to the UDC
    ls /sys/class/udc > UDC

    # Setup network interface and IP
    ip link set dev usb0 up
    ip addr add 10.10.20.1/24 dev usb0

    eend $?
}

stop() {
    ebegin "Stopping USB Gadget"
    echo "" > /sys/kernel/config/usb_gadget/g1/UDC 2>/dev/null
    rm -f /sys/kernel/config/usb_gadget/g1/configs/c.1/ecm.usb0
    rm -f /sys/kernel/config/usb_gadget/g1/configs/c.1/mass_storage.usb0
    rmdir /sys/kernel/config/usb_gadget/g1/configs/c.1/strings/0x409 2>/dev/null
    rmdir /sys/kernel/config/usb_gadget/g1/configs/c.1 2>/dev/null
    rmdir /sys/kernel/config/usb_gadget/g1/functions/ecm.usb0 2>/dev/null
    rmdir /sys/kernel/config/usb_gadget/g1/functions/mass_storage.usb0 2>/dev/null
    rmdir /sys/kernel/config/usb_gadget/g1/strings/0x409 2>/dev/null
    rmdir /sys/kernel/config/usb_gadget/g1 2>/dev/null
    nbd-client -d "$NBD_DEVICE" 2>/dev/null
    eend 0
}
