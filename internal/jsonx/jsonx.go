// Package jsonx provides JSON decoding with unknown-field capture.
//
// Provider payloads evolve faster than this library ships. Every native typed
// struct in provider/* carries an `Extra map[string]json.RawMessage` field;
// Unmarshal routes any JSON key that has no corresponding struct field into
// Extra so no data is ever dropped (fidelity guarantee, DESIGN.md §5.2).
package jsonx

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Unmarshal decodes data into v like encoding/json.Unmarshal, then captures
// unrecognized object keys into v's `Extra map[string]json.RawMessage` field
// when one exists. v must be a non-nil pointer.
func Unmarshal(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return nil
	}
	sv := rv.Elem()
	extra := sv.FieldByName("Extra")
	if !extra.IsValid() || !extra.CanSet() || extra.Type() != reflect.TypeOf(map[string]json.RawMessage(nil)) {
		return nil
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		// Not a JSON object (e.g. array payload); nothing to capture.
		return nil //nolint:nilerr // non-object payloads carry no extra fields
	}
	known := map[string]bool{}
	collectKnownKeys(sv.Type(), known)
	for k := range all {
		if known[k] {
			delete(all, k)
		}
	}
	if len(all) == 0 {
		return nil
	}
	extra.Set(reflect.ValueOf(all))
	return nil
}

func collectKnownKeys(t reflect.Type, keys map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			collectKnownKeys(f.Type, keys)
			continue
		}
		if f.Name == "Extra" {
			continue
		}
		name := f.Name
		if tag, ok := f.Tag.Lookup("json"); ok {
			base, _, _ := strings.Cut(tag, ",")
			if base == "-" {
				continue
			}
			if base != "" {
				name = base
			}
		}
		keys[name] = true
	}
}
