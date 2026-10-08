package gate

import "testing"

// coarseRows are the constructs the literaliser cannot read simply (#213):
// each runs gh or curl in bash, and coarse must flag it.
func coarseRows() []string {
	return []string{
		"bash <<'EOF'\n$'\\x67h' issue view 1\nEOF",
		"cat <<'EOF' | bash\n$'\\x67h' issue view 1\nEOF",
		"bash <<'EOF'\nprintf '\\x67h issue view 1' | bash\nEOF",
		"bash <<'E'OF\ngh issue view 1 $X\nEOF",
		"bash <<<'gh issue view 1'",
		`X='\x67h'; ${X@E} issue view 1`,
		`X='\D{gh}'; ${X@P} issue view 1`,
		`f(){ $X issue view 1; }; X=gh; f`,
		`eval '$X issue view 1'; X=gh; eval '$X issue view 1'`,
		`X=x; bash -c 'X=gh; $X issue view 1'`,
		`X=gxh; ${X/x/} issue view 1`,
		`X=xgh; ${X#x} issue view 1`,
		`c=(x); c+=("gh -R"); ${c[1]} issue view 1`,
		`c=(); c+=(gh); c+=(issue view 1); "${c[@]}"`,
		`${X-gh} issue view 1 ${x~~}`,
		`X=gh; $X$Y issue view 1; Y=zz`,
		`X=gh; ${X%$Y} issue view 1; Y=h`,
		`true || Y=g; X=gh; ${X#$Y} issue view 1`,
		`(Y=g); X=gh; ${X#$Y} issue view 1`,
		`Y=h; read Y < /dev/null; X=gh; ${X%$Y} issue view 1`,
		`X=gh; ${X##*$FOO} issue view 1`,
		`c=(sudo -u '#0'); c+=(gh); c+=(issue view 1); "${c[@]:-x}"`,
		`c=(sudo -u '#0'); c+=(gh); c+=(issue view 1); "${c[@]@E}"`,
		`IFS=; c=(gh issue view 1); ${c[@]}`,
		`X=x; unset "X"; X+=g; X+=h; $X issue view 1`,
		`c=(grep gh); ${c[@]:1} issue view 1`,
		`shopt -s nocasematch; X=Xgh; ${X/x/} issue view 1`,
		`shopt -u globasciiranges; X=Bgh; ${X#[a-c]} issue view 1`,
		`X=g; ${X/g/&h} issue view 1`,
		`X='\0'aaaaaaaa; : ${X@E}; gh issue view 1`,
		`source "$F"; curl x`,
		`eval "$CMD"; gh issue view 1`,
	}
}

// coarseClean are ordinary scripts: a risky construct with no listed tool in
// reach stays clean.
func coarseClean() []string {
	return []string{
		`for f in *.go; do echo ${f%.go}; done`,
		`bash -c 'echo ok'`,
		`bash <<'EOF'
echo ok
EOF`,
		`eval "$(ssh-agent -s)"`,
		`X=1; echo ${X:-2} ${X/1/3}`,
		`source ./env.sh`,
		`$EDITOR file.txt`,
	}
}

func TestCoarse(t *testing.T) {
	for _, cmd := range coarseRows() {
		t.Run(cmd, func(t *testing.T) {
			if !IngestsCall("Bash", []byte(commandInput(cmd))) {
				t.Errorf("IngestsCall(%q) = false, want true", cmd)
			}
		})
	}
	for _, cmd := range coarseClean() {
		t.Run(cmd, func(t *testing.T) {
			if IngestsCall("Bash", []byte(commandInput(cmd))) {
				t.Errorf("IngestsCall(%q) = true, want false", cmd)
			}
		})
	}
	argv := `{"command":["X=x","bash","-c","X=gh; $X issue view 1"]}`
	if !IngestsCall("Bash", []byte(argv)) {
		t.Errorf("IngestsCall(%s) = false, want true", argv)
	}
}
