// Package gate holds the write-path checks a fact passes before it may leave
// the host: the redaction rule set, the secret scanner, and the provenance,
// content and size flagging gates.
package gate

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed rules.toml
var builtinRules []byte

type rule struct {
	id string
	re *regexp.Regexp
}

// Rules is a loaded redaction rule set. The zero value matches nothing, so
// callers must obtain one through LoadRules to fail closed.
type Rules struct {
	rules []rule
}

// LoadRules returns the built-in rule set, plus the rules in the file at path
// when path is not "". The built-in set is a floor config cannot remove. An id
// the file shares with a built-in or repeats, an unknown key, or a missing,
// unparsable or empty file is an error, so a bad config fails closed.
func LoadRules(path string) (Rules, error) {
	r, err := parseRules(Rules{}, builtinRules)
	if err != nil {
		return Rules{}, fmt.Errorf("built-in rules: %w", err)
	}
	if path == "" {
		return r, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is the operator-configured rule set
	if err != nil {
		return Rules{}, fmt.Errorf("rules: %w", err)
	}
	r, err = parseRules(r, data)
	if err != nil {
		return Rules{}, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// parseRules returns base extended with the rules in data.
func parseRules(base Rules, data []byte) (Rules, error) {
	var file struct {
		Rule []struct {
			ID    string `toml:"id"`
			Regex string `toml:"regex"`
		} `toml:"rule"`
	}
	md, err := toml.Decode(string(data), &file)
	if err != nil {
		return Rules{}, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Rules{}, fmt.Errorf("unknown key(s) %q", strings.Join(keys, ", "))
	}
	if len(file.Rule) == 0 {
		return Rules{}, errors.New("no rules")
	}
	r := Rules{rules: slices.Clone(base.rules)}
	builtin := map[string]bool{}
	for _, ru := range base.rules {
		builtin[ru.id] = true
	}
	seen := map[string]bool{}
	for i, fr := range file.Rule {
		if fr.ID == "" || fr.Regex == "" {
			return Rules{}, fmt.Errorf("rule %d: id and regex are required", i+1)
		}
		if builtin[fr.ID] {
			return Rules{}, fmt.Errorf("rule %q: duplicate id of a built-in rule", fr.ID)
		}
		if seen[fr.ID] {
			return Rules{}, fmt.Errorf("rule %q: duplicate id", fr.ID)
		}
		seen[fr.ID] = true
		re, err := regexp.Compile(fr.Regex)
		if err != nil {
			return Rules{}, fmt.Errorf("rule %q: %w", fr.ID, err)
		}
		r.rules = append(r.rules, rule{id: fr.ID, re: re})
	}
	return r, nil
}

// Match returns the IDs of the rules text matches, in rule order. It never
// returns the matched text, so a report built from it cannot leak a secret.
func (r Rules) Match(text string) []string {
	var ids []string
	for _, ru := range r.rules {
		if ru.re.MatchString(text) {
			ids = append(ids, ru.id)
		}
	}
	return ids
}
