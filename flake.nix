{
  description = "s3site - serve static websites from tar.gz archives in S3-compatible storage";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages.default = pkgs.buildGoModule {
          pname = "s3site";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-JvyCQOq7D8V4BqqT3Ykk4BisIHbgCw7fR1epAOK/G9E=";
          subPackages = [ "cmd/s3site" ];
          ldflags = [ "-s" "-w" ];
          meta = with pkgs.lib; {
            description = "Serve static websites from tar.gz archives in S3-compatible storage";
            homepage = "https://github.com/rhnvrm/s3site";
            license = licenses.bsd2;
            mainProgram = "s3site";
            platforms = platforms.linux ++ platforms.darwin;
          };
        };

        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/s3site";
        };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gotools
            just
          ];
        };
      }
    );
}
