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
          pi-bridge = import ./nix/checks/pi_bridge.nix {inherit pkgs;};
          flow-bundle = import ./nix/checks/flow-bundle.nix {inherit pkgs;};
          priors = config.packages.priors;
          priors-pinned = import ./nix/checks/priors-pinned.nix {
            inherit pkgs;
            priors = config.packages.priors;
          };
        };

        treefmt = {
          projectRootFile = "flake.nix";
          programs = {
            alejandra.enable = true;
            gofmt.enable = true;
          };
        };

        packages = let
          vendorHash = "sha256-7WjrSgJ3hDpt+YTUZpmB5O6iWnQEw/trYARrbmGFWDg=";
          hookyard = pkgs.buildGoModule {
            pname = "hookyard";
            version = "0.1.0";
            src = ./.;
            inherit vendorHash;
            subPackages = ["cmd/hookyard"];
            # dispatch_e2e_test.go asks `ps -o sid=` whether a dispatched
            # handler got a session of its own. stdenv has no ps, so without
            # this the probe writes an empty file and the one assertion that
            # pins Setsid fails in the sandbox while passing in a devshell.
            nativeCheckInputs = [pkgs.procps];
            meta = {
              description = "Register agent hooks once, route them to every coding agent";
              mainProgram = "hookyard";
            };
          };
          # The memory layer's CLI and session_start handler
          # (docs/design/memory-layer.md §5).
          priors = let
            tools = "github.com/noamsto/hookyard/cmd/priors/internal/tools";
            # The tests exec these; the binary gets them as pinned paths.
            runtimeDeps = [pkgs.git pkgs.ripgrep pkgs.betterleaks pkgs.openssh];
          in
            pkgs.buildGoModule {
              pname = "priors";
              version = "0.1.0";
              src = ./.;
              inherit vendorHash;
              subPackages = ["cmd/priors"];
              # Pinned so that neither the config nor PATH can swap them.
              ldflags = [
                "-X ${tools}.Git=${pkgs.git}/bin/git"
                "-X ${tools}.SSH=${pkgs.openssh}/bin/ssh"
                "-X ${tools}.Rg=${pkgs.ripgrep}/bin/rg"
                "-X ${tools}.Scanner=${pkgs.betterleaks}/bin/betterleaks"
              ];
              nativeCheckInputs = runtimeDeps;
              # subPackages alone would test only cmd/priors; the gates,
              # routing and store live in its internal packages.
              preCheck = ''
                subPackages=cmd/priors/...
              '';
              meta = {
                description = "Cross-harness agent memory: markdown fact stores, write gates, tier-1 index and search";
                mainProgram = "priors";
              };
            };
        in {
          default = hookyard;
          inherit priors;
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
          nilaway = {
            enable = true;
            name = "nilaway";
            entry = "${pkgs.nilaway}/bin/nilaway -include-pkgs=github.com/noamsto/hookyard ./...";
            language = "system";
            types = ["go"];
            pass_filenames = false;
            stages = ["pre-push"];
          };
          statix.enable = true;
          deadnix.enable = true;
          alejandra.enable = true;
          typos.enable = true;
          check-merge-conflicts.enable = true;
          trim-trailing-whitespace = {
            enable = true;
            # Committed esbuild output, not hand-edited prose.
            excludes = ["^internal/serve/assets/flow/"];
          };
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
              pkgs.nilaway
              pkgs.d2
              # For running the bridge's `node --check` by hand, and for the
              # bridge runtime tests, which skip when node is absent.
              pkgs.nodejs
              # priors' search backend and write-path secret scanner.
              pkgs.ripgrep
              pkgs.betterleaks
              config.treefmt.build.wrapper
            ];
        };
      };
    });
}
