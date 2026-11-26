// Package schemas holds hookyard's published JSON Schemas for its three wire
// shapes and the test that keeps them honest. The schemas are the contract a
// non-Go consumer reads; this test fails when a Go struct and its schema
// drift apart, and when a golden fixture stops satisfying either side.
package schemas

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/pkg/envelope"
)

type schema = map[string]any

func readJSON(t *testing.T, name string) any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

func loadSchema(t *testing.T, name string) schema {
	t.Helper()
	s, ok := readJSON(t, name).(map[string]any)
	if !ok {
		t.Fatalf("%s: not an object", name)
	}
	return s
}

// ---- minimal validator for the keyword subset the schemas use ----

var annotations = map[string]bool{
	"$schema": true, "$id": true, "title": true, "description": true, "$defs": true,
}

// validate returns every violation of s by v. A keyword outside the supported
// subset is itself a violation, so a schema cannot quietly gain a constraint
// this test never enforces.
func validate(root, s schema, v any, path string) []string {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, path+": "+fmt.Sprintf(f, a...)) }

	if ref, ok := s["$ref"].(string); ok {
		name, found := strings.CutPrefix(ref, "#/$defs/")
		defs, _ := root["$defs"].(map[string]any)
		target, _ := defs[name].(map[string]any)
		if !found || target == nil {
			return []string{path + ": unresolved $ref " + ref}
		}
		return validate(root, target, v, path)
	}
	for k, want := range s {
		switch k {
		case "type":
			if !typeMatches(want.(string), v) {
				add("want type %v, got %T", want, v)
			}
		case "enum":
			if !containsValue(want.([]any), v) {
				add("%v not in enum %v", v, want)
			}
		case "required":
			obj, ok := v.(map[string]any)
			if !ok {
				continue
			}
			for _, r := range want.([]any) {
				if _, present := obj[r.(string)]; !present {
					add("missing required %q", r)
				}
			}
		case "properties":
			obj, ok := v.(map[string]any)
			if !ok {
				continue
			}
			props := want.(map[string]any)
			for name, sub := range props {
				if val, present := obj[name]; present {
					errs = append(errs, validate(root, sub.(map[string]any), val, path+"."+name)...)
				}
			}
			if s["additionalProperties"] == false {
				for name := range obj {
					if _, known := props[name]; !known {
						add("unknown property %q", name)
					}
				}
			}
		case "additionalProperties":
			if want != false {
				add("unsupported additionalProperties value %v", want)
			}
		case "items":
			arr, ok := v.([]any)
			if !ok {
				continue
			}
			for i, el := range arr {
				errs = append(errs, validate(root, want.(map[string]any), el, fmt.Sprintf("%s[%d]", path, i))...)
			}
		case "minItems":
			if arr, ok := v.([]any); ok && float64(len(arr)) < want.(float64) {
				add("fewer than %v items", want)
			}
		case "minimum":
			if n, ok := v.(float64); ok && n < want.(float64) {
				add("%v below minimum %v", n, want)
			}
		case "maximum":
			if n, ok := v.(float64); ok && n > want.(float64) {
				add("%v above maximum %v", n, want)
			}
		case "minLength":
			if str, ok := v.(string); ok && float64(len(str)) < want.(float64) {
				add("shorter than %v", want)
			}
		case "pattern":
			if str, ok := v.(string); ok && !regexp.MustCompile(want.(string)).MatchString(str) {
				add("%q does not match %s", str, want)
			}
		default:
			if !annotations[k] {
				add("validator does not support keyword %q", k)
			}
		}
	}
	return errs
}

func typeMatches(typ string, v any) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && n == float64(int64(n))
	}
	return false
}

func containsValue(list []any, v any) bool {
	for _, x := range list {
		if reflect.DeepEqual(x, v) {
			return true
		}
	}
	return false
}

// ---- struct <-> schema drift ----

type field struct {
	name      string
	omitEmpty bool
	typ       reflect.Type
}

