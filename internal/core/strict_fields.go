package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// encoding/json accepts case-folded field names; configuration and the node
// protocol do not. Validate spelling before Decode can overwrite a field via
// another spelling. Arbitrary map keys remain case-sensitive user data.
func exactFields(raw []byte, dst any) error {
	t := reflect.TypeOf(dst)
	if t == nil || t.Kind() != reflect.Pointer || reflect.ValueOf(dst).IsNil() {
		return Fail("INVALID_JSON", "JSONの出力先が不正です", nil)
	}
	return exactValue(raw, t.Elem(), "$")
}

func exactValue(raw json.RawMessage, t reflect.Type, path string) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// time.Time and other explicitly defined wire types own their representation.
	unmarshaler := reflect.TypeFor[json.Unmarshaler]()
	if t.Implements(unmarshaler) || reflect.PointerTo(t).Implements(unmarshaler) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var values map[string]json.RawMessage
		if e := json.Unmarshal(raw, &values); e != nil {
			return nil
		} // decoder reports type errors
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fields[name] = f.Type
		}
		for key, value := range values {
			child, ok := fields[key]
			if !ok {
				return Fail("INVALID_JSON", "JSONの項目名は大文字・小文字を含めて正確に指定してください", map[string]string{"path": path, "field": key})
			}
			if e := exactValue(value, child, path+"."+key); e != nil {
				return e
			}
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return nil
		} // []byte is base64 JSON text
		var values []json.RawMessage
		if e := json.Unmarshal(raw, &values); e != nil {
			return nil
		}
		for i, value := range values {
			if e := exactValue(value, t.Elem(), fmt.Sprintf("%s[%d]", path, i)); e != nil {
				return e
			}
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil
		}
		var values map[string]json.RawMessage
		if e := json.Unmarshal(raw, &values); e != nil {
			return nil
		}
		for key, value := range values {
			if e := exactValue(value, t.Elem(), path+"."+key); e != nil {
				return e
			}
		}
	}
	return nil
}
