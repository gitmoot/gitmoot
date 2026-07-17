package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	ServiceSchemaVersion          = 1
	MaxServiceSchemaFields        = 32
	MaxServiceStringLength        = 32 * 1024
	DefaultServiceInputMaxBody    = 64 * 1024
	ServiceInputEnvironmentPrefix = "GITMOOT_INPUT_"
)

type ServiceFieldType string

const (
	ServiceFieldString  ServiceFieldType = "string"
	ServiceFieldInteger ServiceFieldType = "integer"
	ServiceFieldBoolean ServiceFieldType = "boolean"
)

// ServiceSchema is the deliberately small, flat schema accepted by pipeline
// service exposures. It is not JSON Schema: only the fields represented here
// are legal, and ParseServiceSchema rejects every other construct.
type ServiceSchema struct {
	Version int                     `json:"version"`
	Fields  map[string]ServiceField `json:"fields"`
}

type ServiceField struct {
	Type      ServiceFieldType `json:"type"`
	Required  bool             `json:"required,omitempty"`
	MaxLength int              `json:"max_length,omitempty"`
	Minimum   *int64           `json:"minimum,omitempty"`
	Maximum   *int64           `json:"maximum,omitempty"`
	Enum      []string         `json:"enum,omitempty"`
}

// TypedValue preserves the validated JSON type until the service-run enqueuer
// converts it to the reserved environment namespace. Exactly one value member
// is meaningful according to Type.
type TypedValue struct {
	Type    ServiceFieldType
	String  string
	Integer int64
	Boolean bool
}

func (v TypedValue) EnvironmentValue() string {
	switch v.Type {
	case ServiceFieldString:
		return v.String
	case ServiceFieldInteger:
		return strconv.FormatInt(v.Integer, 10)
	case ServiceFieldBoolean:
		return strconv.FormatBool(v.Boolean)
	default:
		return ""
	}
}

// FieldError is intentionally value-free: it identifies only the schema field,
// the stable failure code, and a message derived from the schema constraint.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// InputDiagnostic is the envelope the HTTP pass can serialize directly.
type InputDiagnostic struct {
	Error  string       `json:"error"`
	Fields []FieldError `json:"fields"`
}

func NewInputDiagnostic(fields []FieldError) InputDiagnostic {
	copyOfFields := append([]FieldError(nil), fields...)
	sortFieldErrors(copyOfFields)
	return InputDiagnostic{Error: "invalid_input", Fields: copyOfFields}
}

// ServiceInputEnvName derives the only environment name a caller-controlled
// field can receive. Callers never supply an environment name themselves.
func ServiceInputEnvName(field string) string {
	return ServiceInputEnvironmentPrefix + strings.ToUpper(field)
}

// ServiceInputEnvironment renders validated values in lexical field order.
func ServiceInputEnvironment(values map[string]TypedValue) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, ServiceInputEnvName(key)+"="+values[key].EnvironmentValue())
	}
	return env
}

// CanonicalJSON returns the normalized schema. encoding/json sorts map keys, and
// the fixed structs above pin member order, making this suitable for hashing.
func (s ServiceSchema) CanonicalJSON() ([]byte, error) {
	return json.Marshal(s)
}

type serviceSchemaWire struct {
	Version json.RawMessage            `json:"version"`
	Fields  map[string]json.RawMessage `json:"fields"`
}

type serviceFieldWire struct {
	Type      json.RawMessage `json:"type"`
	Required  json.RawMessage `json:"required"`
	MaxLength json.RawMessage `json:"max_length"`
	Minimum   json.RawMessage `json:"minimum"`
	Maximum   json.RawMessage `json:"maximum"`
	Enum      json.RawMessage `json:"enum"`
}