func jsonFields(t reflect.Type) []field {
	var out []field
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		out = append(out, field{name, strings.Contains(opts, "omitempty"), f.Type})
	}
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	sort.Strings(out)
	return out
}

// kindType is the schema type a Go type must be published as; "" means the
// schema must leave the property unconstrained (json.RawMessage).
func kindType(t reflect.Type) string {
	if t == reflect.TypeFor[json.RawMessage]() {
		return ""
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "integer"
	case reflect.Slice:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	case reflect.Pointer:
		return kindType(t.Elem())
	default:
	}
	panic("unsupported kind " + t.String())
}

// checkShape compares a struct's json tags with an object schema: the same
// property names, the same type per property, and required == every field
// without omitempty minus exempt (fields a documented reduced form omits).
func checkShape(t *testing.T, name string, typ reflect.Type, s schema, exempt ...string) {
	t.Helper()
	props, _ := s["properties"].(map[string]any)
	fields := jsonFields(typ)
	var goNames, goRequired []string
	for _, f := range fields {
		goNames = append(goNames, f.name)
		if !f.omitEmpty && !containsStr(exempt, f.name) {
			goRequired = append(goRequired, f.name)
		}
	}
	sort.Strings(goNames)
	sort.Strings(goRequired)
	if got := keys(props); !reflect.DeepEqual(got, goNames) {
		t.Errorf("%s: schema properties %v != Go json fields %v", name, got, goNames)
	}
	var schemaRequired []string
	if r, ok := s["required"]; ok {
		schemaRequired = strs(r)
	}
	if !reflect.DeepEqual(schemaRequired, goRequired) {
		t.Errorf("%s: schema required %v != Go required fields %v", name, schemaRequired, goRequired)
	}
	for _, f := range fields {
		prop, ok := props[f.name].(map[string]any)
		if !ok {
			continue
		}
		want := kindType(f.typ)
		if want == "" {
			if _, typed := prop["type"]; typed {
				t.Errorf("%s.%s: Go type is json.RawMessage but schema constrains type", name, f.name)
			}
			continue
		}
		if _, hasEnum := prop["enum"]; hasEnum {
			if want != "string" {
				t.Errorf("%s.%s: enum on non-string Go field", name, f.name)
			}
			continue
		}
		if prop["type"] != want && prop["$ref"] == nil {
			t.Errorf("%s.%s: schema type %v != Go kind %s", name, f.name, prop["type"], want)
		}
	}
}

func containsStr(list []string, s string) bool { return slices.Contains(list, s) }

func def(t *testing.T, s schema, name string) schema {
	t.Helper()
	d, ok := s["$defs"].(map[string]any)[name].(map[string]any)
	if !ok {
		t.Fatalf("missing $defs/%s", name)
	}
	return d
}

func TestSchemasMatchGoStructs(t *testing.T) {
	env := loadSchema(t, "envelope.schema.json")
	checkShape(t, "envelope", reflect.TypeFor[envelope.Envelope](), env)

	man := loadSchema(t, "manifest.schema.json")
	checkShape(t, "manifest", reflect.TypeFor[manifest.Manifest](), man)
	// match, timeout_ms and lane are optional in a hand-authored manifest: Load
	// defaults them.
	checkShape(t, "manifest.handler", reflect.TypeFor[manifest.Handler](), def(t, man, "handler"),
		"match", "timeout_ms", "lane")
	checkShape(t, "manifest.command", reflect.TypeFor[manifest.Command](), def(t, man, "command"))

	// A size-capped record collapses to the minimal fields plus truncated, so
	// these Go-required fields are not schema-required.
	rec := loadSchema(t, "record.schema.json")
	checkShape(t, "record", reflect.TypeFor[record.Record](), rec,
		"canonical_event", "native_event", "cwd", "tool_name", "enforced", "router_ms", "handlers")
	checkShape(t, "record.handler", reflect.TypeFor[record.RecordHandler](), def(t, rec, "handler"))
}

