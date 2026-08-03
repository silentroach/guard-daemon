{
  description = "Необязательное локальное окружение разработки guard-daemon";

  inputs.nixpkgs.url = "git+https://github.com/NixOS/nixpkgs?ref=nixos-unstable&shallow=1";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      nodeArchives = {
        aarch64-darwin = {
          platform = "darwin-arm64";
          hash = "sha256-HWC3A/5dfnBySJvoGH9DDxoJWmWMMeXh4oEzGlhz+sM=";
        };
        aarch64-linux = {
          platform = "linux-arm64";
          hash = "sha256-cgHjoJ3IJbrFeGfIGRPiuPDvh9BMuQgq9M2oL2/z2Iw=";
        };
        x86_64-linux = {
          platform = "linux-x64";
          hash = "sha256-1sZk3z8/YUWOjCd1hVcTKFItcFFmcjp8eCOpJTpNFaA=";
        };
      };
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      devShells = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          nodeArchive = nodeArchives.${system};
          nodejsPinned = pkgs.stdenvNoCC.mkDerivation {
            pname = "nodejs";
            version = "24.18.1";
            src = pkgs.fetchurl {
              url = "https://nodejs.org/dist/v24.18.1/node-v24.18.1-${nodeArchive.platform}.tar.xz";
              hash = nodeArchive.hash;
            };
            nativeBuildInputs = pkgs.lib.optionals pkgs.stdenv.isLinux [ pkgs.autoPatchelfHook ];
            buildInputs = pkgs.lib.optionals pkgs.stdenv.isLinux [ pkgs.stdenv.cc.cc.lib ];
            installPhase = ''
              runHook preInstall
              mkdir -p "$out"
              cp -R bin include lib share "$out/"
              runHook postInstall
            '';
          };
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              actionlint
              foundry
              git
              gitleaks
              gnumake
              go_1_26
              govulncheck
              nodejsPinned
              shellcheck
              stdenv.cc
            ];
          };
        }
      );
    };
}
