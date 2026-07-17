package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

const (
	defaultPipelineExposureBucketCapacity = 5
	pipelineServiceStatusURLShape         = "/v1/pipelines/runs/<run-id>"
	pipelineServiceReceiptURLShape        = "/receipts/<run-id>"
)

type pipelineExposeFieldSummary struct {
	Name      string                    `json:"name"`
	Type      pipeline.ServiceFieldType `json:"type"`
	Required  bool                      `json:"required"`
	MaxLength int                       `json:"max_length,omitempty"`
	Minimum   *int64                    `json:"minimum,omitempty"`
	Maximum   *int64                    `json:"maximum,omitempty"`
	Enum      []string                  `json:"enum,omitempty"`
}

type pipelineExposeOutput struct {
	Pipeline      string                       `json:"pipeline"`
	Enabled       bool                         `json:"enabled"`
	SchemaVersion int                          `json:"schema_version"`
	SchemaHash    string                       `json:"schema_hash"`
	Fields        []pipelineExposeFieldSummary `json:"fields"`
	Token         string                       `json:"token,omitempty"`
	TokenIssued   bool                         `json:"token_issued"`
	StatusURL     string                       `json:"status_url"`
	ReceiptURL    string                       `json:"receipt_url"`
}

func runPipelineExpose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipeline expose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	schemaFile := fs.String("schema", "", "flat versioned service schema JSON file")
	rotateToken := fs.Bool("rotate-token", false, "mint a replacement bearer token and print it once")
	disable := fs.Bool("disable", false, "store the exposure disabled")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		fmt.Fprintln(stderr, "pipeline expose requires exactly one pipeline name (flags must precede the name)")
		return 2
	}
	if strings.TrimSpace(*schemaFile) == "" {
		fmt.Fprintln(stderr, "pipeline expose requires --schema <file>")
		return 2
	}
	pipelineName := strings.TrimSpace(fs.Arg(0))
	rawSchema, err := os.ReadFile(*schemaFile)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline expose: read schema: %v\n", err)
		return 1
	}
	schema, err := pipeline.ParseServiceSchema(rawSchema)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline expose: %v\n", err)
		return 2
	}
	canonicalSchema, err := schema.CanonicalJSON()
	if err != nil {
		fmt.Fprintf(stderr, "pipeline expose: canonicalize schema: %v\n", err)
		return 1
	}
	schemaHash := pipeline.Hash(canonicalSchema)

	result := pipelineExposeOutput{
		Pipeline: pipelineName, Enabled: !*disable, SchemaVersion: schema.Version, SchemaHash: schemaHash,
		Fields: exposeFieldSummaries(schema), StatusURL: pipelineServiceStatusURLShape, ReceiptURL: pipelineServiceReceiptURLShape,
	}
	err = withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		record, ok, err := store.GetPipeline(ctx, pipelineName)
		if err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("pipeline %s not found", pipelineName)
		}
		spec, err := pipeline.Load([]byte(record.SpecYAML))
		if err != nil {
			return fmt.Errorf("stored pipeline is invalid: %w", err)
		}
		if !*disable {
			if err := servicePipelinePublicSafetyError(spec); err != nil {
				return err
			}
		}
		token, created, err := store.CreateExposure(ctx, db.PipelineExposure{
			PipelineName: pipelineName, SchemaVersion: schema.Version, SchemaJSON: string(canonicalSchema), SchemaHash: schemaHash,
			Enabled: !*disable, BucketTokens: defaultPipelineExposureBucketCapacity, BucketUpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			return err
		}
		if created {
			result.Token = token
			result.TokenIssued = true
			return nil
		}
		if *rotateToken {
			token, err = store.RotateExposureToken(ctx, pipelineName)
			if err != nil {
				return err
			}
			result.Token = token
			result.TokenIssued = true
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "pipeline expose: %v\n", err)
		return 1
	}

	if *jsonOutput {
		encoded, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(stderr, "pipeline expose: encode result: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	}
	writeLine(stdout, "pipeline: %s", result.Pipeline)
	writeLine(stdout, "status: %s", enabledLabel(result.Enabled))
	writeLine(stdout, "schema: v%d %s", result.SchemaVersion, result.SchemaHash)
	if len(result.Fields) == 0 {
		writeLine(stdout, "fields: -")
	} else {
		writeLine(stdout, "fields:")
		for _, field := range result.Fields {
			writeLine(stdout, "  %s: %s%s", field.Name, field.Type, exposeFieldConstraint(field))
		}
	}
	if result.TokenIssued {
		writeLine(stdout, "token: %s (shown once)", result.Token)
	} else {
		writeLine(stdout, "token: - (unchanged; use --rotate-token to replace)")
	}
	writeLine(stdout, "status_url: %s", result.StatusURL)
	writeLine(stdout, "receipt_url: %s", result.ReceiptURL)
	return 0
}

func exposeFieldSummaries(schema pipeline.ServiceSchema) []pipelineExposeFieldSummary {
	keys := make([]string, 0, len(schema.Fields))
	for name := range schema.Fields {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	fields := make([]pipelineExposeFieldSummary, 0, len(keys))
	for _, name := range keys {
		field := schema.Fields[name]
		fields = append(fields, pipelineExposeFieldSummary{
			Name: name, Type: field.Type, Required: field.Required, MaxLength: field.MaxLength,
			Minimum: field.Minimum, Maximum: field.Maximum, Enum: append([]string(nil), field.Enum...),
		})
	}
	return fields
}

func exposeFieldConstraint(field pipelineExposeFieldSummary) string {
	parts := make([]string, 0, 3)
	if field.Required {
		parts = append(parts, "required")
	}
	if field.MaxLength > 0 {
		parts = append(parts, fmt.Sprintf("max_length=%d", field.MaxLength))
	}
	if field.Minimum != nil {
		parts = append(parts, fmt.Sprintf("minimum=%d", *field.Minimum))
	}
	if field.Maximum != nil {
		parts = append(parts, fmt.Sprintf("maximum=%d", *field.Maximum))
	}
	if len(field.Enum) > 0 {
		parts = append(parts, "enum="+strings.Join(field.Enum, "|"))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
