// This test lives in package rest (not rest_test like the handler tests)
// because the two mappers it guards, clientSettingsToAPI and
// clientSettingsFromAPI, are unexported.
package rest

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/eleboucher/memini/internal/store"
)

// TestClientSettingsWireParity guards the hand-written mappers in
// config_shared.go against silently dropping a field: every exported
// store.ClientSettings field is set to a distinct non-nil value, pushed
// through clientSettingsToAPI then clientSettingsFromAPI, and reflect-compared
// back to the original. A field missing from either mapper comes back nil and
// fails by name, so the next ClientSettings addition cannot skip the wire the
// way auto_save_min_events and the capture max-chars pair once did (the
// handshake never sent them and PUTs of them were dropped).
//
// There is no allowlist: every field round-trips. If a genuinely server-only
// field is ever added, exempt it here explicitly with a justification.
func TestClientSettingsWireParity(t *testing.T) {
	var orig store.ClientSettings
	ov := reflect.ValueOf(&orig).Elem()
	tp := ov.Type()
	for i := range tp.NumField() {
		f := tp.Field(i)
		if !f.IsExported() {
			continue
		}
		ov.Field(i).Set(distinctFieldValue(t, f))
	}

	got := clientSettingsFromAPI(clientSettingsToAPI(orig))

	gv := reflect.ValueOf(got)
	for i := range tp.NumField() {
		f := tp.Field(i)
		if !f.IsExported() {
			continue
		}
		back := gv.Field(i)
		if back.Kind() == reflect.Pointer && back.IsNil() {
			t.Errorf("field %s: went in non-nil, came back nil; wire it through both "+
				"clientSettingsToAPI and clientSettingsFromAPI in config_shared.go", f.Name)
			continue
		}
		if want, have := ov.Field(i).Interface(), back.Interface(); !reflect.DeepEqual(want, have) {
			t.Errorf("field %s: round trip changed the value: sent %s, got back %s",
				f.Name, derefForMsg(want), derefForMsg(have))
		}
	}
}

// distinctFieldValue builds a non-nil value for one store.ClientSettings
// field, distinct per field so a crossed-wire copy (field A written into
// field B) is caught, not just a dropped one. Every field today is a pointer
// to bool/int/float64/string/[]string; a new field of any other shape fails
// loudly here so the test is extended alongside the struct.
func distinctFieldValue(t *testing.T, f reflect.StructField) reflect.Value {
	t.Helper()
	if f.Type.Kind() != reflect.Pointer {
		t.Fatalf("store.ClientSettings field %s is not a pointer (%s); teach distinctFieldValue its shape", f.Name, f.Type)
	}
	idx := f.Index[0]
	p := reflect.New(f.Type.Elem())
	switch elem := f.Type.Elem(); elem.Kind() {
	case reflect.Bool:
		p.Elem().SetBool(true)
	case reflect.Int:
		p.Elem().SetInt(int64(idx) + 1)
	case reflect.Float64:
		// The min-score fields cross the wire as float32, so the probe value
		// must be exactly representable in float32 to round-trip losslessly.
		p.Elem().SetFloat(float64(idx) + 0.5)
	case reflect.String:
		p.Elem().SetString("x-" + f.Name)
	case reflect.Slice:
		if elem.Elem().Kind() != reflect.String {
			t.Fatalf("store.ClientSettings field %s is a %s slice; teach distinctFieldValue its shape", f.Name, elem.Elem())
		}
		s := reflect.MakeSlice(elem, 1, 1)
		s.Index(0).SetString("x-" + f.Name)
		p.Elem().Set(s)
	default:
		t.Fatalf("store.ClientSettings field %s has unhandled type %s; teach distinctFieldValue its shape", f.Name, f.Type)
	}
	return p
}

// derefForMsg renders a pointer's pointee for failure messages, since printing
// the pointers themselves (addresses) would make the diff unreadable.
func derefForMsg(p any) string {
	rv := reflect.ValueOf(p)
	if rv.Kind() == reflect.Pointer && !rv.IsNil() {
		return fmt.Sprintf("%v", rv.Elem().Interface())
	}
	return fmt.Sprintf("%v", p)
}
