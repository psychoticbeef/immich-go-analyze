{
  description = "AI-powered keyword tagger for Immich using Ollama vision models";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
  let
    forAllSystems = nixpkgs.lib.genAttrs [
      "x86_64-linux"
      "aarch64-linux"
      "aarch64-darwin"
      "x86_64-darwin"
    ];
  in {
    packages = forAllSystems (system:
      let pkgs = nixpkgs.legacyPackages.${system};
      in {
        default = pkgs.buildGoModule {
          pname = "immich-go-analyze";
          version = self.shortRev or self.dirtyShortRev or "dev";
          src = ./.;
          vendorHash = "sha256-FxmWGkQM/NlYWxdWORpKr0sTLQ2lVpmSwqRc/EGfn7s=";
          meta.mainProgram = "immich-go-analyze";
        };
      }
    );
  };
}
