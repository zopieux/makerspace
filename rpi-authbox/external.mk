include $(sort $(wildcard $(BR2_EXTERNAL_RPI_AUTHBOX_PATH)/package/*/*.mk))

rpi-authbox: all
	mkdir -p $(O)/rpi-authbox
	cp -r \
		$(BR2_EXTERNAL_RPI_AUTHBOX_PATH)/cmdline.txt \
		$(BR2_EXTERNAL_RPI_AUTHBOX_PATH)/config.txt \
		$(O)/images/Image \
		$(O)/images/rootfs.cpio.xz \
		$(O)/images/bcm*dtb \
		$(O)/images/rpi-firmware/fixup.dat \
		$(O)/images/rpi-firmware/start.elf \
		$(O)/images/rpi-firmware/bootcode.bin \
		$(O)/images/rpi-firmware/overlays \
		$(O)/rpi-authbox

rpi-authbox.7z: rpi-authbox
	7z a $(O)/rpi-authbox.7z $(O)/rpi-authbox/*
