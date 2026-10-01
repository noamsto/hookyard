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

// LoadRules reads the rule set at path, or the built-in one when path is "".
func LoadRules(path string) (Rules, error) {
	data, source := builtinRules, "built-in rules"
	if path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // path is the operator-configured rule set
		if err != nil {
			return Rules{}, fmt.Errorf("rules: %w", err)
		}
		data, source = b, path
	}
	r, err := parseRules(data)
	if err != nil {
		return Rules{}, fmt.Errorf("%s: %w", source, err)
	}
	return r, nil
}

func parseRules(data []byte) (Rules, error) {
	var file struct {
		Rule []struct {
			ID    string `toml:"id"`
			Regex string `toml:"regex"`
		} `toml:"rule"`
	}
	if err := toml.Unmarshal(data, &file); err != nil {
		return Rules{}, err
	}
	if len(file.Rule) == 0 {
		return Rules{}, errors.New("no rules")
	}
	seen := map[string]bool{}
	var r Rules
	for i, fr := range file.Rule {
		if fr.ID == "" || fr.Regex == "" {
			return Rules{}, fmt.Errorf("rule %d: id and regex are required", i+1)
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
