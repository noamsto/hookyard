package render

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// object is a JSON object that remembers its key order.
//
// Go maps do not, and marshalling one re-sorts every key. Two of the three
// target files carry hand-edited content, so reshuffling their top level on
// every activation would turn a two-line hook change into a whole-file diff —
// a cosmetic clobber of exactly the content §8 commits to leaving alone.
type object struct {
	keys   []string
	values map[string]json.RawMessage
}

func parseObject(raw []byte) (*object, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return &object{values: map[string]json.RawMessage{}}, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, fmt.Errorf("want a JSON object, got %v", tok)
	}
	obj := &object{values: values}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("want a string key, got %v", key)
		}
		obj.keys = append(obj.keys, name)
		if err := skipValue(dec); err != nil {
			return nil, err
		}
	}
	return obj, nil
}

// skipValue consumes one value, descending through nested containers so the
// decoder lands on the next key of the object being scanned.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return nil
	}
	for dec.More() {
		if err := skipValue(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token() // closing delimiter
	return err
}

func (o *object) get(key string) (json.RawMessage, bool) {
	v, ok := o.values[key]
	return v, ok
}

func (o *object) set(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = raw
	return nil
}

func (o *object) delete(key string) {
	if _, exists := o.values[key]; !exists {
		return
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *object) marshalIndent() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, key := range o.keys {
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		var value bytes.Buffer
		if err := json.Indent(&value, o.values[key], "  ", "  "); err != nil {
			return nil, err
		}
		fmt.Fprintf(&buf, "  %s: %s", name, value.String())
		if i < len(o.keys)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

// marshalCompact is how one object nests inside another. object's fields are
// unexported and it has no MarshalJSON, so set would render it as {}; feeding
// these bytes back as a json.RawMessage round-trips them verbatim instead.
// marshalIndent cannot serve here — it is top-level-only, writing a trailing
// newline and an indent fixed at two spaces — and marshalIndent's json.Indent
// over each value is what re-indents these compact bytes on the outer encode.
func (o *object) marshalCompact() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{")
	for i, key := range o.keys {
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			buf.WriteString(",")
		}
		buf.Write(name)
		buf.WriteString(":")
		if err := json.Compact(&buf, o.values[key]); err != nil {
			return nil, err
		}
	}
	buf.WriteString("}")
	return buf.Bytes(), nil
}
