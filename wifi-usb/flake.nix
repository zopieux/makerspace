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
          umtprd = pkgs.pkgsCross.aarch64-multiplatform-musl.stdenv.mkDerivation {
            pname = "umtprd";
            version = "1.8.1";
            src = pkgs.fetchFromGitHub {
              owner = "viveris";
              repo = "uMTP-Responder";
              rev = "umtprd-1.8.1";
              hash = "sha256-kZXuEgyxNHKbuZjoMpOVjt6ygiar73/C1FsF942pjFM=";
            };
            makeFlags = [
              "CC=${pkgs.pkgsCross.aarch64-multiplatform-musl.stdenv.cc.targetPrefix}gcc"
              "LDFLAGS=-static -lpthread -lrt"
            ];
            installPhase = ''
              install -Dm755 umtprd $out/bin/umtprd
            '';
          };

          usb-gadget = pkgs.pkgsCross.aarch64-multiplatform-musl.buildGoModule {
            pname = "usb-gadget";
            version = "0.0.1";
            src = ./usb-gadget;
            vendorHash = "sha256-Db09ftEG9DJgN6mb4LaA2cOGiOjQx36DzeDqzAik2Fs=";
            subPackages = [ "cmd/gadget-web" "cmd/gadget-ha-rclone" ];
            env = { CGO_ENABLED = "0"; };
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
            UMTPRD_PATH = "${self.packages.${system}.umtprd}/bin/umtprd";
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
