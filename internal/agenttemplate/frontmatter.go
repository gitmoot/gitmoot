package agenttemplate

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const TemplateKind = "agent-template"
const TemplateVersion = 1

var validTemplateCapabilities = map[string]struct{}{
	"ask":       {},
	"review":    {},
	"implement": {},
}

var validTemplateRuntimes = map[string]struct{}{
	"codex":  {},
	"claude": {},
	"shell":  {},
}

type Metadata struct {
	ID                   string            `json:"id" yaml:"id"`
	Name                 string            `json:"name" yaml:"name"`
	Description          string            `json:"description" yaml:"description"`
	Kind                 string            `json:"kind" yaml:"kind"`
	Version              int               `json:"version" yaml:"version"`
	Capabilities         []string          `json:"capabilities" yaml:"capabilities"`
	RuntimeCompatibility []string          `json:"runtime_compatibility" yaml:"runtime_compatibility"`
	Tags                 []string          `json:"tags" yaml:"tags"`
	Inputs               []string          `json:"inputs" yaml:"inputs"`
	Outputs              []string          `json:"outputs" yaml:"outputs"`
	Evaluation           map[string]string `json:"evaluation,omitempty" yaml:"evaluation,omitempty"`
}

type ParsedTemplate struct {
	Metadata Metadata
	Body     string
}

func ParseTemplateContent(content string) (ParsedTemplate, error) {
	frontmatter, body, err := splitFrontmatter(content)
	if err != nil {
		return ParsedTemplate{}, err
	}
	var metadata Metadata
	if err := yaml.Unmarshal([]byte(frontmatter), &metadata); err != nil {
		return ParsedTemplate{}, fmt.Errorf("parse template frontmatter: %w", err)
	}
	metadata = normalizeMetadata(metadata)
	if err := validateMetadata(metadata); err != nil {
		return ParsedTemplate{}, err
	}
	if strings.TrimSpace(body) == "" {
		return ParsedTemplate{}, errors.New("template body is empty")
	}
	return ParsedTemplate{Metadata: metadata, Body: body}, nil
}

func MarshalMetadata(metadata Metadata) (string, error) {
	metadata = normalizeMetadata(metadata)
	if err := validateMetadata(metadata); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode template metadata: %w", err)
	}
	return string(encoded), nil
}

func UnmarshalMetadata(content string) (Metadata, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Metadata{}, errors.New("template metadata is empty")
	}
	var metadata Metadata
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		return Metadata{}, fmt.Errorf("decode template metadata: %w", err)
	}
	metadata = normalizeMetadata(metadata)
	if err := validateMetadata(metadata); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func splitFrontmatter(content string) (string, string, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimSpace(content)
	if content == "" {
		return "", "", errors.New("template content is empty")
	}
	lines := strings.Split(content, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", errors.New("template must start with YAML frontmatter")
	}
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) != "---" {
			continue
		}
		frontmatter := strings.Join(lines[1:index], "\n")
		body := strings.TrimLeft(strings.Join(lines[index+1:], "\n"), "\n")
		return frontmatter, body, nil
	}
	return "", "", errors.New("template frontmatter is missing closing ---")
}

func normalizeMetadata(metadata Metadata) Metadata {
	metadata.ID = strings.TrimSpace(metadata.ID)
	metadata.Name = strings.TrimSpace(metadata.Name)
	metadata.Description = strings.TrimSpace(metadata.Description)
	metadata.Kind = strings.TrimSpace(metadata.Kind)
	metadata.Capabilities = compactMetadataStrings(metadata.Capabilities)
	metadata.RuntimeCompatibility = compactMetadataStrings(metadata.RuntimeCompatibility)
	metadata.Tags = compactMetadataStrings(metadata.Tags)
	metadata.Inputs = compactMetadataStrings(metadata.Inputs)
	metadata.Outputs = compactMetadataStrings(metadata.Outputs)
	if len(metadata.Evaluation) > 0 {
		normalized := make(map[string]string, len(metadata.Evaluation))
		for key, value := range metadata.Evaluation {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key != "" && value != "" {
				normalized[key] = value
			}
		}
		metadata.Evaluation = normalized
	}
	return metadata
}

