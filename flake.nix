{
  description = "Register agent hooks once, route them to every coding agent";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    git-hooks-nix = {
      url = "github:cachix/git-hooks.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    # Check-only dependency (nix/checks/hm-module.nix evaluates the module
    # against it); a consumer repo supplies its own home-manager (§9).
    home-manager = {
      url = "github:nix-community/home-manager";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = inputs @ {flake-parts, ...}:
    flake-parts.lib.mkFlake {inherit inputs;} ({withSystem, ...}: {
      imports = [
        inputs.treefmt-nix.flakeModule
        inputs.git-hooks-nix.flakeModule
      ];

      systems = ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];

      flake = {
        # nix-config uses `homeManagerModules.default` for nine of its twelve
        # flake-input module imports; `homeModules` is the newer canonical
        # spelling. Export both names under both attrsets so a consumer's
        # guess costs nothing.
        homeManagerModules = {
          default = inputs.self.homeManagerModules.hookyard;
          hookyard = import ./nix/hm-module.nix {
            # A consumer on a system outside `systems` above gets an eval
            # error from `withSystem`; `programs.hookyard.package` is the
            # escape hatch for that case.
            packageFor = system: withSystem system (perSystem: perSystem.config.packages.hookyard);
          };
        };
        homeModules = inputs.self.homeManagerModules;
      };

      perSystem = {
        pkgs,
        config,
        ...
      }: {
        checks = {
          hm-module = import ./nix/checks/hm-module.nix {
            inherit pkgs;
            inherit (inputs) home-manager;
            hookyardPackage = config.packages.hookyard;
          };
          diagrams = import ./nix/checks/diagrams.nix {inherit pkgs;};
        };

        treefmt = {
          projectRootFile = "flake.nix";
          programs = {
            alejandra.enable = true;
            gofmt.enable = true;
          };
        };

        packages = let
          hookyard = pkgs.buildGoModule {
            pname = "hookyard";
            version = "0.1.0";
            src = ./.;
            vendorHash = "sha256-pbA/AlBz3cQYRTMnQ/qBPcinYOKokrBLNhkbRTq54gE=";
            subPackages = ["cmd/hookyard"];
            meta = {
              description = "Register agent hooks once, route them to every coding agent";
              mainProgram = "hookyard";
            };
          };
        in {
          default = hookyard;
          # §9 names this attribute explicitly: nix-config takes it as the
          # home-manager module's default package, so "one input, one binary"
          # holds by construction rather than by the consumer wiring it up.
          inherit hookyard;
        };

        pre-commit.settings.hooks = {
          golangci-lint = {
            enable = true;
            stages = ["pre-push"];
          };
          gotest = {
            enable = true;
            stages = ["pre-push"];
          };
          statix.enable = true;
          deadnix.enable = true;
          alejandra.enable = true;
          typos.enable = true;
          check-merge-conflicts.enable = true;
          trim-trailing-whitespace.enable = true;
        };

        devShells.default = pkgs.mkShell {
          inherit (config.pre-commit) shellHook;
          packages =
            config.pre-commit.settings.enabledPackages
            ++ [
              pkgs.go
              pkgs.gopls
              pkgs.gotools
              pkgs.golangci-lint
              pkgs.d2
              config.treefmt.build.wrapper
            ];
        };
      };
    });
}
