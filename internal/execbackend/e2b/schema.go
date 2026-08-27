package e2b

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// listed_sandbox_schema.json mirrors ListedSandbox and every referenced
// constraint at e2b-dev/e2b spec commit f0facc5d. clientID is deliberately
// removed from required because the vendor marks it deprecated.
//
//go:embed listed_sandbox_schema.json
var listedSandboxSchemaData []byte

var listedSandboxSchema = mustLoadListedSandboxSchema()

type schemaNode struct {
	Ref                  string                 `json:"$ref"`
	Type                 string                 `json:"type"`
	Format               string                 `json:"format"`
	Minimum              *int64                 `json:"minimum"`
	Enum                 []string               `json:"enum"`
	Required             []string               `json:"required"`
	Properties           map[string]*schemaNode `json:"properties"`
	Items                *schemaNode            `json:"items"`
	AdditionalProperties *schemaNode            `json:"additionalProperties"`
	Defs                 map[string]*schemaNode `json:"$defs"`
}

func mustLoadListedSandboxSchema() *schemaNode {
	var schema schemaNode
	if err := json.Unmarshal(listedSandboxSchemaData, &schema); err != nil {
		panic(fmt.Sprintf("load embedded E2B ListedSandbox schema: %v", err))
	}
	return &schema
}

func decodeListedSandbox(data []byte) (Sandbox, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return Sandbox{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Sandbox{}, err
	}
	if err := validateSchemaValue(listedSandboxSchema, listedSandboxSchema, value, "ListedSandbox"); err != nil {
		return Sandbox{}, err
	}

	var listed listedSandbox
	if err := json.Unmarshal(data, &listed); err != nil {
		return Sandbox{}, err
	}
	return listed.sandbox()
}

func validateSchemaValue(root, schema *schemaNode, value any, path string) error {
	resolved, err := resolveSchemaRef(root, schema)
	if err != nil {
		return err
	}
	schema = resolved

	switch schema.Type {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		for _, name := range schema.Required {
			if _, present := object[name]; !present {
				return fmt.Errorf("%s.%s is required", path, name)
			}
		}
		for name, child := range object {
			propertySchema, known := schema.Properties[name]
			if known {
				if err := validateSchemaValue(root, propertySchema, child, path+"."+name); err != nil {
					return err
				}
				continue
			}
			if schema.AdditionalProperties != nil {
				if err := validateSchemaValue(root, schema.AdditionalProperties, child, path+"."+name); err != nil {
					return err
				}
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		if schema.Items != nil {
			for i := range array {
				if err := validateSchemaValue(root, schema.Items, array[i], fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", path)
		}
		if len(schema.Enum) > 0 && !containsString(schema.Enum, text) {
			return fmt.Errorf("%s value %q is outside the schema enum", path, text)
		}
		if schema.Format == "date-time" {
			if _, err := time.Parse(time.RFC3339, text); err != nil {
				return fmt.Errorf("%s must use date-time format: %w", path, err)
			}
		}
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("%s must be an integer", path)
		}
		integer, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil {
			return fmt.Errorf("%s must be an integer", path)
		}
		if schema.Minimum != nil && integer < *schema.Minimum {
			return fmt.Errorf("%s must be at least %d", path, *schema.Minimum)
		}
		if schema.Format == "int32" && (integer < math.MinInt32 || integer > math.MaxInt32) {
			return fmt.Errorf("%s must fit int32", path)
		}
	case "":
		return errors.New("embedded E2B schema contains an unconstrained node")
	default:
		return fmt.Errorf("embedded E2B schema uses unsupported type %q", schema.Type)
	}
	return nil
}

func resolveSchemaRef(root, schema *schemaNode) (*schemaNode, error) {
	if schema.Ref == "" {
		return schema, nil
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(schema.Ref, prefix) {
		return nil, fmt.Errorf("embedded E2B schema uses unsupported reference %q", schema.Ref)
	}
	resolved, ok := root.Defs[strings.TrimPrefix(schema.Ref, prefix)]
	if !ok {
		return nil, fmt.Errorf("embedded E2B schema reference %q is missing", schema.Ref)
	}
	return resolved, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
