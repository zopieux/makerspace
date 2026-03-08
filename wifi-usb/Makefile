SDCARD_SIZE_MB := 64
DTB_FILE := bcm2710-rpi-zero-2-w.dtb
TMP := .tmp
OUT := out

# Ensure required variables are set
$(foreach v,RPI_FIRMWARE_TAG SDCARD_LABEL UMTPRD_PATH DROP_PORTAL_PATH BOOT_SERVER_IP BOOT_SERVER_PORT AP_SSID AP_PSK,$(if $(strip $($v)),,$(error $v is unset)))

RPI_FW_BASE := https://github.com/raspberrypi/firmware/raw/$(RPI_FIRMWARE_TAG)/boot

BLOBS := bootcode.bin start.elf fixup.dat $(DTB_FILE)
OVERLAYS := miniuart-bt.dtbo disable-bt.dtbo dwc2.dtbo

SDCARD_IMG := $(OUT)/sdcard.img
APKOVL_NAME := cnc-usb.apkovl.tar.gz
APKOVL := $(OUT)/$(APKOVL_NAME)
TMP_BLOBS := $(addprefix $(TMP)/,$(BLOBS))
TMP_OVERLAYS := $(addprefix $(TMP)/overlays/,$(OVERLAYS))
TMP_SYSTEM := $(TMP)/config.txt $(TMP)/cmdline.txt
KERNEL_FILES := $(TMP)/vmlinuz-rpi $(TMP)/initramfs-rpi

PODMAN_BUILD := podman build \
	--platform linux/arm64 \
	-v /nix/store:/nix/store:ro

export MTOOLSRC := $(TMP)/.mtoolsrc

.PHONY: all clean

all: $(SDCARD_IMG) $(APKOVL)

$(TMP)/initramfs-rpi $(TMP)/vmlinuz-rpi &: Dockerfile.initramfs patch-init.awk $(TMP)/initramfs
	@mkdir -p $(TMP)
	$(PODMAN_BUILD) --output type=local,dest=$(TMP) -f Dockerfile.initramfs .

$(APKOVL): Dockerfile.apkovl $(TMP)/apkovl
	@mkdir -p $(OUT)
	$(PODMAN_BUILD) --output type=local,dest=$(OUT) -f Dockerfile.apkovl .

$(TMP)/apkovl: $(shell find apkovl -type f 2>/dev/null) $(UMTPRD_PATH) $(DROP_PORTAL_PATH)
	@rm -rf $@
	@mkdir -p $(dir $@)
	@cp -a apkovl $@
	@find $@ -type f -exec perl -pi -e 's/@([a-zA-Z0-9_]+)@/$$ENV{$$1} \/\/ $$&/eg' {} +
	@mkdir -p $@/usr/bin
	@install -m755 $(UMTPRD_PATH) $@/usr/bin/umtprd
	@install -m755 $(DROP_PORTAL_PATH) $@/usr/bin/drop-portal

$(TMP)/initramfs: $(shell find initramfs -type f 2>/dev/null)
	@rm -rf $@
	@mkdir -p $(dir $@)
	@cp -a initramfs $@
	@find $@ -type f -exec perl -pi -e 's/@([a-zA-Z0-9_]+)@/$$ENV{$$1} \/\/ $$&/eg' {} +

$(TMP)/overlays/%:
	@mkdir -p $(dir $@)
	wget -q -O $@ $(RPI_FW_BASE)/overlays/$*

$(TMP)/config.txt:
	@mkdir -p $(dir $@)
	@echo "arm_64bit=1" > $@
	@echo "kernel=vmlinuz-rpi" >> $@
	@echo "initramfs initramfs-rpi followkernel" >> $@
	@echo "gpu_mem=64" >> $@
	@echo "enable_uart=1" >> $@
	@echo "uart_2ndstage=1" >> $@
	@echo "dtoverlay=miniuart-bt" >> $@
	@echo "dtoverlay=dwc2" >> $@
	@echo "dtoverlay=disable-bt" >> $@
	@echo "device_tree=$(DTB_FILE)" >> $@

$(TMP)/cmdline.txt:
	@mkdir -p $(dir $@)
	@echo "console=tty1 console=ttyAMA0,115200 apkovl=http://$(BOOT_SERVER_IP):$(BOOT_SERVER_PORT)/$(APKOVL_NAME) modules=loop,squashfs,sd-mod,usb-storage loglevel=7" > $@

$(TMP)/.mtoolsrc:
	@mkdir -p $(dir $@)
	@echo "drive r:" > $@
	@echo "    file=\"$(SDCARD_IMG)\"" >> $@
	@echo "    offset=$$((2048 * 512))" >> $@
	@echo "    mformat_only" >> $@

$(TMP)/%:
	@mkdir -p $(dir $@)
	wget -q -O $@ $(RPI_FW_BASE)/$*

$(SDCARD_IMG): $(TMP_BLOBS) $(TMP_OVERLAYS) $(TMP_SYSTEM) $(KERNEL_FILES) $(TMP)/.mtoolsrc
	@rm -f $@
	@mkdir -p $(OUT)
	dd if=/dev/zero of=$@ bs=1M count=$(SDCARD_SIZE_MB) status=progress
	parted -s $@ mklabel msdos mkpart primary fat32 1MiB 100% set 1 boot on
	mkfs.fat -F 32 --offset 2048 -n "$(SDCARD_LABEL)" $@
	mcopy -o $(TMP_BLOBS) $(TMP_SYSTEM) $(KERNEL_FILES) r:/
	mmd r:/overlays
	mcopy -o $(TMP_OVERLAYS) r:/overlays/

clean:
	rm -rf $(TMP) $(OUT)
