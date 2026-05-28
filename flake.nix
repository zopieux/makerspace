{
  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [ "x86_64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
      nixpkgsFor = forAllSystems (system: import nixpkgs { inherit system; });
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = nixpkgsFor.${system};
          aarch64Pkgs = pkgs.pkgsCross.aarch64-multiplatform-musl;
        in
        {
          usb-gadget = aarch64Pkgs.buildGoModule {
            pname = "usb-gadget";
            version = "0.0.1";
            src = ./.;
            modRoot = "wifi-usb/usb-gadget";
            proxyVendor = true;
            vendorHash = "sha256-lhErjrnecb6qDF7SsdiNgvdP5gIHfqeLzQ+SBnlG2Pg=";
            subPackages = [ "cmd/gadget-ha-rclone" ];
            env = { CGO_ENABLED = "0"; };
            ldflags = [ "-s" "-w" "-extldflags '-static'" ];
          };

          gauthbox = pkgs.buildGoModule {
            pname = "gauthbox";
            version = "0.0.1";
            src = ./gauthbox;
            vendorHash = "sha256-Wb+/nhUEoCM2NZqxbE2ciPsmyhh5yTOMgxDiobOoHy4=";
            proxyVendor = true;
            subPackages = [ "cmd/config" "cmd/local" ];
            postInstall = ''
              mv $out/bin/config $out/bin/authbox_config
              mv $out/bin/local $out/bin/buttonless
            '';
          };
        }
      );

      devShells = forAllSystems (system:
        let
          pkgs = nixpkgsFor.${system};
        in
        {
          default = pkgs.mkShell {
            buildInputs = with pkgs; [
              go
              gopls
              go-tools
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
              qemu
            ];
          };
        }
      );
    };
}