// ParseServiceSchema parses the versioned flat service schema. It rejects
// duplicate JSON members before decoding so no validator/consumer discrepancy
// can arise from encoding/json's last-member-wins behavior.
func ParseServiceSchema(raw []byte) (ServiceSchema, error) {
	if err := rejectDuplicateServiceJSONKeys(raw); err != nil {
		return ServiceSchema{}, fmt.Errorf("service schema: %w", err)
	}
	var wire serviceSchemaWire
	if err := decodeStrictServiceJSON(raw, &wire); err != nil {
		return ServiceSchema{}, fmt.Errorf("service schema: %w", err)
	}
	version, err := parseRequiredInteger(wire.Version, "version")
	if err != nil {
		return ServiceSchema{}, fmt.Errorf("service schema: %w", err)
	}
	if version != ServiceSchemaVersion {
		return ServiceSchema{}, fmt.Errorf("service schema: version must be %d", ServiceSchemaVersion)
	}
	if wire.Fields == nil {
		return ServiceSchema{}, errors.New("service schema: fields object is required")
	}
	if len(wire.Fields) > MaxServiceSchemaFields {
		return ServiceSchema{}, fmt.Errorf("service schema: fields has %d entries; maximum is %d", len(wire.Fields), MaxServiceSchemaFields)
	}

	keys := make([]string, 0, len(wire.Fields))
	for name := range wire.Fields {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	fields := make(map[string]ServiceField, len(keys))
	for _, name := range keys {
		if !ValidTriggerPayloadKey(name) {
			return ServiceSchema{}, fmt.Errorf("service schema: field %q must be 1-64 bytes and match ^[a-z][a-z0-9_]*$", name)
		}
		field, err := parseServiceField(name, wire.Fields[name])
		if err != nil {
			return ServiceSchema{}, err
		}
		fields[name] = field
	}
	return ServiceSchema{Version: ServiceSchemaVersion, Fields: fields}, nil
}

func parseServiceField(name string, raw json.RawMessage) (ServiceField, error) {
	var wire serviceFieldWire
	if err := decodeStrictServiceJSON(raw, &wire); err != nil {
		return ServiceField{}, fmt.Errorf("service schema: field %q: %w", name, err)
	}
	var typeName string
	if len(wire.Type) == 0 {
		return ServiceField{}, fmt.Errorf("service schema: field %q: type is required", name)
	}
	if err := json.Unmarshal(wire.Type, &typeName); err != nil || strings.TrimSpace(typeName) != typeName {
		return ServiceField{}, fmt.Errorf("service schema: field %q: type must be string, integer, or boolean", name)
	}
	field := ServiceField{Type: ServiceFieldType(typeName)}
	if len(wire.Required) > 0 {
		if err := json.Unmarshal(wire.Required, &field.Required); err != nil || bytes.Equal(bytes.TrimSpace(wire.Required), []byte("null")) {
			return ServiceField{}, fmt.Errorf("service schema: field %q: required must be a boolean", name)
		}
	}

	switch field.Type {
	case ServiceFieldString:
		maxLength, err := parseRequiredInteger(wire.MaxLength, "max_length")
		if err != nil {
			return ServiceField{}, fmt.Errorf("service schema: field %q: %w", name, err)
		}
		if maxLength < 1 || maxLength > MaxServiceStringLength {
			return ServiceField{}, fmt.Errorf("service schema: field %q: max_length must be between 1 and %d", name, MaxServiceStringLength)
		}
		field.MaxLength = int(maxLength)
		if len(wire.Minimum) > 0 || len(wire.Maximum) > 0 {
			return ServiceField{}, fmt.Errorf("service schema: field %q: minimum/maximum are only valid for integer fields", name)
		}
		if len(wire.Enum) > 0 {
			if err := json.Unmarshal(wire.Enum, &field.Enum); err != nil || field.Enum == nil {
				return ServiceField{}, fmt.Errorf("service schema: field %q: enum must be an array of strings", name)
			}
			if len(field.Enum) == 0 {
				return ServiceField{}, fmt.Errorf("service schema: field %q: enum must not be empty", name)
			}
			seen := make(map[string]struct{}, len(field.Enum))
			for _, choice := range field.Enum {
				if utf8.RuneCountInString(choice) > field.MaxLength {
					return ServiceField{}, fmt.Errorf("service schema: field %q: enum value exceeds max_length", name)
				}
				if strings.ContainsRune(choice, '\x00') {
					return ServiceField{}, fmt.Errorf("service schema: field %q: enum values must not contain U+0000", name)
				}
				if _, duplicate := seen[choice]; duplicate {
					return ServiceField{}, fmt.Errorf("service schema: field %q: enum contains a duplicate value", name)
				}
				seen[choice] = struct{}{}
			}
		}
	case ServiceFieldInteger:
		if len(wire.MaxLength) > 0 || len(wire.Enum) > 0 {
			return ServiceField{}, fmt.Errorf("service schema: field %q: max_length/enum are only valid for string fields", name)
		}
		var err error
		if len(wire.Minimum) > 0 {
			minimum, parseErr := parseRequiredInteger(wire.Minimum, "minimum")
			err = parseErr
			field.Minimum = &minimum
		}
		if err == nil && len(wire.Maximum) > 0 {
			maximum, parseErr := parseRequiredInteger(wire.Maximum, "maximum")
			err = parseErr
			field.Maximum = &maximum
		}
		if err != nil {
			return ServiceField{}, fmt.Errorf("service schema: field %q: %w", name, err)
		}
		if field.Minimum != nil && field.Maximum != nil && *field.Minimum > *field.Maximum {
			return ServiceField{}, fmt.Errorf("service schema: field %q: minimum must be <= maximum", name)
		}
	case ServiceFieldBoolean:
		if len(wire.MaxLength) > 0 || len(wire.Minimum) > 0 || len(wire.Maximum) > 0 || len(wire.Enum) > 0 {
			return ServiceField{}, fmt.Errorf("service schema: field %q: boolean fields accept only type and required", name)
		}
	default:
		return ServiceField{}, fmt.Errorf("service schema: field %q: unknown type %q", name, typeName)
	}
	return field, nil
}

func parseRequiredInteger(raw json.RawMessage, name string) (int64, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("%s is required", name)
	}
	trimmed := strings.TrimSpace(string(raw))
	value, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return value, nil
}

