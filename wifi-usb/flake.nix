{
  description = "Alpine Raspberry Pi Zero 2 USB Gadget Image";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=54caed8f89e27a841ec890b7663f9a53b0e4e25c";
  };

  outputs = { self, nixpkgs }@inputs:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
    in
    {
      packages.${system} = {
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

        drop-portal = pkgs.pkgsCross.aarch64-multiplatform-musl.buildGoModule {
          pname = "drop-portal";
          version = "0.0.1";
          src = ./drop-portal;
          vendorHash = null;
          env = { CGO_ENABLED = "0"; };
          ldflags = [ "-s" "-w" "-extldflags '-static'" ];
        };
      };

      devShells.${system}.default = pkgs.mkShell {
        UMTPRD_PATH = "${self.packages.${system}.umtprd}/bin/umtprd";
        DROP_PORTAL_PATH = "${self.packages.${system}.drop-portal}/bin/drop-portal";

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
        ];
      };
    };
}