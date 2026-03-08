DTB_FILE := bcm2710-rpi-zero-2-w.dtb
TMP := .tmp
OUT := out

# Ensure required variables are set
REQUIRED_VARS := RPI_FIRMWARE_TAG SDCARD_LABEL UMTPRD_PATH AP_SSID AP_PSK
ifndef BOOT_SERVER_SDCARD
REQUIRED_VARS += BOOT_SERVER_IP BOOT_SERVER_PORT
SDCARD_SIZE_MB := 128
else
SDCARD_SIZE_MB := 64
endif

$(foreach v,$(REQUIRED_VARS),$(if $(strip $($v)),,$(error $v is unset)))

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

ifdef BOOT_SERVER_SDCARD
PODMAN_BUILD_INITRAMFS := $(PODMAN_BUILD) --build-arg BOOT_MODE=sdcard
else
PODMAN_BUILD_INITRAMFS := $(PODMAN_BUILD)
endif

$(TMP)/initramfs-rpi $(TMP)/vmlinuz-rpi &: Dockerfile.initramfs patch-init.awk $(TMP)/initramfs
	@mkdir -p $(TMP)
	$(PODMAN_BUILD_INITRAMFS) --output type=local,dest=$(TMP) -f Dockerfile.initramfs .

$(APKOVL): Dockerfile.apkovl $(TMP)/apkovl
	@mkdir -p $(OUT)
	$(PODMAN_BUILD) --output type=local,dest=$(OUT) -f Dockerfile.apkovl .

$(TMP)/apkovl: $(shell find apkovl -type f 2>/dev/null) $(UMTPRD_PATH) $(shell find drop-portal -type f 2>/dev/null)
	@rm -rf $@
	@mkdir -p $(dir $@)
	@cp -a apkovl $@
	@find $@ -type f -exec perl -pi -e 's/@([a-zA-Z0-9_]+)@/$$ENV{$$1} \/\/ $$&/eg' {} +
	@mkdir -p $@/usr/bin
	@install -m755 $(UMTPRD_PATH) $@/usr/bin/umtprd
	@install -m755 $$(nix build .#drop-portal --print-out-paths --no-link)/bin/drop-portal $@/usr/bin/drop-portal

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
ifdef BOOT_SERVER_SDCARD
	@echo "console=tty1 console=ttyAMA0,115200 modules=loop,squashfs,sd-mod,usb-storage loglevel=7" > $@
else
	@echo "console=tty1 console=ttyAMA0,115200 apkovl=http://$(BOOT_SERVER_IP):$(BOOT_SERVER_PORT)/$(APKOVL_NAME) modules=loop,squashfs,sd-mod,usb-storage loglevel=7" > $@
endif

$(TMP)/.mtoolsrc:
	@mkdir -p $(dir $@)
	@echo "drive r:" > $@
	@echo "    file=\"$(SDCARD_IMG)\"" >> $@
	@echo "    offset=$$((2048 * 512))" >> $@
	@echo "    mformat_only" >> $@

$(TMP)/%:
	@mkdir -p $(dir $@)
	wget -q -O $@ $(RPI_FW_BASE)/$*

ifdef BOOT_SERVER_SDCARD
SDCARD_IMG_DEPS := $(APKOVL)
endif

$(SDCARD_IMG): $(TMP_BLOBS) $(TMP_OVERLAYS) $(TMP_SYSTEM) $(KERNEL_FILES) $(TMP)/.mtoolsrc $(SDCARD_IMG_DEPS)
	@rm -f $@
	@mkdir -p $(OUT)
	dd if=/dev/zero of=$@ bs=1M count=$(SDCARD_SIZE_MB) status=progress
	parted -s $@ mklabel msdos mkpart primary fat32 1MiB 100% set 1 boot on
	mkfs.fat -F 32 --offset 2048 -n "$(SDCARD_LABEL)" $@
	mcopy -o $(TMP_BLOBS) $(TMP_SYSTEM) $(KERNEL_FILES) r:/
ifdef BOOT_SERVER_SDCARD
	mcopy -o $(APKOVL) r:/
endif
	mmd r:/overlays
	mcopy -o $(TMP_OVERLAYS) r:/overlays/

clean:
	rm -rf $(TMP) $(OUT)
