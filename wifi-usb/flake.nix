{
  description = "Alpine Raspberry Pi Zero 2 USB Gadget Image";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
  };

  outputs = { self, nixpkgs }@inputs:
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
            vendorHash = "sha256-8StURvZEOWX/Z5/Yo7DVR1QMoc/JspSJcuOxX/DL/b4=";
            subPackages = [ "cmd/gadget-web" "cmd/gadget-ha-rclone" ];
            env = { CGO_ENABLED = "1"; };
            ldflags = [ "-s" "-w" "-extldflags '-static'" ];
          };
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
            ];
          };
        }
      );
    };
}
