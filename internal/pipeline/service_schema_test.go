package pipeline

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const validServiceSchemaJSON = `{"version":1,"fields":{"app_name":{"type":"string","required":true,"max_length":120},"count":{"type":"integer","minimum":1,"maximum":5},"draft":{"type":"boolean"},"locale":{"type":"string","max_length":2,"enum":["en","de"]}}}`

func TestParseServiceSchemaFirewall(t *testing.T) {
	schema, err := ParseServiceSchema([]byte(validServiceSchemaJSON))
	if err != nil {
		t.Fatalf("ParseServiceSchema(valid): %v", err)
	}
	if schema.Version != 1 || len(schema.Fields) != 4 || schema.Fields["app_name"].MaxLength != 120 {
		t.Fatalf("parsed schema = %+v", schema)
	}
	canonical, err := schema.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "default") || strings.Contains(string(canonical), "regex") {
		t.Fatalf("canonical schema carried an unsupported construct: %s", canonical)
	}

	manyFields := make([]string, 33)
	for i := range manyFields {
		manyFields[i] = fmt.Sprintf(`"field_%d":{"type":"boolean"}`, i)
	}
	tests := []struct {
		name string
		raw  string
	}{
		{"float version", `{"version":1.5,"fields":{}}`},
		{"float bound", `{"version":1,"fields":{"count":{"type":"integer","minimum":1.5}}}`},
		{"object type", `{"version":1,"fields":{"body":{"type":"object"}}}`},
		{"array type", `{"version":1,"fields":{"items":{"type":"array"}}}`},
		{"default", `{"version":1,"fields":{"draft":{"type":"boolean","default":true}}}`},
		{"regex", `{"version":1,"fields":{"name":{"type":"string","max_length":12,"regex":".*"}}}`},
		{"unknown type", `{"version":1,"fields":{"count":{"type":"number"}}}`},
		{"caller env", `{"version":1,"fields":{"name":{"type":"string","max_length":12,"env":"PATH"}}}`},
		{"non snake", `{"version":1,"fields":{"AppName":{"type":"string","max_length":12}}}`},
		{"too many", `{"version":1,"fields":{` + strings.Join(manyFields, ",") + `}}`},
		{"unbounded string", `{"version":1,"fields":{"name":{"type":"string"}}}`},
		{"excessive string bound", `{"version":1,"fields":{"name":{"type":"string","max_length":32769}}}`},
		{"duplicate key", `{"version":1,"version":1,"fields":{}}`},
		{"nested duplicate key", `{"version":1,"fields":{"draft":{"type":"boolean","type":"boolean"}}}`},
		{"unknown top level", `{"version":1,"fields":{},"title":"no"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseServiceSchema([]byte(test.raw)); err == nil {
				t.Fatalf("ParseServiceSchema accepted %s", test.raw)
			}
		})
	}
}

func TestValidateServiceInputTypedAndValueFree(t *testing.T) {
	schema, err := ParseServiceSchema([]byte(validServiceSchemaJSON))
	if err != nil {
		t.Fatal(err)
	}
	values, fieldErrors := ValidateServiceInput(schema, []byte(`{"app_name":"Acme","count":3,"draft":false,"locale":"de"}`), DefaultServiceInputMaxBody)
	if len(fieldErrors) != 0 {
		t.Fatalf("good input errors = %+v", fieldErrors)
	}
	wantEnv := []string{
		"GITMOOT_INPUT_APP_NAME=Acme",
		"GITMOOT_INPUT_COUNT=3",
		"GITMOOT_INPUT_DRAFT=false",
		"GITMOOT_INPUT_LOCALE=de",
	}
	if got := ServiceInputEnvironment(values); !reflect.DeepEqual(got, wantEnv) {
		t.Fatalf("service env = %#v, want %#v", got, wantEnv)
	}

	const secretValue = "DO-NOT-ECHO-THIS-VALUE"
	_, fieldErrors = ValidateServiceInput(schema, []byte(`{"unknown":"`+secretValue+`","count":"`+secretValue+`","locale":"fr"}`), DefaultServiceInputMaxBody)
	want := []FieldError{
		{Field: "app_name", Code: "required", Message: "is required"},
		{Field: "count", Code: "type", Message: "must be an integer"},
		{Field: "locale", Code: "enum", Message: "must be one of the declared values"},
		{Field: "unknown", Code: "unknown", Message: "is not allowed"},
	}
	if !reflect.DeepEqual(fieldErrors, want) {
		t.Fatalf("bad input errors = %#v, want %#v", fieldErrors, want)
	}
	diagnostic, err := json.Marshal(NewInputDiagnostic(fieldErrors))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(diagnostic), secretValue) {
		t.Fatalf("diagnostic leaked caller value: %s", diagnostic)
	}
	if !strings.HasPrefix(string(diagnostic), `{"error":"invalid_input","fields":[`) {
		t.Fatalf("diagnostic shape = %s", diagnostic)
	}
}

func TestValidateServiceInputFailures(t *testing.T) {
	schema, err := ParseServiceSchema([]byte(validServiceSchemaJSON))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		body string
		max  int
		code string
	}{
		{"bad type", `{"app_name":12}`, 0, "type"},
		{"unknown", `{"app_name":"ok","extra":true}`, 0, "unknown"},
		{"missing required", `{}`, 0, "required"},
		{"below minimum", `{"app_name":"ok","count":0}`, 0, "minimum"},
		{"above maximum", `{"app_name":"ok","count":6}`, 0, "maximum"},
		{"bad enum", `{"app_name":"ok","locale":"fr"}`, 0, "enum"},
		{"float input", `{"app_name":"ok","count":1.0}`, 0, "type"},
		{"nested input", `{"app_name":{"nested":true}}`, 0, "type"},
		{"array input", `{"app_name":[]}`, 0, "type"},
		{"duplicate input", `{"app_name":"a","app_name":"b"}`, 0, "malformed_json"},
		{"malformed", `{"app_name":`, 0, "malformed_json"},
		{"not object", `[]`, 0, "malformed_json"},
		{"oversized", `{"app_name":"too large"}`, 8, "body_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, fieldErrors := ValidateServiceInput(schema, []byte(test.body), test.max)
			found := false
			for _, fieldErr := range fieldErrors {
				if fieldErr.Code == test.code {
					found = true
				}
			}
			if !found {
				t.Fatalf("errors = %+v, want code %q", fieldErrors, test.code)
			}
		})
	}
}
