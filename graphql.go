package graphjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Unmarshal parses the JSON-encoded GraphQL response data and stores
// the result in the GraphQL query data structure pointed to by v.
//
// The implementation is created on top of the JSON tokenizer available
// in "encoding/json".Decoder.
func Unmarshal(data []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	err := (&decoder{tokenizer: dec}).Decode(v)
	if err != nil {
		return err
	}
	tok, err := dec.Token()
	switch err {
	case io.EOF:
		// Expect to get io.EOF. There shouldn't be any more
		// tokens left after we've decoded v successfully.
		fixStructWithTypename(reflect.ValueOf(v))
		return nil
	case nil:
		return fmt.Errorf("invalid token '%v' after top-level value", tok)
	default:
		return err
	}
}

// decoder is a JSON decoder that performs custom unmarshaling behavior
// for GraphQL query data structures. It's implemented on top of a JSON tokenizer.
type decoder struct {
	tokenizer interface {
		Token() (json.Token, error)
	}

	// Stack of what part of input JSON we're in the middle of - objects, arrays.
	parseState []json.Delim

	// Stacks of values where to unmarshal.
	// The top of each stack is the reflect.Value where to unmarshal next JSON value.
	//
	// The reason there's more than one stack is because we might be unmarshaling
	// a single JSON value into multiple GraphQL fragments or embedded structs, so
	// we keep track of them all.
	vs [][]reflect.Value
}

// Decode decodes a single JSON value from d.tokenizer into v.
func (d *decoder) Decode(v interface{}) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr {
		return fmt.Errorf("cannot decode into non-pointer %T", v)
	}
	d.vs = [][]reflect.Value{{rv.Elem()}}
	return d.decode()
}