// TestSchemaVocabularyMatchesGo keeps the schema enums in step with the Go
// constants they mirror.
func TestSchemaVocabularyMatchesGo(t *testing.T) {
	prop := func(s schema, path ...string) []string {
		var cur any = s
		for _, p := range path {
			cur = cur.(map[string]any)[p]
		}
		return strs(cur.(map[string]any)["enum"])
	}
	sorted := func(in ...string) []string { sort.Strings(in); return in }

	rec := loadSchema(t, "record.schema.json")
	wantOutcomes := sorted(record.OutcomeAllow, record.OutcomeDeny, record.OutcomeAsk, record.OutcomeAdvise,
		record.OutcomeAbstain, record.OutcomeError, record.OutcomeTimeout, record.OutcomeDispatched, record.OutcomeSuppressed)
	if got := prop(rec, "$defs", "handler", "properties", "outcome"); !reflect.DeepEqual(got, wantOutcomes) {
		t.Errorf("record outcome enum %v != Go %v", got, wantOutcomes)
	}
	if got, want := prop(rec, "properties", "router"), sorted(record.RouterOK, record.RouterError, record.RouterTimeout); !reflect.DeepEqual(got, want) {
		t.Errorf("router enum %v != Go %v", got, want)
	}

	man := loadSchema(t, "manifest.schema.json")
	if got, want := prop(man, "$defs", "handler", "properties", "lane"), sorted(manifest.LaneVerdict, manifest.LaneFireAndForget); !reflect.DeepEqual(got, want) {
		t.Errorf("lane enum %v != Go %v", got, want)
	}
	timeout := def(t, man, "handler")["properties"].(map[string]any)["timeout_ms"].(map[string]any)
	if timeout["maximum"] != float64(manifest.MaxHandlerTimeoutMS) {
		t.Errorf("timeout_ms maximum %v != manifest.MaxHandlerTimeoutMS %d", timeout["maximum"], manifest.MaxHandlerTimeoutMS)
	}
}

// ---- golden fixtures ----

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// assertRoundTrip decodes raw into ptr and re-encodes it: any fixture key the
// Go type does not carry, or a field the encoder renames or drops, shows up as
// a difference.
func assertRoundTrip(t *testing.T, raw []byte, ptr any) {
	t.Helper()
	if err := json.Unmarshal(raw, ptr); err != nil {
		t.Fatalf("decode into Go type: %v", err)
	}
	out, err := json.Marshal(ptr)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("Go round-trip differs from fixture:\n fixture: %s\n go:      %s", bytes.TrimSpace(raw), out)
	}
}

func TestGoldenFixtures(t *testing.T) {
	cases := []struct {
		fixture string
		schema  string
		into    func() any
	}{
		{"envelope.golden.json", "envelope.schema.json", func() any { return new(envelope.Envelope) }},
		{"envelope.no-tool.golden.json", "envelope.schema.json", func() any { return new(envelope.Envelope) }},
		{"manifest.golden.json", "manifest.schema.json", func() any { return new(manifest.Manifest) }},
		{"manifest.minimal.golden.json", "manifest.schema.json", func() any { return new(manifest.Manifest) }},
		{"record.golden.json", "record.schema.json", func() any { return new(record.Record) }},
		{"record.minimal.golden.json", "record.schema.json", func() any { return new(record.Record) }},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			raw := fixture(t, c.fixture)
			s := loadSchema(t, c.schema)
			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			if errs := validate(s, s, doc, "$"); len(errs) > 0 {
				t.Errorf("fixture violates %s:\n%s", c.schema, strings.Join(errs, "\n"))
			}
			if strings.Contains(c.fixture, "minimal") {
				// A reduced form omits fields the Go type would
				// emit as zero values, so only decoding is checked.
				if err := json.Unmarshal(raw, c.into()); err != nil {
					t.Fatal(err)
				}
				return
			}
			assertRoundTrip(t, raw, c.into())
		})
	}
}

