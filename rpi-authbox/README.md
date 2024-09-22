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

```bash
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
