# priors execs git, ssh, rg and the secret scanner through store paths fixed at
# build time. This runs the real binary (index, lint, search, add) with
# look-alike shims first on PATH and fails if any of them is ever executed, so
# a PATH lookup coming back on those paths is caught here.
{
  pkgs,
  priors,
}:
pkgs.runCommand "priors-pinned" {} ''
  fake=$TMPDIR/fake
  marker=$TMPDIR/shim-ran
  mkdir -p "$fake"
  for tool in git ssh rg betterleaks gitleaks; do
    printf '#!${pkgs.runtimeShell}\ntouch "%s"\necho []\nexit 0\n' "$marker" > "$fake/$tool"
    chmod +x "$fake/$tool"
  done

  export HOME=$TMPDIR/home
  mkdir -p "$HOME"
  export PATH="$fake:${pkgs.coreutils}/bin"
  export GIT_CONFIG_NOSYSTEM=1
  export GIT_CONFIG_GLOBAL=$TMPDIR/gitconfig
  printf '[user]\n\tname = t\n\temail = t@example.com\n[commit]\n\tgpgsign = false\n' > "$GIT_CONFIG_GLOBAL"

  store=$TMPDIR/store
  mkdir -p "$store/demo"
  ${pkgs.git}/bin/git init -q "$store"
  token="gh""p_""Zq8Kd3LmX0pR7tVb2Nw9""Yc4Hf6Js1GaEuQiO"
  fact() {
    cat > "$store/demo/$1.md" <<FACT
  ---
  name: $1
  description: description of $1
  metadata:
    node_type: memory
    type: project
    scope: repo
    repos:
      - demo
    valid_from: 2026-01-01
    superseded_by: null
    verified: 2026-01-01
    confidence: proposed
    provenance:
      engine: claude
      session: s
      host: h
    modified: 2026-01-01T00:00:00Z
  ---
  $2
  FACT
  }
  fact leaky-fact "the token is $token"
  fact needle-fact "a haystack holding the needle"

  cfg=$TMPDIR/config.toml
  cat > "$cfg" <<CFG
  profile = "personal"
  personal_store = "$store"
  state_dir = "$TMPDIR/state"
  work_orgs = ["github.com/factify-inc"]
  personal_orgs = ["github.com/noamsto"]
  CFG

  ${priors}/bin/priors index --write --config "$cfg"

  set +e
  lint=$(${priors}/bin/priors lint --dir "$store" --kind personal --work-org github.com/x 2>&1)
  code=$?
  set -e
  echo "$lint"
  [ "$code" = 1 ] || { echo "lint exited $code, want 1"; exit 1; }
  echo "$lint" | ${pkgs.gnugrep}/bin/grep -qF 'demo/leaky-fact.md: gate:secret: scanner matched rule(s)' \
    || { echo "lint did not report the scanner's finding"; exit 1; }

  repo=$TMPDIR/repo
  ${pkgs.git}/bin/git init -q "$repo"
  ${pkgs.git}/bin/git -C "$repo" remote add origin git@github.com:noamsto/demo.git
  hits=$(PRIORS_CONFIG=$cfg ${priors}/bin/priors search needle --cwd "$repo")
  echo "$hits"
  echo "$hits" | ${pkgs.gnugrep}/bin/grep -qF needle-fact \
    || { echo "search did not find needle-fact through the pinned git and rg"; exit 1; }

  PRIORS_CONFIG=$cfg ${priors}/bin/priors add --name clean-fact --description "a clean fact" --type project \
    --cwd "$repo" --session s1 </dev/null

  if [ -e "$marker" ]; then
    echo "a PATH shim ran"
    exit 1
  fi
  touch $out
''
