package module

import (
	"fmt"
	"strconv"
)

// FieldSchema declares one param a module's request or response data
// form carries. This is the "standard" data shape the request/
// response validation is checked against -- deliberately a small,
// bespoke shape (name/type/required) rather than full JSON Schema,
// matching the rest of this project's no-dependency philosophy. Also
// what gets marshaled into the JSON manifest uploaded to radar-api
// (see ToManifest in module.go) -- json tags matter here, not just
// yaml ones.
type FieldSchema struct {
	Name     string `yaml:"name" json:"name"`
	Type     string `yaml:"type" json:"type"` // "string" | "number" | "bool" | "object" | "array"
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

// The full set of JSON's structural types except null (a field's
// absence, not a type of its own, already covers that via Required).
// object/array exist for params that are inherently structured --
// e.g. a full xray/sing-box config blob -- not just scalars; a module
// using either never gets type-coercion leniency the way scalars do
// (see matchesType), since there's no sensible "string that looks
// like an object" the way there's a string that looks like a number.
var validFieldTypes = map[string]bool{"string": true, "number": true, "bool": true, "object": true, "array": true}

func validateFieldSchema(fields []FieldSchema, moduleName, which string) error {
	seen := map[string]bool{}
	for _, f := range fields {
		if f.Name == "" {
			return fmt.Errorf("module %q: %s field missing a name", moduleName, which)
		}
		if seen[f.Name] {
			return fmt.Errorf("module %q: %s field %q declared more than once", moduleName, which, f.Name)
		}
		seen[f.Name] = true
		if !validFieldTypes[f.Type] {
			return fmt.Errorf("module %q: %s field %q: type must be \"string\", \"number\", \"bool\", \"object\", or \"array\", got %q", moduleName, which, f.Name, f.Type)
		}
	}
	return nil
}

// validateRequest checks params against a module's declared request
// schema, run before any real probe/action is attempted. A missing
// required field or a value of the wrong type is rejected outright --
// this is the sole gate behind probe.Invalid.
func validateRequest(fields []FieldSchema, params map[string]any) error {
	for _, f := range fields {
		v, present := params[f.Name]
		if !present || v == nil {
			if f.Required {
				return fmt.Errorf("missing required param %q", f.Name)
			}
			continue
		}
		if !matchesType(v, f.Type) {
			return fmt.Errorf("param %q must be a %s", f.Name, f.Type)
		}
	}
	return nil
}

// matchesType accepts the natural Go type a JSON-sourced param
// carries (a real job's params always arrive this way, faithfully
// typed end to end from radar-api), but is deliberately lenient about
// a string that looks like the right type -- --param on the `probe`
// CLI can only ever produce strings, and rejecting `--param count=5`
// against a `number` field would be a real ergonomics regression for
// local testing that a job's actual params never hit in practice.
func matchesType(v any, wantType string) bool {
	switch wantType {
	case "string":
		_, ok := v.(string)
		return ok
	case "bool":
		if _, ok := v.(bool); ok {
			return true
		}
		s, ok := v.(string)
		return ok && (s == "true" || s == "false")
	case "number":
		switch v.(type) {
		case int, int64, float64:
			return true
		}
		if s, ok := v.(string); ok {
			_, err := strconv.ParseFloat(s, 64)
			return err == nil
		}
		return false
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	default:
		return false
	}
}
