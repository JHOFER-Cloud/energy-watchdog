{
  description = "energy-watchdog";

  inputs.nixpkgs.url = "https://flakehub.com/f/JHOFER-Cloud/NixOS-nixpkgs/0.1.tar.gz";

  outputs = {nixpkgs, ...}: let
    inherit (nixpkgs) lib;

    systems = ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];
    forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

    # Derive the Go toolchain from the `go x.y` directive in go.mod,
    # e.g. "go 1.25.0" -> pkgs.go_1_25.
    goParts =
      lib.splitString "."
      (lib.removePrefix "go "
        (lib.findFirst (l: lib.hasPrefix "go " l) "go 0.0"
          (lib.splitString "\n" (builtins.readFile ./go.mod))));
    goAttr = "go_${lib.elemAt goParts 0}_${lib.elemAt goParts 1}";
    goFor = pkgs: pkgs.${goAttr};

    package = pkgs:
      pkgs.buildGoModule {
        pname = "energy-watchdog";
        version = "0.1.0";
        src = ./.;
        vendorHash = "sha256-g+yaVIx4jxpAQ/+WrGKxhVeliYx7nLQe/zsGpxV4Fn4=";
        go = goFor pkgs;
      };
  in {
    formatter = forAllSystems (pkgs: pkgs.alejandra);

    packages = forAllSystems (pkgs: {
      default = package pkgs;
    });

    checks = forAllSystems (pkgs: {
      # `nix flake check` builds this, which runs `go test ./...` in checkPhase.
      go-tests = (package pkgs).overrideAttrs {doCheck = true;};
    });

    devShells = forAllSystems (pkgs: {
      default = pkgs.mkShell {
        packages = [(goFor pkgs) pkgs.gopls pkgs.alejandra];
      };
    });
  };
}