func validateMetadata(metadata Metadata) error {
	if err := ValidateID(metadata.ID); err != nil {
		return err
	}
	if metadata.Name == "" {
		return errors.New("template frontmatter missing name")
	}
	if metadata.Description == "" {
		return errors.New("template frontmatter missing description")
	}
	if metadata.Kind != TemplateKind {
		return fmt.Errorf("template kind must be %q", TemplateKind)
	}
	if metadata.Version != TemplateVersion {
		return fmt.Errorf("template version must be %d", TemplateVersion)
	}
	if len(metadata.Capabilities) == 0 {
		return errors.New("template frontmatter missing capabilities")
	}
	if err := validateKnownValues("capability", metadata.Capabilities, validTemplateCapabilities); err != nil {
		return err
	}
	if len(metadata.RuntimeCompatibility) == 0 {
		return errors.New("template frontmatter missing runtime_compatibility")
	}
	if err := validateKnownValues("runtime_compatibility", metadata.RuntimeCompatibility, validTemplateRuntimes); err != nil {
		return err
	}
	if len(metadata.Tags) == 0 {
		return errors.New("template frontmatter missing tags")
	}
	if len(metadata.Inputs) == 0 {
		return errors.New("template frontmatter missing inputs")
	}
	if len(metadata.Outputs) == 0 {
		return errors.New("template frontmatter missing outputs")
	}
	return nil
}

func validateKnownValues(label string, values []string, allowed map[string]struct{}) error {
	for _, value := range values {
		if _, ok := allowed[value]; !ok {
			return fmt.Errorf("template frontmatter has invalid %s %q", label, value)
		}
	}
	return nil
}

func compactMetadataStrings(values []string) []string {
	compacted := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			compacted = append(compacted, value)
		}
	}
	return compacted
}

func FormatTemplateContent(metadata Metadata, body string) string {
	metadata = normalizeMetadata(metadata)
	var builder strings.Builder
	builder.WriteString("---\n")
	writeYAMLScalar(&builder, "id", metadata.ID)
	writeYAMLScalar(&builder, "name", metadata.Name)
	writeYAMLScalar(&builder, "description", metadata.Description)
	writeYAMLScalar(&builder, "kind", metadata.Kind)
	builder.WriteString("version: ")
	builder.WriteString(fmt.Sprint(metadata.Version))
	builder.WriteByte('\n')
	writeYAMLList(&builder, "capabilities", metadata.Capabilities)
	writeYAMLList(&builder, "runtime_compatibility", metadata.RuntimeCompatibility)
	writeYAMLList(&builder, "tags", metadata.Tags)
	writeYAMLList(&builder, "inputs", metadata.Inputs)
	writeYAMLList(&builder, "outputs", metadata.Outputs)
	if len(metadata.Evaluation) > 0 {
		builder.WriteString("evaluation:\n")
		keys := make([]string, 0, len(metadata.Evaluation))
		for key := range metadata.Evaluation {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := metadata.Evaluation[key]
			writeYAMLScalar(&builder, "  "+key, value)
		}
	}
	builder.WriteString("---\n\n")
	builder.WriteString(strings.TrimSpace(body))
	builder.WriteByte('\n')
	return builder.String()
}

func writeYAMLScalar(builder *strings.Builder, key string, value string) {
	builder.WriteString(key)
	builder.WriteString(": ")
	encoded, err := yaml.Marshal(value)
	if err != nil {
		builder.WriteString("\"")
		builder.WriteString(strings.ReplaceAll(value, "\"", "\\\""))
		builder.WriteString("\"\n")
		return
	}
	builder.WriteString(strings.TrimSpace(string(encoded)))
	builder.WriteByte('\n')
}

func writeYAMLList(builder *strings.Builder, key string, values []string) {
	builder.WriteString(key)
	builder.WriteString(":\n")
	for _, value := range values {
		builder.WriteString("  - ")
		encoded, err := yaml.Marshal(value)
		if err != nil {
			builder.WriteString("\"")
			builder.WriteString(strings.ReplaceAll(value, "\"", "\\\""))
			builder.WriteString("\"\n")
			continue
		}
		builder.WriteString(strings.TrimSpace(string(encoded)))
		builder.WriteByte('\n')
	}
}
