# Evaluates the exported home-manager module against two scratch
# configurations and then executes the rendered install command. Two
# properties are silent when broken and nothing else in the repo checks
# either: the emitted router path must be the rebuild-stable *profile* path,
# never a store path, and `hookyard install` must be invoked exactly once
# across the whole merged activation script. Self-contained like
# ../hm-module.nix (see its header comment) — takes the package as an
# argument instead of importing flake.nix.
{
  pkgs,
  lib ? pkgs.lib,
  home-manager,
  hookyardPackage,
}: let
  hookyardModule = import ../hm-module.nix {packageFor = _: hookyardPackage;};

  mkManifest = id:
    pkgs.writeText "hookyard-${id}.json" (builtins.toJSON {
      handlers = [
        {
          inherit id;
          # loadAll stats exec before the --dry-run branch.
          exec = "${pkgs.coreutils}/bin/true";
          # Chosen so the rendered plan is non-empty (Bash -> Cursor's native
          # Shell matcher) — an empty plan would make the dry-run grep below
          # pass vacuously against a literal "(nothing)".
          events = ["pre_tool"];
          engines = ["cursor"];
          match = ["Bash"];
          timeout_ms = 0;
        }
      ];
    });

  # Two configurations over the same module: standalone, and the
  # NixOS-submodule shape nix-config uses, where home.profileDirectory takes
  # the /etc/profiles/per-user/<user> form. Both are known to evaluate:
  # submoduleSupport's two options are internal but not readOnly, and
  # osConfig defaults to null (home-manager's submodule-support.nix).
  mkScratch = name: submoduleSupport: extraHookyard: let
    evaluated = home-manager.lib.homeManagerConfiguration {
      inherit pkgs;
      modules = [
        hookyardModule
        {
          inherit submoduleSupport;
          home = {
            username = "hookyard-check";
            homeDirectory = "/home/hookyard-check";
            stateVersion = "24.05";
          };
          programs.hookyard =
            {
              enable = true;
              manifests = [(mkManifest "a-${name}") (mkManifest "b-${name}")];
              claudeSettings = "/build/hookyard-check-${name}/claude-settings.json";
              codexConfig = "/build/hookyard-check-${name}/config.toml";
              cursorHooks = "/build/hookyard-check-${name}/hooks.json";
            }
            // extraHookyard;
        }
      ];
    };
  in {
    inherit name;
    cfg = evaluated.config;
  };

  standalone = mkScratch "standalone" {} {};
  submodule = mkScratch "submodule" {
    enable = true;
    externalPackageInstall = true;
  } {};

  # Proves the quoting above survives a whitespace-containing value:
  # stateDir here is not exercised by dryRunScript below, because
  # checkShellSafe correctly refuses a space in --state-dir before a real
  # `install` ever runs — that refusal is the CLI's own defense, not this
  # bug. So this configuration is checked only at the Nix-evaluation level
  # in scratchChecks, against the rendered installCommand string: the
  # `--state-dir` assertion there fails if quoting is lost, because the
  # substring it looks for is the whole quoted flag, space included.
  spaced = mkScratch "spaced" {} {
    stateDir = "/build/hookyard-check-spaced/state dir";
  };

  # Counts non-overlapping occurrences of `needle` in `haystack` — how the
  # "hookyard install occurs exactly once" assertion is made without a
  # second, hand-maintained count living next to it.
  countOccurrences = needle: haystack: builtins.length (lib.splitString needle haystack) - 1;

  wholeActivationScript = cfg:
    lib.concatMapStringsSep "\n" (e: e.data) (lib.attrValues cfg.home.activation);

  # lib.hasInfix turns its needle into a builtins.match pattern, which
  # refuses a needle carrying string context — and every needle below is
  # built from store paths (the package, jq, coreutils). The comparison
  # itself is pure text, so discarding context here is safe; the real
  # dependency edges still flow through installCommand wherever it's
  # actually embedded in a build script (dryRunScript below).
  hasInfixCtx = needle: haystack: lib.hasInfix (builtins.unsafeDiscardStringContext needle) haystack;

  # Per-configuration assertions against evaluated values, never hardcoded
  # strings. Counting "hookyard install" within this module's own
  # entry alone would be vacuous, so it is counted across every
  # home.activation.*.data instead.
  scratchChecks = {
    name,
    cfg,
  }: let
    hy = cfg.programs.hookyard;
    entry = cfg.home.activation.hookyardInstall;
    expectedRouterFlag = ''--router-path "${cfg.home.profileDirectory}/bin/hookyard"'';
  in
    lib.optionals (name == "submodule") [
      {
        cond = lib.hasPrefix "/etc/profiles/per-user/" cfg.home.profileDirectory;
        msg = "submoduleSupport didn't produce the /etc/profiles/per-user profile path, got ${cfg.home.profileDirectory} — this check would be vacuous against the default .nix-profile form";
      }
    ]
    ++ [
      {
        cond = hasInfixCtx expectedRouterFlag hy.installCommand;
        msg = "installCommand is missing ${expectedRouterFlag}: ${hy.installCommand}";
      }
      {
        cond = hasInfixCtx hy.installCommand entry.data;
        msg = "home.activation.hookyardInstall.data doesn't contain installCommand verbatim: ${entry.data}";
      }
      {
        cond = countOccurrences "hookyard install" (wholeActivationScript cfg) == 1;
        msg = "expected exactly one \"hookyard install\" across the merged activation script, found ${toString (countOccurrences "hookyard install" (wholeActivationScript cfg))}";
      }
      {
        cond = countOccurrences "--manifest" hy.installCommand == builtins.length hy.manifests;
        msg = "expected ${toString (builtins.length hy.manifests)} --manifest flags, found ${toString (countOccurrences "--manifest" hy.installCommand)}: ${hy.installCommand}";
      }
      {
        cond = hasInfixCtx ''--state-dir "${hy.stateDir}"'' hy.installCommand;
        msg = ''installCommand is missing --state-dir "${hy.stateDir}": ${hy.installCommand}'';
      }
      {
        cond = hasInfixCtx "${pkgs.jq}/bin" hy.installCommand;
        msg = "installCommand's PATH is missing ${pkgs.jq}/bin: ${hy.installCommand}";
      }
      {
        cond = hasInfixCtx "${pkgs.coreutils}/bin" hy.installCommand;
        msg = "installCommand's PATH is missing ${pkgs.coreutils}/bin: ${hy.installCommand}";
      }
      {
        cond = builtins.elem "writeBoundary" entry.after && builtins.elem "installPackages" entry.after;
        msg = "home.activation.hookyardInstall.after is missing writeBoundary or installPackages, got ${builtins.toJSON entry.after}";
      }
      {
        cond = builtins.elem hy.package cfg.home.packages;
        msg = "programs.hookyard.package is not in home.packages";
      }
    ];

  allChecks = lib.concatMap scratchChecks [standalone submodule spaced];
  failures = lib.filter (c: !c.cond) allChecks;

  # Fail with a readable message: throwing every collected offender beats a
  # bare assertion failing on the first one with no context.
  guard =
    if failures == []
    then null
    else throw (lib.concatMapStringsSep "\n" (c: "- ${c.msg}") failures);

  # Execute the exact string the activation runs and grep its stdout
  # for the profile router path, proving the whole chain — flag parsing,
  # checkShellSafe, the rendered plan — accepts it, not just that the module
  # produced a plausible-looking string. --dry-run is side-effect-free and
  # exits before any destination is touched, so none of the three scratch
  # destination paths above need to exist. HOME is left at stdenv's default
  # (install resolves its flag defaults from os.UserHomeDir() before parsing,
  # so a cleared HOME breaks it even with every flag supplied explicitly).
  dryRunScript = {
    name,
    cfg,
  }: ''
    echo "=== ${name} ==="
    # named plan_out, not out: $out is the derivation's own output path below.
    plan_out=$(${cfg.programs.hookyard.installCommand} --dry-run)
    if ! grep -qF "${cfg.home.profileDirectory}/bin/hookyard" <<<"$plan_out"; then
      echo "hm-module check (${name}): --dry-run output did not contain the profile router path ${cfg.home.profileDirectory}/bin/hookyard" >&2
      echo "$plan_out" >&2
      exit 1
    fi
  '';
in
  builtins.seq guard (pkgs.runCommand "hookyard-hm-module-check" {} ''
    set -euo pipefail
    ${dryRunScript standalone}
    ${dryRunScript submodule}
    touch $out
  '')
