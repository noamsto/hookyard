# Evaluates the exported home-manager module against several scratch
# configurations and then executes the rendered install command. Three
# properties are silent when broken and nothing else in the repo checks any
# of them: the emitted router path must be the rebuild-stable *profile* path,
# never a store path; `hookyard install` must be invoked exactly once across
# the whole merged activation script; and every interpolated path must reach
# bash escaped, since the activation script runs with the user's own
# privileges. Two more join them now that the Claude Code emit options exist
# `claudeOverlay.merged` must still evaluate — and be buildable — with
# hookyard disabled or with no manifests at all, since a consumer's `claude`
# wrapper reads it unconditionally regardless of `enable`; and `emit` itself
# must accept a manifest whose `exec` does not exist in the build sandbox
# since a consumer's checked-in manifest routinely names one under
# their own $HOME. Self-contained like
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
          # Chosen so the rendered plan is non-empty on both engines a check
          # below exercises: Bash -> Cursor's native Shell matcher (the
          # dry-run/expansion checks, unchanged from before) and Bash ->
          # Claude Code's PreToolUse (the emit checks added alongside). An
          # empty plan on either engine would make its grep below pass
          # vacuously against a literal "(nothing)".
          events = ["pre_tool"];
          engines = ["cursor" "claude-code"];
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
              codexConfig = "/build/hookyard-check-${name}/config.toml";
              cursorHooks = "/build/hookyard-check-${name}/hooks.json";
              # Unlike the other three destinations, Pi's writer lands a
              # second artifact beside the settings file — the bridge, at
              # <dir>/bin/hookyard-bridge.ts — so the scratch target needs a
              # writable directory under it, not just a writable file.
              piSettings = "/build/hookyard-check-${name}/pi/settings.json";
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

  # One hostile value carrying both halves of what escapeShellArg buys: a
  # space, and a command substitution. The space is no longer the interesting
  # half, because manual double quotes already stop word-splitting. The
  # substitution is what they do not stop — bash expands $(...), backticks and
  # $VAR inside double quotes just the same, so an unescaped value here runs at
  # activation time with the user's own privileges.
  #
  # The evaluation-level `--state-dir` assertion in scratchChecks catches a
  # lost escape on its own: its needle is built with escapeShellArg, so an
  # unescaped or merely double-quoted rendering does not contain it.
  #
  # The substitution is a `touch` rather than something inert like $(id) so
  # that an expansion leaves evidence behind for expansionScript below. The
  # marker is a relative path, created in the build's cwd, so that assertion
  # can't pass vacuously because some absolute directory happened not to exist.
  pwnedMarker = "hookyard-check-pwned";

  hostile = mkScratch "hostile" {} {
    stateDir = "/build/hookyard-check-hostile/state $(touch ${pwnedMarker}) dir";
  };

  # The two states in which `claudeOverlay.merged` must still evaluate
  # even though `installCommand` — and everything scratchChecks below asserts
  # about it — is undefined. Both go through mkScratch rather than a
  # hand-rolled homeManagerConfiguration, so they exercise the exact same
  # module instantiation as every scratch above; only the module's own config
  # differs.
  disabled = mkScratch "disabled" {} {enable = false;};
  emptyManifests = mkScratch "empty-manifests" {} {manifests = [];};

  # The manifest shape `emit` must not refuse — an absolute,
  # non-store exec that is simply absent from the sandbox, the same shape a
  # consumer's own checked-in manifest uses when it names a path under their
  # $HOME. `install`'s refusal of this exact shape is already unit-tested in
  # Go (manifest_test.go); nothing here duplicates that half. mkManifest above
  # cannot stand in for it: its exec is `${pkgs.coreutils}/bin/true`
  # specifically so dryRunScript's `install --dry-run` — which does stat exec,
  # via loadAll — keeps working for standalone/submodule.
  nonSandboxManifest = pkgs.writeText "hookyard-nonsandbox-exec.json" (builtins.toJSON {
    handlers = [
      {
        id = "nonsandbox-exec";
        exec = "/home/hookyard-check/.local/bin/nonsandbox-exec-handler";
        events = ["pre_tool"];
        engines = ["claude-code"];
        match = ["Bash"];
        timeout_ms = 0;
      }
    ];
  });

  # A scratch of its own, not standalone/submodule with this manifest merely
  # appended: if this exec were ever loadAll-stat'd again — regressing
  # LoadStatic back to Load — the failure needs to land on this one
  # configuration's build, not silently break standalone's dryRunScript too.
  nonSandboxExec = mkScratch "nonsandbox-exec" {} {manifests = [nonSandboxManifest];};

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
    # Built with escapeShellArg, like the module itself, so the two cannot
    # drift into agreeing on a hand-copied quoting style that neither uses.
    expectedRouterFlag = "--router-path ${lib.escapeShellArg "${cfg.home.profileDirectory}/bin/hookyard"}";
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
        cond = hasInfixCtx "--state-dir ${lib.escapeShellArg hy.stateDir}" hy.installCommand;
        msg = "installCommand is missing --state-dir ${lib.escapeShellArg hy.stateDir}: ${hy.installCommand}";
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

  # `merged`'s readOnly-with-no-default shape means a reference to
  # it throws immediately, before `cond` is even compared, if it regresses to
  # being defined only under `mkIf cfg.enable` — there is no false `cond` to
  # report in that world, only a build that aborts with a raw nixpkgs
  # module-system trace instead of one of the messages below. lib.isDerivation
  # is the "readable" half: it proves the option resolved to a package, the
  # shape the consumer's `claude` wrapper actually interpolates, not merely to
  # some other readOnly value that happens not to throw.
  mergedIsReadable = {
    name,
    cfg,
  }: {
    cond = lib.isDerivation cfg.programs.hookyard.claudeOverlay.merged;
    msg = "hm-module check (${name}): programs.hookyard.claudeOverlay.merged did not evaluate to a package";
  };

  # The disabled configuration's own, smaller assertion set (item 2): none of
  # it reads installCommand, because nothing here is defined when
  # enable = false. What it does assert is the other side of turning hookyard
  # off — no binary lands in home.packages, and no activation entry runs it —
  # so a regression that keeps installing the binary while claiming to be
  # disabled still gets caught, just not through installCommand.
  disabledChecks = let
    hy = disabled.cfg.programs.hookyard;
  in [
    (mergedIsReadable disabled)
    {
      cond = !(builtins.elem hy.package disabled.cfg.home.packages);
      msg = "programs.hookyard.package is in home.packages with enable = false — disabling hookyard should leave no binary on PATH";
    }
    {
      cond = !(builtins.hasAttr "hookyardInstall" disabled.cfg.home.activation);
      msg = "home.activation.hookyardInstall exists with enable = false — disabling hookyard should leave no activation entry";
    }
  ];

  allChecks =
    lib.concatMap scratchChecks [standalone submodule hostile]
    ++ disabledChecks
    ++ [(mergedIsReadable emptyManifests)];
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

  # The regression escapeShellArg exists to prevent, executed rather than
  # asserted about. hostile deliberately does not go through dryRunScript:
  # checkShellSafe refuses a space and a `$` in --state-dir before any real
  # install, so a success-path check there would exercise the CLI's own guard
  # rather than this module's escaping. What is proved here instead is that
  # bash handed the binary the substitution as literal text — the command is
  # refused, and the marker was never created. Unescaped, bash runs `touch`
  # first and hookyard only ever sees the harmless empty expansion.
  expansionScript = ''
    echo "=== hostile ==="
    rm -f ${pwnedMarker}
    if ${hostile.cfg.programs.hookyard.installCommand} --dry-run; then
      echo "hm-module check (hostile): install accepted a --state-dir it should have refused" >&2
      exit 1
    fi
    if [ -e ${pwnedMarker} ]; then
      echo "hm-module check (hostile): activation executed the command substitution in --state-dir" >&2
      exit 1
    fi
  '';

  # Proves the claude-code emit path the same way dryRunScript
  # above proves --dry-run — by executing the artifact and grepping its
  # output, not by comparing option values. This reads `claudeOverlay.merged`,
  # never `claudeHooks` directly, because `merged` is the option a consumer
  # the consumer's `claude` wrapper to interpolate (and here falls back to
  # `claudeHooks` verbatim, since none of these scratches set
  # `claudeOverlay.base`). Building it here is a real build, not the
  # import-from-derivation: nothing in ../hm-module.nix ever calls
  # `builtins.readFile`/`fromJSON` on this value, only this check does, and
  # only after the module has already produced the store path. Reused for
  # `nonSandboxExec` below (item 5): forcing that build here is also the proof
  # that `emit` succeeded over a manifest whose exec doesn't exist in the
  # sandbox — if it hadn't, `cat` would never run, because the derivation it
  # names would already have failed to build.
  emitScript = {
    name,
    cfg,
  }: ''
    echo "=== ${name} (emit) ==="
    emit_out=$(cat ${cfg.programs.hookyard.claudeOverlay.merged})
    if ! grep -qF "${cfg.home.profileDirectory}/bin/hookyard" <<<"$emit_out"; then
      echo "hm-module check (${name}): claudeOverlay.merged did not contain the profile router path ${cfg.home.profileDirectory}/bin/hookyard" >&2
      echo "$emit_out" >&2
      exit 1
    fi
    if ! grep -qF "PreToolUse" <<<"$emit_out"; then
      echo "hm-module check (${name}): claudeOverlay.merged did not contain PreToolUse, this configuration's manifest's native event key for claude-code" >&2
      echo "$emit_out" >&2
      exit 1
    fi
  '';

  # Item 2/4's build-time half: mergedIsReadable above only proves the option
  # resolved to a derivation, cheaply, without building it — the same
  # eval-vs-execute split scratchChecks' --state-dir assertion and
  # expansionScript already draw for hostile (see the comment above
  # pwnedMarker). This is the executed half for disabled/emptyManifests: it
  # forces the emit derivation to actually build with no manifests
  # registered (disabled, via `emittedManifestFlags`'s `cfg.enable` gate) or
  # with an empty `manifests` list, and only then reads its output.
  mergedBuildsScript = {
    name,
    cfg,
  }: ''
    echo "=== ${name} (claudeOverlay.merged builds) ==="
    cat ${cfg.programs.hookyard.claudeOverlay.merged} >/dev/null
  '';
in
  builtins.seq guard (pkgs.runCommand "hookyard-hm-module-check" {} ''
    set -euo pipefail
    ${dryRunScript standalone}
    ${dryRunScript submodule}
    ${expansionScript}
    ${emitScript standalone}
    ${emitScript nonSandboxExec}
    ${mergedBuildsScript disabled}
    ${mergedBuildsScript emptyManifests}
    touch $out
  '')