// TestSchemasRejectInvalid proves the validator (and so the fixtures check)
// can fail, and that the schemas' constraints bite.
func TestSchemasRejectInvalid(t *testing.T) {
	mutate := func(fix string, f func(map[string]any)) any {
		var doc map[string]any
		if err := json.Unmarshal(fixture(t, fix), &doc); err != nil {
			t.Fatal(err)
		}
		f(doc)
		return doc
	}
	handler0 := func(d map[string]any) map[string]any {
		return d["handlers"].([]any)[0].(map[string]any)
	}
	cases := []struct {
		name, fixture, schema string
		mut                   func(map[string]any)
	}{
		{"envelope missing session_id", "envelope.golden.json", "envelope.schema.json", func(d map[string]any) { delete(d, "session_id") }},
		{"envelope unknown engine", "envelope.golden.json", "envelope.schema.json", func(d map[string]any) { d["engine"] = "vim" }},
		{"manifest empty command description", "manifest.golden.json", "manifest.schema.json", func(d map[string]any) {
			d["commands"].([]any)[0].(map[string]any)["description"] = ""
		}},
		{"manifest missing exec", "manifest.golden.json", "manifest.schema.json", func(d map[string]any) { delete(handler0(d), "exec") }},
		{"manifest bad lane", "manifest.golden.json", "manifest.schema.json", func(d map[string]any) { handler0(d)["lane"] = "fast" }},
		{"manifest timeout too high", "manifest.golden.json", "manifest.schema.json", func(d map[string]any) { handler0(d)["timeout_ms"] = 4301 }},
		{"manifest bad id", "manifest.golden.json", "manifest.schema.json", func(d map[string]any) { handler0(d)["id"] = "-x" }},
		{"manifest no handlers", "manifest.golden.json", "manifest.schema.json", func(d map[string]any) { d["handlers"] = []any{} }},
		{"record missing verdict", "record.golden.json", "record.schema.json", func(d map[string]any) { delete(d, "verdict") }},
		{"record bad outcome", "record.golden.json", "record.schema.json", func(d map[string]any) { handler0(d)["outcome"] = "maybe" }},
		{"record string ms", "record.golden.json", "record.schema.json", func(d map[string]any) { handler0(d)["ms"] = "7" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := loadSchema(t, c.schema)
			if errs := validate(s, s, mutate(c.fixture, c.mut), "$"); len(errs) == 0 {
				t.Error("invalid document was accepted")
			}
		})
	}
}

// TestRecordUnknownFieldsTolerated pins the record contract: readers ignore
// what they do not recognize, so the schema must not forbid additions.
func TestRecordUnknownFieldsTolerated(t *testing.T) {
	s := loadSchema(t, "record.schema.json")
	doc := map[string]any{}
	if err := json.Unmarshal(fixture(t, "record.minimal.golden.json"), &doc); err != nil {
		t.Fatal(err)
	}
	doc["future_field"] = []any{1}
	if errs := validate(s, s, doc, "$"); len(errs) > 0 {
		t.Errorf("additive field rejected: %v", errs)
	}
}

// TestManifestUnknownFieldsTolerated pins manifest.Load's forward-compat
// contract (TestLoadIgnoresUnknownFields): the schema must not forbid fields a
// newer hookyard adds.
func TestManifestUnknownFieldsTolerated(t *testing.T) {
	s := loadSchema(t, "manifest.schema.json")
	doc := map[string]any{}
	if err := json.Unmarshal(fixture(t, "manifest.minimal.golden.json"), &doc); err != nil {
		t.Fatal(err)
	}
	doc["future_root"] = true
	doc["handlers"].([]any)[0].(map[string]any)["future_field"] = map[string]any{"nested": true}
	if errs := validate(s, s, doc, "$"); len(errs) > 0 {
		t.Errorf("additive field rejected: %v", errs)
	}
}
