package clientgen

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// schema is the part of a JSON Schema that decides a Go type. Properties
// keep the order they have in the document: it is the order of the fields
// of the struct generated from them, and so of the JSON that struct
// marshals to.
type schema struct {
	boolean         *bool // a boolean schema: true admits anything, false nothing
	types           []string
	description     string
	props           []property
	hasProps        bool
	required        map[string]bool
	items           *schema
	tuple           bool    // prefixItems, or items as a list: positions have their own schemas
	additional      *schema // additionalProperties; nil when the keyword is absent
	patternProps    bool
	enum            []json.RawMessage
	ref             string
	contentEncoding string
	format          string
	anyOf           []*schema
	oneOf           []*schema
	allOf           []*schema
	defs            map[string]*schema // $defs and definitions
}

type property struct {
	name   string
	schema *schema
}

// members reads a JSON object's members in document order.
func members(raw json.RawMessage) ([]string, []json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, fmt.Errorf("not a JSON object")
	}
	var keys []string
	var values []json.RawMessage
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, _ := tok.(string)
		if seen[key] {
			return nil, nil, fmt.Errorf("member %q appears twice", key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, nil, err
		}
		keys, values = append(keys, key), append(values, value)
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, err
	}
	return keys, values, nil
}

// parseSchema reads one schema. Empty input is no schema (nil).
func parseSchema(raw json.RawMessage) (*schema, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] != '{' {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("a schema is an object or a boolean")
		}
		return &schema{boolean: &b}, nil
	}
	keys, values, err := members(raw)
	if err != nil {
		return nil, err
	}
	s := &schema{}
	for i, key := range keys {
		value := values[i]
		var err error
		switch key {
		case "type":
			var one string
			if json.Unmarshal(value, &one) == nil {
				s.types = []string{one}
			} else {
				err = json.Unmarshal(value, &s.types)
			}
		case "description":
			err = json.Unmarshal(value, &s.description)
		case "properties":
			s.hasProps = true
			s.props, err = parseProperties(value)
		case "required":
			var names []string
			if err = json.Unmarshal(value, &names); err == nil {
				s.required = map[string]bool{}
				for _, n := range names {
					s.required[n] = true
				}
			}
		case "items":
			if bytes.HasPrefix(bytes.TrimSpace(value), []byte("[")) {
				s.tuple = true
			} else {
				s.items, err = parseSchema(value)
			}
		case "prefixItems":
			s.tuple = true
		case "additionalProperties":
			s.additional, err = parseSchema(value)
		case "patternProperties":
			s.patternProps = true
		case "enum":
			err = json.Unmarshal(value, &s.enum)
		case "$ref":
			err = json.Unmarshal(value, &s.ref)
		case "contentEncoding":
			err = json.Unmarshal(value, &s.contentEncoding)
		case "format":
			err = json.Unmarshal(value, &s.format)
		case "anyOf":
			s.anyOf, err = parseSchemas(value)
		case "oneOf":
			s.oneOf, err = parseSchemas(value)
		case "allOf":
			s.allOf, err = parseSchemas(value)
		case "$defs", "definitions":
			var defs []property
			if defs, err = parseProperties(value); err == nil {
				if s.defs == nil {
					s.defs = map[string]*schema{}
				}
				for _, d := range defs {
					s.defs[d.name] = d.schema
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	return s, nil
}

func parseProperties(raw json.RawMessage) ([]property, error) {
	keys, values, err := members(raw)
	if err != nil {
		return nil, err
	}
	props := make([]property, len(keys))
	for i, key := range keys {
		sub, err := parseSchema(values[i])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if sub == nil {
			sub = &schema{}
		}
		props[i] = property{name: key, schema: sub}
	}
	return props, nil
}

func parseSchemas(raw json.RawMessage) ([]*schema, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]*schema, len(list))
	for i, item := range list {
		sub, err := parseSchema(item)
		if err != nil {
			return nil, err
		}
		if sub == nil {
			sub = &schema{}
		}
		out[i] = sub
	}
	return out, nil
}

// isNull reports a schema that admits only null.
func (s *schema) isNull() bool {
	return s != nil && len(s.types) == 1 && s.types[0] == "null"
}

// open reports an object that admits members beyond its properties: its
// additionalProperties is true or a schema, or it has patternProperties.
func (s *schema) open() bool {
	if s.patternProps {
		return true
	}
	if s.additional == nil {
		return false
	}
	return s.additional.boolean == nil || *s.additional.boolean
}
