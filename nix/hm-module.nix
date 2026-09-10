# Takes the default package as a function argument rather than importing the
# flake, so this file has no dependency on flake.nix and can be unit-checked
# on its own (nix/checks/hm-module.nix). `packageFor` is `system: derivation`;
# resolving it against a concrete system happens below, inside the module's
# own `config`, never in `imports` — `pkgs` is itself config-defined
# `_module.args` in home-manager, so an `imports` list computed from
# `pkgs.stdenv.hostPlatform.system` would be infinite recursion.
{packageFor}: {
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.programs.hookyard;

  # The aggregation point (§8): the marker strip that removes hookyard's rows
  # on re-render does not record which manifest produced a row, so a second
  # per-repo invocation would strip the first repo's rows and those handlers
  # would vanish with no error. Consumers contribute to this one list; they
  # cannot express a second invocation.
  manifestFlags = lib.concatMapStringsSep " " (p: ''--manifest "${p}"'') cfg.manifests;

  # §9: hookyard is invoked by its **store** path here, because a store path
  # is a dependency of the generation and so is always present during
  # activation, while the **profile** path is what gets emitted via
  # --router-path for the three engines to call at hook-fire time.
  installCommand = lib.concatStringsSep " " (
    [
      "env"
      "PATH=\"${lib.makeBinPath [pkgs.jq pkgs.coreutils]}:$PATH\""
      "${cfg.package}/bin/hookyard"
      "install"
    ]
    ++ lib.optional (manifestFlags != "") manifestFlags
    # An empty list must render as "nothing registered", not as "skip the
    # invocation" — skipping would leave the previous generation's rows and
    # table.json live, so handlers a consumer just removed would keep firing
    # with nothing registering them any more.
    ++ lib.optional (cfg.manifests == []) "--allow-empty"
    ++ [
      # Never a store path here (§9): it would churn three engines' config on
      # every hookyard bump and invalidate Codex's per-entry hook trust hash.
      # The profile path changes only when the guard x event x engine table
      # does.
      #
      # Every path below is quoted: this runs as bash, which word-splits an
      # unquoted value on whitespace, and Go's flag package stops parsing at
      # the first non-flag argument — so one whitespace-containing value
      # would silently truncate the flags after it and let their defaults
      # resolve against the real $HOME instead of these configured targets.
      ''--router-path "${config.home.profileDirectory}/bin/hookyard"''
      ''--state-dir "${cfg.stateDir}"''
      ''--claude-settings "${cfg.claudeSettings}"''
      ''--codex-config "${cfg.codexConfig}"''
      ''--cursor-hooks "${cfg.cursorHooks}"''
    ]
  );
in {
  options.programs.hookyard = {
    enable = lib.mkEnableOption "hookyard, the cross-engine agent hook router";

    package = lib.mkOption {
      type = lib.types.package;
      default = packageFor pkgs.stdenv.hostPlatform.system;
      description = "The hookyard package to install and invoke.";
    };

    manifests = lib.mkOption {
      type = lib.types.listOf lib.types.path;
      default = [];
      description = ''
        The shared list of handler manifests, rendered into all three
        engines' native config by one `hookyard install` invocation (§9).
        Consumer modules contribute their own paths here; contributions
        merge across modules. A consumer contributes data, and never its own
        hookyard input or its own activation entry that calls
        `hookyard install` directly.
      '';
    };

    stateDir = lib.mkOption {
      type = lib.types.str;
      default = "${config.xdg.stateHome}/hookyard";
      description = "Directory the router reads its handler table from and writes records to.";
    };

    # These three exist for two reasons. First, determinism: without them
    # `install` would resolve its targets from CLAUDE_CONFIG_DIR/CODEX_HOME/
    # $HOME in the activation environment — the same environment this module
    # already refuses to trust for `jq` above — and a correct registration
    # written to a file no engine reads is a §5 fail-open. Second, they are
    # the operator's lever: a user who genuinely sets CLAUDE_CONFIG_DIR, or
    # whose settings.json is managed by another Nix module, needs somewhere
    # to point hookyard, and this is it.
    claudeSettings = lib.mkOption {
      type = lib.types.str;
      default = "${config.home.homeDirectory}/.claude/settings.json";
      description = "Claude Code settings.json to render hookyard's registration into.";
    };

    codexConfig = lib.mkOption {
      type = lib.types.str;
      default = "${config.home.homeDirectory}/.codex/config.toml";
      description = "Codex config.toml to render hookyard's registration into.";
    };

    cursorHooks = lib.mkOption {
      type = lib.types.str;
      default = "${config.home.homeDirectory}/.cursor/hooks.json";
      description = "Cursor hooks.json to render hookyard's registration into.";
    };

    # No default: evalOptionValue prepends a default to the definition list
    # before the readOnly arity check runs, so a default here plus the
    # `config` definition below would throw "read-only, set multiple times".
    # This is how nix/checks/hm-module.nix reaches the exact string the
    # activation runs — a `let` binding inside this file isn't visible to a
    # check outside it. Keep the name in sync with that check.
    installCommand = lib.mkOption {
      type = lib.types.str;
      internal = true;
      readOnly = true;
      description = "The exact command home.activation.hookyardInstall runs. Read-only.";
    };
  };

  config = lib.mkIf cfg.enable {
    # §9: putting the binary in the profile is this module's job, because a
    # profile path only resolves if it is there. §10's first-deployment
    # failure is precisely config wired without the binary installed.
    home.packages = [cfg.package];

    assertions = [
      {
        assertion = lib.hasPrefix "/" cfg.stateDir;
        message = "programs.hookyard.stateDir must be an absolute path, got ${cfg.stateDir}";
      }
    ];

    programs.hookyard.installCommand = installCommand;

    # Ordered after writeBoundary per the task, and after installPackages
    # because writeBoundary alone lets the config naming the profile path be
    # written before the profile that resolves it exists — home-manager's own
    # comment on installPackages gives this exact reason, and dconf, xfconf
    # and vicinae all depend on it the same way. `run` is home-manager's own
    # wrapper, so `home-manager switch -n` does not install for real.
    home.activation.hookyardInstall = lib.hm.dag.entryAfter ["writeBoundary" "installPackages"] ''
      run ${installCommand}
    '';
  };
}
