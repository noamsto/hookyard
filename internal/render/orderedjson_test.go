package render

import (
	"encoding/json"
	"strings"
	"testing"
)

// Nesting an object inside another is what lets an examined hook group be
// re-encoded without flattening it into a Go map first, so the group's own key
// order and the fields hookyard does not model both have to survive the trip.
func TestObjectNestsInsideAnotherObjectWithOrderAndForeignFieldsIntact(t *testing.T) {
	inner, err := parseObject([]byte(`{"matcher":"Bash","hooks":[1],"failClosed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	nested, err := inner.marshalCompact()
	if err != nil {
		t.Fatal(err)
	}

	outer, err := parseObject([]byte(`{"theme":"dark"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := outer.set("hooks", json.RawMessage(nested)); err != nil {
		t.Fatal(err)
	}
	got, err := outer.marshalIndent()
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)

	var probe map[string]any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Fatalf("nested output is not valid JSON: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"failClosed": true`) {
		t.Errorf("the nested object lost a foreign field\n--- got ---\n%s", out)
	}
	matcher := strings.Index(out, `"matcher"`)
	hooks := strings.Index(out, `"hooks": [`)
	failClosed := strings.Index(out, `"failClosed"`)
	if matcher > hooks || hooks > failClosed {
		t.Errorf("the nested object's keys were reordered (matcher=%d hooks=%d failClosed=%d)\n--- got ---\n%s",
			matcher, hooks, failClosed, out)
	}
}
