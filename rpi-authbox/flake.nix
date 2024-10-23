{
  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-24.05";
    buildroot-nix.url = "github:zopieux/nix-buildroot?ref=2024.08";
  };

  outputs =
    {
      self,
      nixpkgs,
      buildroot-nix,
      ...
    }@inputs:
    let
      pkgs = import nixpkgs { system = "x86_64-linux"; };
      buildrootRpi3B = buildroot-nix.lib.mkBuildroot {
        name = "rpi-authbox";
        inherit pkgs;
        extraHashes = {
          "stable_20240529.tar.gz" = "15wif7j86f0s76rzml8x3d081lnbnzf8rf3fqr2bl85pm4g8bb05";
        };
        src = ./.;
        defconfig = "rpi3b_defconfig";
        lockfile = ./buildroot.lock;
        nativeBuildInputs = with pkgs; [
          (lib.hiPrio gcc)
          bash
          bc
          cpio
          expat
          file
          flock
          getent
          git
          gnumake
          libxcrypt
          ncurses.dev
          perl
          pkg-config
          pkgs.autoPatchelfHook
          pkgs.gcc.cc.lib
          rsync
          unzip
          util-linux
          wget
          which
        ];
      };
    in
    {
      # nix build '.#lockFile' && cp result buildroot.lock
      packages.x86_64-linux.lockFile = buildrootRpi3B.lockFile;
      packages.x86_64-linux.default = buildrootRpi3B.build;
      devShells.x86_64-linux.default = buildrootRpi3B.devShell;
      formatter.x86_64-linux = pkgs.nixfmt-rfc-style;
    };
}
