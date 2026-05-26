{
  description = "Alpine Raspberry Pi Zero 2 USB Gadget Image";

  nixConfig = {
    extra-substituters = [
      "https://nixos-raspberrypi.cachix.org"
    ];
    extra-trusted-public-keys = [
      "nixos-raspberrypi.cachix.org-1:4iMO9LXa8BqhU+Rpg6LQKiGa2lsNh/j2oiYLNOQ5sPI="
    ];
  };

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
    nixos-raspberrypi = {
      url = "github:nvmd/nixos-raspberrypi/main";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { self, nixpkgs, nixos-raspberrypi }@inputs:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
      nixpkgsFor = forAllSystems (system: import nixpkgs { inherit system; });
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = nixpkgsFor.${system};
        in
        {
          usb-gadget = pkgs.pkgsCross.aarch64-multiplatform-musl.buildGoModule {
            pname = "usb-gadget";
            version = "0.0.1";
            src = ./usb-gadget;
            vendorHash = "sha256-bBBesRtJIcxMb7on9ve9Qqi7poDvlINli8+p82WHsPw=";
            subPackages = [ "cmd/gadget-web" "cmd/gadget-ha-rclone" ];
            env = { CGO_ENABLED = "1"; };
            ldflags = [ "-s" "-w" "-extldflags '-static'" ];
          };

          zero2w = (nixos-raspberrypi.lib.nixosSystem {
            specialArgs = inputs // { inherit system; };
            modules = [ ./pizero2w.nix ];
          }).config.system.build.images.sd-card;
        }
      );

      defaultPackage = forAllSystems (system: self.packages.${system}.usb-gadget);

      devShells = forAllSystems (system:
        let
          pkgs = nixpkgsFor.${system};
        in
        {
          default = pkgs.mkShell {
            USB_GADGET_PATH = "${self.packages.${system}.usb-gadget}/bin/gadget-web";

            buildInputs = with pkgs; [
              apk-tools
              nix-prefetch-scripts
              jq
              wget
              mtools
              dosfstools
              parted
              openssl
              cpio
              gzip
              dtc
              go
              gopls

              qemu
            ];
          };
        }
      );
    };
}