// decode decodes a single JSON value from d.tokenizer into d.vs.
func (d *decoder) decode() error {
	// The loop invariant is that the top of each d.vs stack
	// is where we try to unmarshal the next JSON value we see.
	for len(d.vs) > 0 {
		tok, err := d.tokenizer.Token()
		if err == io.EOF {
			return errors.New("unexpected end of JSON input")
		} else if err != nil {
			return err
		}

		switch {

		// Are we inside an object and seeing next key (rather than end of object)?
		case d.state() == '{' && tok != json.Delim('}'):
			key, ok := tok.(string)
			if !ok {
				return errors.New("unexpected non-key in JSON input")
			}
			someFieldExist := false
			for i := range d.vs {
				v := d.vs[i][len(d.vs[i])-1]
				if v.Kind() == reflect.Ptr {
					v = v.Elem()
				}
				var f reflect.Value
				if v.Kind() == reflect.Struct {
					f = fieldByGraphQLName(v, key)
					if f.IsValid() {
						someFieldExist = true
					}
				}
				d.vs[i] = append(d.vs[i], f)
			}
			if !someFieldExist {
				return fmt.Errorf("struct field for %q doesn't exist in any of %v places to unmarshal", key, len(d.vs))
			}

			// We've just consumed the current token, which was the key.
			// Read the next token, which should be the value, and let the rest of code process it.
			tok, err = d.tokenizer.Token()
			if err == io.EOF {
				return errors.New("unexpected end of JSON input")
			} else if err != nil {
				return err
			}

		// Are we inside an array and seeing next value (rather than end of array)?
		case d.state() == '[' && tok != json.Delim(']'):
			someSliceExist := false
			for i := range d.vs {
				v := d.vs[i][len(d.vs[i])-1]
				if v.Kind() == reflect.Ptr {
					v = v.Elem()
				}
				var f reflect.Value
				if v.Kind() == reflect.Slice {
					v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem()))) // v = append(v, T).
					f = v.Index(v.Len() - 1)
					someSliceExist = true
				}
				d.vs[i] = append(d.vs[i], f)
			}
			if !someSliceExist {
				return fmt.Errorf("slice doesn't exist in any of %v places to unmarshal", len(d.vs))
			}
		}

		switch tok := tok.(type) {
		case string, json.Number, bool, nil:
			// Value.

			for i := range d.vs {
				v := d.vs[i][len(d.vs[i])-1]
				if !v.IsValid() {
					continue
				}
				err := unmarshalValue(tok, v)
				if err != nil {
					return err
				}
			}
			d.popAllVs()

		case json.Delim:
			switch tok {
			case '{':
				// Start of object.

				d.pushState(tok)

				frontier := make([]reflect.Value, len(d.vs)) // Places to look for GraphQL fragments/embedded structs.
				for i := range d.vs {
					v := d.vs[i][len(d.vs[i])-1]
					frontier[i] = v
					// TODO: Do this recursively or not? Add a test case if needed.
					//
					// type A struct { B *B }{ B: nil }
					// ↓
					// A{B: &B{}}
					if v.Kind() == reflect.Ptr && v.IsNil() {
						v.Set(reflect.New(v.Type().Elem())) // v = new(T).
					}
				}
				// Find GraphQL fragments/embedded structs recursively, adding to frontier
				// as new ones are discovered and exploring them further.
				for len(frontier) > 0 {
					v := frontier[0]
					frontier = frontier[1:]
					if v.Kind() == reflect.Ptr {
						v = v.Elem()
					}
					if v.Kind() != reflect.Struct {
						continue
					}
					for i := 0; i < v.NumField(); i++ {
						if isGraphQLFragment(v.Type().Field(i)) || v.Type().Field(i).Anonymous {
							// Add GraphQL fragment or embedded struct.
							d.vs = append(d.vs, []reflect.Value{v.Field(i)})
							frontier = append(frontier, v.Field(i))
						}
					}
				}
			case '[':
				// Start of array.

				d.pushState(tok)

				for i := range d.vs {
					v := d.vs[i][len(d.vs[i])-1]
					// TODO: Confirm this is needed, write a test case.
					//if v.Kind() == reflect.Ptr && v.IsNil() {
					//	v.Set(reflect.New(v.Type().Elem())) // v = new(T).
					//}

					// Reset slice to empty (in case it had non-zero initial value).
					if v.Kind() == reflect.Ptr {
						v = v.Elem()
					}
					if v.Kind() != reflect.Slice {
						continue
					}
					v.Set(reflect.MakeSlice(v.Type(), 0, 0)) // v = make(T, 0, 0).
				}
			case '}', ']':
				// End of object or array.
				d.popAllVs()
				d.popState()
			default:
				return errors.New("unexpected delimiter in JSON input")
			}
		default:
			return errors.New("unexpected token in JSON input")
		}
	}
	return nil
}

// pushState pushes a new parse state s onto the stack.
func (d *decoder) pushState(s json.Delim) {
	d.parseState = append(d.parseState, s)
}

// popState pops a parse state (already obtained) off the stack.
// The stack must be non-empty.
func (d *decoder) popState() {
	d.parseState = d.parseState[:len(d.parseState)-1]
}

// state reports the parse state on top of stack, or 0 if empty.
func (d *decoder) state() json.Delim {
	if len(d.parseState) == 0 {
		return 0
	}
	return d.parseState[len(d.parseState)-1]
}

// popAllVs pops from all d.vs stacks, keeping only non-empty ones.
func (d *decoder) popAllVs() {
	var nonEmpty [][]reflect.Value
	for i := range d.vs {
		d.vs[i] = d.vs[i][:len(d.vs[i])-1]
		if len(d.vs[i]) > 0 {
			nonEmpty = append(nonEmpty, d.vs[i])
		}
	}
	d.vs = nonEmpty
}

