### rpi-authbox

A Buildroot-based lightweight OS image that implements the makerspace authbox software for Raspberry Pis.

### Main goals and requirements

* Fully stateless, read-only system: avoids SD card wear and bugs caused by lingering state.
* Net booted: the image is centrally distributed, no need to open the authbox to make changes.
* Lightweight: minimal Linux system that boots quickly, without bloat.
* Built in a reproducible way: no random state.
* Minimal amounts of customizations compared to mainline Buildroot.
* Run on RPI 3B+.

### Building

#### With Nix

```bash
nix build
```

#### With Debian

Dependencies:

```
gcc
bash
bc
cpio
libexpat1-dev
file
util-linux
passwd
git
make
libxcrypt-dev
libncurses5-dev
perl
pkg-config
libgcc-12-dev
rsync
unzip
util-linux
wget
which
7zip
```

### Flashing a minimal netboot SD card

Assuming the SD card is `/dev/sda`. The `AUTHBOX` label is important.

```shell
$ echo ",,c" | sudo sfdisk /dev/sda && sync && sudo mkfs.vfat -F32 -n "AUTHBOX" /dev/sda1
$ wget https://raw.githubusercontent.com/raspberrypi/firmware/refs/heads/master/boot/bootcode.bin
$ sudo mount /dev/sda1 /mnt
$ sudo cp bootcode.bin /mnt
# Optionally, a local config (fallback for remote http config):
$ sudo cp authbox.config.json /mnt
$ sudo umount /mnt
```
