# Evaluates the exported home-manager module against several scratch
# configurations and then executes the rendered install command. Three
# properties are silent when broken and nothing else in the repo checks any
# of them: the emitted router path must be the rebuild-stable `<stateDir>/bin/hookyard`
# symlink, independent of which activation form (NixOS-submodule vs
# standalone) last ran — never a store path, and no longer the profile path
# either; `hookyard install` must be invoked exactly once across
# the whole merged activation script; and every interpolated path must reach
# bash escaped, since the activation script runs with the user's own
# privileges. Two more join them now that the Claude Code emit options exist:
# `claudeOverlay.merged` must still evaluate — and be buildable — with
# hookyard disabled or with no manifests at all, since a consumer's `claude`
# wrapper reads it unconditionally regardless of `enable`; and disabling
# hookyard must never force `cfg.package` to build, in every shape the
# disabled branch can take (`claudeOverlay.base` set or unset). Self-contained
# like ../hm-module.nix (see its header comment) — takes the package as an
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
          # Chosen so the rendered plan is non-empty for the engine
          # dryRunScript exercises: Bash -> Cursor's native Shell matcher. An
          # empty plan would make its grep below pass vacuously against a
          # literal "(nothing)". `claude-code` in `engines` gives table.json a
          # claude-code row; the emit checks below render the fixed catalog
          # and never read this handler.
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
              piSettings = ["/build/hookyard-check-${name}/pi/settings.json"];
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

  # A consumer-authored overlay: one inherited PreToolUse hook plus a
  # statusLine, standing in for the file a consumer's own Nix already wires
  # as Claude Code's `--settings` overlay. A derivation, not inline text, so
  # evaluating `disabledWithBase` goes through a real store path the way
  # nix-config's `base` does.
  overlayBase = pkgs.writeText "hookyard-check-overlay-base.json" (builtins.toJSON {
    statusLine = {
      type = "command";
      command = "echo hookyard-check-status";
    };
    hooks.PreToolUse = [
      {
        matcher = "Bash";
        hooks = [
          {
            type = "command";
            command = "echo hookyard-check-inherited";
          }
        ];
      }
    ];
  });

  # The two states in which `claudeOverlay.merged` must still evaluate — and
  # build — even though `installCommand` — and everything scratchChecks below
  # asserts about it — is undefined. Both go through mkScratch rather than a
  # hand-rolled homeManagerConfiguration, so they exercise the exact same
  # module instantiation as every scratch above; only the module's own config
  # differs. `package` is a `throw`, so anything in the disabled branch that
  # forces `cfg.package` fails eval instead of quietly building it.
  disabled = mkScratch "disabled" {} {
    enable = false;
    package = throw "hookyard package must not be forced when enable = false";
  };
  disabledWithBase = mkScratch "disabled-with-base" {} {
    enable = false;
    package = throw "hookyard package must not be forced when enable = false";
    claudeOverlay.base = overlayBase;
  };
  emptyManifests = mkScratch "empty-manifests" {} {manifests = [];};

  # Item 4's scratch: a normal, non-hostile configuration like standalone,
  # just pointed at its own /build subtree so installCommand's real `install`
  # (not --dry-run) can run without colliding with any other scratch's
  # destinations.
  e2e = mkScratch "e2e" {} {stateDir = "/build/hookyard-check-e2e/state";};

  # The witness half of item 4's comparison: the exact text hm-module.nix
  # would place at xdg.configFile."hookyard/generation.json" for e2e, copied
  # out to its own store path so the runCommand script below can jq against
  # it after the real install has written its receipt.
  witnessFile =
    pkgs.writeText "hookyard-check-e2e-witness.json"
    e2e.cfg.xdg.configFile."hookyard/generation.json".text;

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
    expectedRouterFlag = "--router-path ${lib.escapeShellArg "${hy.stateDir}/bin/hookyard"}";
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
        # What makes the build-time gate (nix/hm-module.nix's
        # hookyard-manifests-validated) structural rather than a promise in a
        # comment: installCommand's --manifest flags must be the *validated*
        # copies, read from the option itself — never from a `let` binding
        # inside that file, which installCommand's own comment there says
        # this check cannot see.
        cond = lib.all (m: hasInfixCtx "--manifest ${lib.escapeShellArg m}" hy.installCommand) hy.validatedManifestPaths;
        msg = "installCommand is missing a --manifest flag for one of hy.validatedManifestPaths: ${hy.installCommand}";
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
      {
        # `builtins.fromJSON` throws on a string carrying string context, and
        # `text` carries plenty — cfg.manifests are derivation outputs and
        # `hookyard` is `${cfg.package}/bin/hookyard`. Discarding context
        # here is the same discipline hasInfixCtx already applies above, for
        # the same reason: the comparison below is pure text, not
        # import-from-derivation — nothing here ever reads a derivation's
        # *output*, only this eval-time string. `generated` is checked before
        # `parsed` is ever forced, via `&&`'s laziness, so a missing witness
        # reports this message instead of throwing "attribute missing".
        cond = let
          generated = builtins.hasAttr "hookyard/generation.json" cfg.xdg.configFile;
          parsed =
            builtins.fromJSON
            (builtins.unsafeDiscardStringContext cfg.xdg.configFile."hookyard/generation.json".text);
        in
          generated
          && parsed.stateDir == hy.stateDir
          && parsed.routerPath == "${hy.stateDir}/bin/hookyard"
          && parsed.manifests == hy.validatedManifestPaths;
        msg = "xdg.configFile.\"hookyard/generation.json\" is missing, or its stateDir/routerPath/manifests don't match hy";
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
  # disabled still gets caught, just not through installCommand. The package
  # check goes by name, not by `cfg.package` itself, since `disabled` sets
  # `package = throw ...` precisely to catch anything that still forces it.
  disabledChecks = [
    (mergedIsReadable disabled)
    {
      cond = !(lib.any (p: lib.hasInfix "hookyard" (lib.getName p)) disabled.cfg.home.packages);
      msg = "home.packages contains a hookyard-named package with enable = false — disabling hookyard should leave no binary on PATH";
    }
    {
      cond = !(builtins.hasAttr "hookyardInstall" disabled.cfg.home.activation);
      msg = "home.activation.hookyardInstall exists with enable = false — disabling hookyard should leave no activation entry";
    }
    {
      # Turning hookyard off leaves no witness, so `doctor` reports Unknown
      # rather than a false Fail — the same shape as the "no activation
      # entry" and "no hookyard package" assertions above, just for the
      # generation witness.
      cond = !(builtins.hasAttr "hookyard/generation.json" disabled.cfg.xdg.configFile);
      msg = "xdg.configFile contains hookyard/generation.json with enable = false — disabling hookyard should leave no generation witness";
    }
  ];

  allChecks =
    # e2e is added here alongside standalone/submodule/hostile because it is
    # itself an ordinary, non-hostile scratch (see its comment above) — every
    # eval-time assertion scratchChecks makes about installCommand,
    # activation, --manifest flags and the generation witness holds for it
    # too, and running them costs nothing extra since e2e is evaluated
    # regardless for the build-time comparison below.
    lib.concatMap scratchChecks [standalone submodule hostile e2e]
    ++ disabledChecks
    ++ [
      (mergedIsReadable disabledWithBase)
      (mergedIsReadable emptyManifests)
      {
        cond = builtins.unsafeDiscardStringContext standalone.cfg.programs.hookyard.claudeOverlay.merged.outPath == builtins.unsafeDiscardStringContext emptyManifests.cfg.programs.hookyard.claudeOverlay.merged.outPath;
        msg = "claudeOverlay.merged's outPath differs between standalone and empty-manifests — merged must not depend on cfg.manifests";
      }
      {
        cond =
          standalone.cfg.programs.hookyard.stateDir
          == submodule.cfg.programs.hookyard.stateDir
          && hasInfixCtx
          "--router-path ${lib.escapeShellArg "${standalone.cfg.programs.hookyard.stateDir}/bin/hookyard"}"
          submodule.cfg.programs.hookyard.installCommand;
        msg = "submodule installCommand's router flag is not the same fixed stateDir path as standalone's — router must not depend on activation form (nix/hm-module.nix, issue #61)";
      }
    ];
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
    if ! grep -qF "${cfg.programs.hookyard.stateDir}/bin/hookyard" <<<"$plan_out"; then
      echo "hm-module check (${name}): --dry-run output did not contain the fixed stateDir router path ${cfg.programs.hookyard.stateDir}/bin/hookyard" >&2
      echo "$plan_out" >&2
      exit 1
    fi
  '';

  # The regression escapeShellArg exists to prevent, executed rather than
  # asserted about. hostile deliberately does not go through dryRunScript:
  # checkShellSafe refuses a space and a `$` in --state-dir (and, since
  # routerPath is now derived from cfg.stateDir, in --router-path too) before
  # any real install, so a success-path check there would exercise the CLI's
  # own guard rather than this module's escaping. What is proved here instead
  # is that bash handed the binary the substitution as literal text — the
  # command is refused, whichever of the two guards catches it first, and the
  # marker was never created. Unescaped, bash runs `touch` first and hookyard
  # only ever sees the harmless empty expansion.
  expansionScript = ''
    echo "=== hostile ==="
    rm -f ${pwnedMarker}
    if ${hostile.cfg.programs.hookyard.installCommand} --dry-run; then
      echo "hm-module check (hostile): install accepted a --router-path/--state-dir it should have refused" >&2
      exit 1
    fi
    if [ -e ${pwnedMarker} ]; then
      echo "hm-module check (hostile): activation executed the command substitution in --state-dir" >&2
      exit 1
    fi
  '';

  # Proves the claude-code emit path the same way dryRunScript
  # above proves --dry-run — by executing the artifact and querying its
  # output, not by comparing option values. This reads `claudeOverlay.merged`,
  # never `claudeHooks` directly, because `merged` is the option
  # the consumer's `claude` wrapper interpolates (and here falls back to
  # `claudeHooks` verbatim, since none of these scratches set
  # `claudeOverlay.base`). Building it here is a real build, not the
  # import-from-derivation: nothing in ../hm-module.nix ever calls
  # `builtins.readFile`/`fromJSON` on this value, only this check does, and
  # only after the module has already produced the store path.
  emitScript = {
    name,
    cfg,
  }: ''
    echo "=== ${name} (emit) ==="
    emit_out=$(cat ${cfg.programs.hookyard.claudeOverlay.merged})
    if ! grep -qF "${cfg.programs.hookyard.stateDir}/bin/hookyard" <<<"$emit_out"; then
      echo "hm-module check (${name}): claudeOverlay.merged did not contain the fixed stateDir router path ${cfg.programs.hookyard.stateDir}/bin/hookyard" >&2
      echo "$emit_out" >&2
      exit 1
    fi
    if ! ${pkgs.jq}/bin/jq -e '(.hooks|keys|sort) == ["Notification","PostToolUse","PreCompact","PreToolUse","SessionEnd","SessionStart","Stop","UserPromptSubmit"] and ([.hooks[][]] | all(has("matcher")|not)) and ([.hooks[][]] | all(.hooks|length==1))' ${cfg.programs.hookyard.claudeOverlay.merged} >/dev/null; then
      echo "hm-module check (${name}): claudeOverlay.merged is not exactly one matcher-less route row per catalog event" >&2
      cat ${cfg.programs.hookyard.claudeOverlay.merged} >&2
      exit 1
    fi
  '';

  # Item 2/4's build-time half: mergedIsReadable above only proves the option
  # resolved to a derivation, cheaply, without building it — the same
  # eval-vs-execute split scratchChecks' --state-dir assertion and
  # expansionScript already draw for hostile (see the comment above
  # pwnedMarker). This is the executed half for disabled/disabledWithBase/
  # emptyManifests: it forces `claudeOverlay.merged` to actually build and
  # only then reads its output.
  mergedBuildsScript = {
    name,
    cfg,
  }: ''
    echo "=== ${name} (claudeOverlay.merged builds) ==="
    cat ${cfg.programs.hookyard.claudeOverlay.merged} >/dev/null
  '';

  # The build-time half of the --manifest assertion above: `ls`ing the
  # directory validatedManifestPaths' elements live in forces
  # hookyard-manifests-validated to actually build, which is what makes
  # `hookyard validate --build-time` really run and accept the two manifests
  # mkManifest built. mkManifest points `exec` at `${pkgs.coreutils}/bin/true`
  # — a store path whose root IS present in the sandbox — so this exercises
  # the deciding arm of the build-time exec-runnable rule, not the skipped
  # one; leave mkManifest alone.
  validateBuildTimeScript = let
    validated = dirOf (builtins.head standalone.cfg.programs.hookyard.validatedManifestPaths);
  in ''
    echo "=== standalone (manifests validated at build time) ==="
    ls ${validated}
  '';

  emptyOverlay = pkgs.writeText "hookyard-check-empty-overlay.json" "{\n}\n";

  # `disabled`'s merged file must be byte-identical to an empty overlay, and
  # `disabledWithBase`'s to the base it was given. `cmp` rather than jq, so a
  # stray newline or reformatting counts as a difference.
  cmpMergedScript = {
    name,
    cfg,
    expected,
  }: ''
    echo "=== ${name} (claudeOverlay.merged content) ==="
    cmp ${expected} ${cfg.programs.hookyard.claudeOverlay.merged}
  '';

  # Item 4, the important one: nothing else anywhere compares a
  # module-produced witness against a Go-produced receipt. Without this,
  # `hookyard` (Nix writes `${cfg.package}/bin/hookyard`; Go writes
  # `filepath.EvalSymlinks(os.Executable())`) and `manifests` (two
  # independent renderers of one list) could disagree while every unit test
  # on both sides still passes. Runs the real `install` (no --dry-run) and
  # compares its receipt against witnessFile above. No `mkdir -p` beforehand:
  # Go's `atomicfile.Write` `MkdirAll`s its parent at 0755, so every engine
  # writer creates its own directory, and this script must not come to depend
  # on `EnsureStateDir` having made one for it first. `piVersion()` shells out
  # to `pi`, absent in the sandbox — it already falls back to a non-empty
  # string by design, so that is not a failure path.
  e2eScript = ''
    echo "=== e2e (witness matches receipt) ==="
    ${e2e.cfg.programs.hookyard.installCommand}
    if ! ${pkgs.jq}/bin/jq -e --slurpfile w ${witnessFile} '
          .complete == true
          and .schema      == $w[0].schema
          and .manifests   == $w[0].manifests
          and .routerPath  == $w[0].routerPath
          and .stateDir    == $w[0].stateDir
          and .codexConfig == $w[0].codexConfig
          and .cursorHooks == $w[0].cursorHooks
          and .piSettings  == $w[0].piSettings
          and .hookyard    == $w[0].hookyard
        ' /build/hookyard-check-e2e/state/install.json >/dev/null; then
      echo "hm-module check (e2e): install.json does not match the generation witness" >&2
      echo "--- witness ---" >&2; cat ${witnessFile} >&2
      echo "--- receipt ---" >&2; cat /build/hookyard-check-e2e/state/install.json >&2
      exit 1
    fi
  '';
in
  builtins.seq guard (pkgs.runCommand "hookyard-hm-module-check" {} ''
    set -euo pipefail
    ${dryRunScript standalone}
    ${dryRunScript submodule}
    ${validateBuildTimeScript}
    ${expansionScript}
    ${emitScript standalone}
    ${mergedBuildsScript disabled}
    ${mergedBuildsScript disabledWithBase}
    ${cmpMergedScript {
      inherit (disabled) name cfg;
      expected = emptyOverlay;
    }}
    ${cmpMergedScript {
      inherit (disabledWithBase) name cfg;
      expected = overlayBase;
    }}
    ${mergedBuildsScript emptyManifests}
    ${e2eScript}
    touch $out
  '')
