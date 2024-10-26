# rpi-authbox

A Buildroot-based lightweight OS image that implements the makerspace authbox software for Raspberry Pis.

## Philosophy

* Fully stateless, read-only system: avoids SD card wear and bugs caused by lingering state.
* Net booted: the image is centrally distributed, no need to open the authbox to make changes.
* Lightweight: minimal Linux system that boots quickly, without bloat.
* Built in a reproducible way: no random state.
* Minimal amounts of customizations compared to mainline Buildroot.
* Run on RPI 3B+.

## Features

* Main purpose is to run the gauthbox package (authbox automation in Go).
* The system is distributed as a folder meant to be server over TFTP. No NFS.
* The SD card is mounted read-only for local config fallback.
* Built around a few systemd units.
* Read-only, in-memory rootfs.
* Configured by DHCP, including hostname.
* NTP synchronization at boot. Main services wait for the system time to be updated.
* System log (journald) is uploaded to a centralized destination over the network.
* As of 2024, the kernel image is 19MB and the compressed rootfs is 20MB. Mandatory Pi boot files notwithstanding, **the entire system is just under 40MB**.
* A GitHub Workflow builds the system reproducibly, from source. Check the [releases](https://github.com/zopieux/makerspace/releases/).

## Building

### With Nix

> [!WARNING]  
> Sadly, this is not working yet because the sandbox prevents privileged operations like creating suid binaries.

```bash
$ nix build
```

### On a Debian-like system

You might need to tweak the ncurses version.

```bash
$ DEBIAN_FRONTEND=noninteractive sudo apt-get install -y \
    gcc bash bc cpio libexpat1-dev file util-linux passwd git make \
    libncurses5-dev perl pkg-config libgcc-12-dev rsync unzip util-linux \
    wget 7zip golang
$ ( cd ../gauthbox && rm -rf vendor && go mod vendor )
$ mkdir -p .br/{dl,build}
$ export BR2_DL_DIR=$PWD/.br/dl
$ git clone --depth=1 --branch=2024.08 https://gitlab.com/buildroot.org/buildroot.git $PWD/.br/buildroot
$ make BR2_EXTERNAL=$PWD O=$PWD/.br/build -C $PWD/.br/buildroot rpi3b_defconfig
$ make BR2_EXTERNAL=$PWD O=$PWD/.br/build -C $PWD/.br/buildroot rpi-authbox.7z
```

## Flashing a minimal netboot SD card

In an ideal world, the Raspberry Pi 3B+ would need _no SD card whatsoever_ and netboot on its own.
Sadly, Pi ≤3B+ boot ROM is half-arsed crapware and its netboot "feature" is highly unreliable. Indeed:

* It only attempts to get a DHCP lease a limited number of times, with a few seconds of delay, then stops and effectively becomes [an idle brick until power-cycled](https://github.com/raspberrypi/documentation/blob/develop/documentation/asciidoc/computers/raspberry-pi/boot-net.adoc#dhcp-requests-time-out-after-five-tries).
* More importantly, it seems that the netbooting only kicks in if the Ethernet cable is plugged/negotiated right _after_ power-on. In a PoE situation where "having power" happens at the _same time_ as "having eth connectivity", the boot ROM does not send any DHCP request and just idles. Of course, this is not documented.

Therefore, the only reliable way to netboot is to flash an SD card with the less crappy `bootcode.bin` provided by the Raspberry folks. This one implements a completely different, much more reliable netboot which attempts to get a DHCP lease _in perpetuity_ until it succeeds, which is exactly what we want.

We'll therefore need to flash the ~50KB `bootcode.bin` on an otherwise empty SD card.

Since a SD card _has_ to be used to workaround crap silicon, the gauthbox software will also use it to read its fallback "local" configuration. This is optional but recommended.

Assuming the SD card is `/dev/sda`. The `AUTHBOX` label is important.

```shell
# /!\ IRREVERSIBLY DELETES EVERYTHING ON /dev/sda
$ echo ',,c' | sudo sfdisk /dev/sda && sync && sudo mkfs.vfat -F32 -n 'AUTHBOX' /dev/sda1
$ sudo mount /dev/sda1 /mnt
$ curl 'https://raw.githubusercontent.com/raspberrypi/firmware/refs/heads/master/boot/bootcode.bin' | sudo tee >/dev/null /mnt/bootcode.bin
# Optionally, a local config (fallback if HTTP config fails):
$ sudoedit /mnt/authbox.config.json
$ sudo umount /mnt
```