// ValidateServiceInput validates one flat JSON object without ever incorporating
// caller values into diagnostics. maxBody <= 0 selects the 64 KiB default.
func ValidateServiceInput(schema ServiceSchema, body []byte, maxBody int) (map[string]TypedValue, []FieldError) {
	if maxBody <= 0 {
		maxBody = DefaultServiceInputMaxBody
	}
	if len(body) > maxBody {
		return nil, []FieldError{{Code: "body_too_large", Message: fmt.Sprintf("request body must be at most %d bytes", maxBody)}}
	}
	if err := rejectDuplicateServiceJSONKeys(body); err != nil {
		return nil, []FieldError{{Code: "malformed_json", Message: "must be a single JSON object"}}
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, []FieldError{{Code: "malformed_json", Message: "must be a single JSON object"}}
	}
	var rawValues map[string]json.RawMessage
	if err := decodeStrictServiceJSON(body, &rawValues); err != nil || rawValues == nil {
		return nil, []FieldError{{Code: "malformed_json", Message: "must be a single JSON object"}}
	}

	values := make(map[string]TypedValue, len(rawValues))
	errorsByField := make([]FieldError, 0)
	for name := range rawValues {
		if _, ok := schema.Fields[name]; !ok {
			errorsByField = append(errorsByField, FieldError{Field: name, Code: "unknown", Message: "is not allowed"})
		}
	}
	keys := make([]string, 0, len(schema.Fields))
	for name := range schema.Fields {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		field := schema.Fields[name]
		raw, present := rawValues[name]
		if !present {
			if field.Required {
				errorsByField = append(errorsByField, FieldError{Field: name, Code: "required", Message: "is required"})
			}
			continue
		}
		value, fieldErr := validateTypedServiceValue(name, field, raw)
		if fieldErr != nil {
			errorsByField = append(errorsByField, *fieldErr)
			continue
		}
		values[name] = value
	}
	sortFieldErrors(errorsByField)
	if len(errorsByField) > 0 {
		return nil, errorsByField
	}
	return values, nil
}

func validateTypedServiceValue(name string, field ServiceField, raw json.RawMessage) (TypedValue, *FieldError) {
	typeError := func(kind string) (TypedValue, *FieldError) {
		article := "a"
		if kind == "integer" {
			article = "an"
		}
		return TypedValue{}, &FieldError{Field: name, Code: "type", Message: "must be " + article + " " + kind}
	}
	switch field.Type {
	case ServiceFieldString:
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return typeError("string")
		}
		if strings.ContainsRune(value, '\x00') {
			return TypedValue{}, &FieldError{Field: name, Code: "invalid_string", Message: "must not contain U+0000"}
		}
		if utf8.RuneCountInString(value) > field.MaxLength {
			return TypedValue{}, &FieldError{Field: name, Code: "max_length", Message: fmt.Sprintf("must be at most %d characters", field.MaxLength)}
		}
		if len(field.Enum) > 0 {
			allowed := false
			for _, choice := range field.Enum {
				if value == choice {
					allowed = true
					break
				}
			}
			if !allowed {
				return TypedValue{}, &FieldError{Field: name, Code: "enum", Message: "must be one of the declared values"}
			}
		}
		return TypedValue{Type: field.Type, String: value}, nil
	case ServiceFieldInteger:
		value, err := parseRequiredInteger(raw, "value")
		if err != nil {
			return typeError("integer")
		}
		if field.Minimum != nil && value < *field.Minimum {
			return TypedValue{}, &FieldError{Field: name, Code: "minimum", Message: fmt.Sprintf("must be at least %d", *field.Minimum)}
		}
		if field.Maximum != nil && value > *field.Maximum {
			return TypedValue{}, &FieldError{Field: name, Code: "maximum", Message: fmt.Sprintf("must be at most %d", *field.Maximum)}
		}
		return TypedValue{Type: field.Type, Integer: value}, nil
	case ServiceFieldBoolean:
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return typeError("boolean")
		}
		return TypedValue{Type: field.Type, Boolean: value}, nil
	default:
		return TypedValue{}, &FieldError{Field: name, Code: "schema", Message: "has an invalid declared type"}
	}
}

func sortFieldErrors(fieldErrors []FieldError) {
	sort.Slice(fieldErrors, func(i, j int) bool {
		if fieldErrors[i].Field != fieldErrors[j].Field {
			return fieldErrors[i].Field < fieldErrors[j].Field
		}
		if fieldErrors[i].Code != fieldErrors[j].Code {
			return fieldErrors[i].Code < fieldErrors[j].Code
		}
		return fieldErrors[i].Message < fieldErrors[j].Message
	})
}

func decodeStrictServiceJSON(raw []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("must contain one JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateServiceJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object member name must be a string")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := dec.Token(); err == nil {
		return errors.New("must contain one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
