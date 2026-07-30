{
  description = "Воспроизводимое окружение разработки guard-daemon";

  inputs.nixpkgs.url = "git+https://github.com/NixOS/nixpkgs?ref=nixos-unstable&shallow=1";

  outputs = { nixpkgs, ... }:
    let
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-darwin"
        "x86_64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      devShells = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              actionlint
              git
              gitleaks
              gnumake
              go_1_26
              govulncheck
              nodejs_24
              shellcheck
              stdenv.cc
            ];
          };
        }
      );
    };
}
