#!/usr/bin/env bash
set -euo pipefail

declare -r DTB_FILE="bcm2710-rpi-zero-2-w.dtb"
declare -r ALPINE_ARCH="aarch64"
declare -r RPI_FW_BASE="https://github.com/raspberrypi/firmware/raw/${RPI_FIRMWARE_TAG}/boot"
declare -r DIST=$(realpath ./dist)
declare -r SDCARD_SIZE_MB=64

declare -r WORKDIR=$(mktemp -d)
trap 'rm -rf -- "$WORKDIR"' EXIT

log()  { echo -e "\n\033[1;34m>>> $*\033[0m"; }

for blob in bootcode.bin start.elf fixup.dat ${DTB_FILE}; do
    if [[ ! -f "${WORKDIR}/${blob}" ]]; then
        wget -q -O "${WORKDIR}/${blob}" "${RPI_FW_BASE}/${blob}"
    fi
done

mkdir -p -- "${WORKDIR}/overlays"
for overlay in miniuart-bt.dtbo disable-bt.dtbo dwc2.dtbo; do
    if [[ ! -f "${WORKDIR}/overlays/${overlay}" ]]; then
        wget -q -O "${WORKDIR}/overlays/${overlay}" "${RPI_FW_BASE}/overlays/${overlay}"
    fi
done

cat > "${WORKDIR}/config.txt" <<CONFIG
arm_64bit=1
kernel=vmlinuz-rpi
initramfs initramfs-rpi followkernel
gpu_mem=64
enable_uart=1
uart_2ndstage=1
dtoverlay=miniuart-bt
dtoverlay=dwc2
dtoverlay=disable-bt
device_tree=${DTB_FILE}
CONFIG

cat > "${WORKDIR}/cmdline.txt" <<CMDLINE
console=tty1 console=ttyAMA0,115200 apkovl=http://${BOOT_SERVER_IP}:${BOOT_SERVER_PORT}/cnc-usb.apkovl.tar.gz modules=loop,squashfs,sd-mod,usb-storage loglevel=7
CMDLINE

log "Creating SD card image (${SDCARD_SIZE_MB} MB FAT32)"

rm -f "${SDCARD_IMG}"
dd if=/dev/zero of="${SDCARD_IMG}" bs=1M count="${SDCARD_SIZE_MB}" status=progress

parted -s "${SDCARD_IMG}" \
    mklabel msdos \
    mkpart primary fat32 1MiB 100% \
    set 1 boot on

LOOP_OFFSET=$((2048 * 512))

mkfs.fat -F 32 \
    --offset 2048 \
    -n "${SDCARD_LABEL}" \
    "${SDCARD_IMG}"

MTOOLS_RC="${WORKDIR}/.mtoolsrc"
cat > "${MTOOLS_RC}" <<MTOOLS
drive r:
    file="${SDCARD_IMG}"
    offset=${LOOP_OFFSET}
    mformat_only
MTOOLS

export MTOOLSRC="${MTOOLS_RC}"

log "Copying files to SD card image"
for f in \
    "${WORKDIR}/bootcode.bin" \
    "${WORKDIR}/start.elf" \
    "${WORKDIR}/fixup.dat" \
    "${WORKDIR}/${DTB_FILE}" \
    "${WORKDIR}/config.txt" \
    "${WORKDIR}/cmdline.txt" \
    "${DIST}/vmlinuz-rpi" \
    "${DIST}/initramfs-rpi"
do
    mcopy -o "$f" "r:/"
done

mmd "r:/overlays"
for f in "${WORKDIR}/overlays/"*.dtbo; do
    mcopy -o "$f" "r:/overlays/"
done

log "SD card image ready: ${SDCARD_IMG} ($(du -sh "${SDCARD_IMG}" | cut -f1))"