// fieldByGraphQLName returns an exported struct field of struct v
// that matches GraphQL name, or invalid reflect.Value if none found.
func fieldByGraphQLName(v reflect.Value, name string) reflect.Value {
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).PkgPath != "" {
			// Skip unexported field.
			continue
		}
		if hasGraphQLName(v.Type().Field(i), name) {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

// hasGraphQLName reports whether struct field f has GraphQL name.
func hasGraphQLName(f reflect.StructField, name string) bool {
	value, ok := f.Tag.Lookup("graphql")
	if !ok {
		// TODO: caseconv package is relatively slow. Optimize it, then consider using it here.
		//return caseconv.MixedCapsToLowerCamelCase(f.Name) == name
		return strings.EqualFold(f.Name, name)
	}
	value = strings.TrimSpace(value) // TODO: Parse better.
	if strings.HasPrefix(value, "...") {
		// GraphQL fragment. It doesn't have a name.
		return false
	}
	if i := strings.Index(value, "("); i != -1 {
		value = value[:i]
	}
	if i := strings.Index(value, ":"); i != -1 {
		value = value[:i]
	}
	return strings.TrimSpace(value) == name
}

// isGraphQLFragment reports whether struct field f is a GraphQL fragment.
func isGraphQLFragment(f reflect.StructField) bool {
	value, ok := f.Tag.Lookup("graphql")
	if !ok {
		return false
	}
	value = strings.TrimSpace(value) // TODO: Parse better.
	return strings.HasPrefix(value, "...")
}

// isTypename reports whether struct field f is a GraphQL __typename field.
func isTypename(f reflect.StructField) bool {
	value, ok := f.Tag.Lookup("graphql")
	if !ok {
		return false
	}
	value = strings.TrimSpace(value) // TODO: Parse better.
	return value == "__typename"
}

func fixStructWithTypename(v reflect.Value) {
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() == reflect.Slice {
		for i := 0; i < v.Len(); i++ {
			fixStructWithTypename(v.Index(i))
		}
	}
	if v.Kind() == reflect.Struct {
		// find "__typename" key
		var typeName string
		for i := 0; i < v.NumField(); i++ {
			if isTypename(v.Type().Field(i)) {
				iface := v.Field(i).Interface()
				iv, ok := iface.(string)
				if ok {
					typeName = iv
					break
				}
				iv2, ok2 := iface.(*string)
				if ok2 {
					typeName = *iv2
					break
				}
			}
		}

		// if the struct contains field matches to __typename, it's not interface type.
		isInterfaceType := typeName != ""
		for i := 0; i < v.NumField(); i++ {
			fieldType := v.Type().Field(i)
			if !isGraphQLFragment(fieldType) {
				continue
			}
			if !notEqualToTypeCondition(fieldType, typeName) {
				isInterfaceType = false
				break
			}
		}

		for i := 0; i < v.NumField(); i++ {
			// case of field is like "fragmentField fragmentField `graphql:"... on Fragment"`"
			fieldType := v.Type().Field(i)
			if !isInterfaceType && typeName != "" && isGraphQLFragment(fieldType) && notEqualToTypeCondition(fieldType, typeName) {
				e := v.Field(i)
				v.Field(i).Set(reflect.Zero(e.Type()))
			} else {
				// Fields that are not initialized are further explored.
				switch v.Field(i).Kind() {
				case reflect.Ptr,
					reflect.Slice,  // case of "foo": [...]
					reflect.Struct: // case of "foo": {...}
					fixStructWithTypename(v.Field(i))
				}
			}
		}
	}
}

// notEqualToTypeCondition makes sure that the typecondition is not equal to typename.
func notEqualToTypeCondition(f reflect.StructField, typename string) bool {
	value, ok := f.Tag.Lookup("graphql")
	if !ok {
		return false
	}

	// TODO(codehex): change to better handling
	sep := strings.Split(value, " on ")
	if len(sep) != 2 {
		return false
	}
	// sep[0] == "..."
	value = strings.TrimSpace(sep[1]) // TODO: Parse better.
	return value != typename
}

// unmarshalValue unmarshals JSON value into v.
// v must be addressable and not obtained by the use of unexported
// struct fields, otherwise unmarshalValue will panic.
func unmarshalValue(value json.Token, v reflect.Value) error {
	b, err := json.Marshal(value) // TODO: Short-circuit (if profiling says it's worth it).
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v.Addr().Interface())
}
