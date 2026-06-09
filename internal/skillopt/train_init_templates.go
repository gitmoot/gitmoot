package skillopt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/db"
)

const (
	TrainInitTemplateChoiceSourceBuiltin   = "builtin"
	TrainInitTemplateChoiceSourceInstalled = "installed"
	TrainInitTemplateChoiceSourceAgent     = "agent"
)

type TrainInitTemplateChoice struct {
	ID             string                 `json:"id"`
	Label          string                 `json:"label"`
	Source         string                 `json:"source"`
	Installed      bool                   `json:"installed"`
	Current        bool                   `json:"current"`
	CurrentVersion string                 `json:"current_version,omitempty"`
	VersionID      string                 `json:"version_id,omitempty"`
	VersionNumber  int                    `json:"version_number,omitempty"`
	VersionState   string                 `json:"version_state,omitempty"`
	Name           string                 `json:"name,omitempty"`
	Description    string                 `json:"description,omitempty"`
	SourceRepo     string                 `json:"source_repo,omitempty"`
	SourceRef      string                 `json:"source_ref,omitempty"`
	SourcePath     string                 `json:"source_path,omitempty"`
	ResolvedCommit string                 `json:"resolved_commit,omitempty"`
	ContentHash    string                 `json:"content_hash,omitempty"`
	Agents         []string               `json:"agents,omitempty"`
	Metadata       agenttemplate.Metadata `json:"metadata,omitempty"`
}

func ListTrainInitTemplateChoices(ctx context.Context, store *db.Store) ([]TrainInitTemplateChoice, error) {
	if store == nil {
		return nil, errors.New("template store is required")
	}
	installedTemplates, err := store.ListAgentTemplates(ctx)
	if err != nil {
		return nil, err
	}
	installed := map[string]db.AgentTemplate{}
	for _, template := range installedTemplates {
		installed[template.ID] = template
	}
	agentsByTemplate, err := trainInitAgentsByTemplate(ctx, store)
	if err != nil {
		return nil, err
	}
	choicesByID := map[string]TrainInitTemplateChoice{}
	for _, definition := range agenttemplate.Builtins() {
		if agenttemplate.IsRetired(definition.ID) {
			continue
		}
		template, ok := installed[definition.ID]
		choice := trainInitChoiceFromDefinition(definition, template, ok)
		choice.Agents = agentsByTemplate[definition.ID]
		choicesByID[definition.ID] = choice
	}
	for _, template := range installedTemplates {
		if agenttemplate.IsRetired(template.ID) {
			continue
		}
		if _, ok := choicesByID[template.ID]; ok {
			continue
		}
		choice := trainInitChoiceFromInstalled(template)
		choice.Agents = agentsByTemplate[template.ID]
		choicesByID[template.ID] = choice
	}
	for templateID, agents := range agentsByTemplate {
		if agenttemplate.IsRetired(templateID) {
			continue
		}
		if _, ok := choicesByID[templateID]; ok {
			continue
		}
		choicesByID[templateID] = TrainInitTemplateChoice{
			ID:        templateID,
			Label:     templateID + " (used by " + strings.Join(agents, ", ") + ")",
			Source:    TrainInitTemplateChoiceSourceAgent,
			Installed: false,
			Current:   false,
			Agents:    agents,
		}
	}
	choices := make([]TrainInitTemplateChoice, 0, len(choicesByID))
	for _, choice := range choicesByID {
		choice.Agents = sortedUniqueStrings(choice.Agents)
		choices = append(choices, choice)
	}
	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].ID != choices[j].ID {
			return choices[i].ID < choices[j].ID
		}
		return choices[i].Source < choices[j].Source
	})
	return choices, nil
}

func ResolveTrainInitTemplateChoice(ctx context.Context, store *db.Store, fetcher agenttemplate.Fetcher, templateRef string) (db.AgentTemplate, error) {
	if store == nil {
		return db.AgentTemplate{}, errors.New("template store is required")
	}
	templateID, versionRef := db.SplitAgentTemplateReference(templateRef)
	if strings.TrimSpace(templateID) == "" {
		return db.AgentTemplate{}, errors.New("template is required")
	}
	if agenttemplate.IsRetired(templateID) {
		return db.AgentTemplate{}, fmt.Errorf("agent template %s is retired; use %s", templateID, agenttemplate.PlannerTemplateID)
	}
	template, err := store.GetAgentTemplateReference(ctx, templateRef)
	if err == nil {
		return template, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return db.AgentTemplate{}, err
	}
	if _, ok := agenttemplate.Lookup(templateID); !ok {
		return db.AgentTemplate{}, fmt.Errorf("unknown or uninstalled agent template %q; run gitmoot agent template add %s --file <path>", templateID, templateID)
	}
	if strings.TrimSpace(versionRef) != "" {
		return db.AgentTemplate{}, fmt.Errorf("agent template %s is not installed; run gitmoot agent template update %s before selecting %s", templateID, templateID, templateRef)
	}
	if fetcher == nil {
		return db.AgentTemplate{}, errors.New("agent template fetcher is required to install available template")
	}
	return agenttemplate.Update(ctx, store, fetcher, templateID)
}

func trainInitChoiceFromDefinition(definition agenttemplate.Definition, template db.AgentTemplate, installed bool) TrainInitTemplateChoice {
	metadata := agenttemplate.MetadataForDefinition(definition)
	if installed {
		metadata = trainInitTemplateMetadata(template, metadata)
	}
	choice := TrainInitTemplateChoice{
		ID:          definition.ID,
		Label:       definition.Name,
		Source:      TrainInitTemplateChoiceSourceBuiltin,
		Installed:   installed,
		Current:     installed && template.VersionID != "",
		Name:        definition.Name,
		Description: definition.Description,
		SourceRepo:  definition.SourceRepo,
		SourceRef:   definition.SourceRef,
		SourcePath:  definition.SourcePath,
		Metadata:    metadata,
	}
	if installed {
		choice = applyTrainInitInstalledTemplate(choice, template)
	}
	return choice
}

func trainInitChoiceFromInstalled(template db.AgentTemplate) TrainInitTemplateChoice {
	choice := TrainInitTemplateChoice{
		ID:          template.ID,
		Label:       firstNonEmpty(template.Name, template.ID),
		Source:      TrainInitTemplateChoiceSourceInstalled,
		Installed:   true,
		Current:     template.VersionID != "",
		Name:        template.Name,
		Description: template.Description,
		Metadata:    trainInitTemplateMetadata(template, agenttemplate.Metadata{}),
	}
	return applyTrainInitInstalledTemplate(choice, template)
}

func applyTrainInitInstalledTemplate(choice TrainInitTemplateChoice, template db.AgentTemplate) TrainInitTemplateChoice {
	choice.VersionID = template.VersionID
	choice.CurrentVersion = template.VersionID
	choice.VersionNumber = template.VersionNumber
	choice.VersionState = template.VersionState
	choice.SourceRepo = template.SourceRepo
	choice.SourceRef = template.SourceRef
	choice.SourcePath = template.SourcePath
	choice.ResolvedCommit = template.ResolvedCommit
	choice.ContentHash = template.ContentHash
	if template.Name != "" {
		choice.Name = template.Name
		choice.Label = template.Name
	}
	if template.Description != "" {
		choice.Description = template.Description
	}
	return choice
}

func trainInitTemplateMetadata(template db.AgentTemplate, fallback agenttemplate.Metadata) agenttemplate.Metadata {
	if strings.TrimSpace(template.MetadataJSON) != "" {
		if metadata, err := agenttemplate.UnmarshalMetadata(template.MetadataJSON); err == nil {
			return metadata
		}
	}
	if strings.TrimSpace(template.Content) != "" {
		if parsed, err := agenttemplate.ParseTemplateContent(template.Content); err == nil {
			return parsed.Metadata
		}
	}
	return fallback
}

func trainInitAgentsByTemplate(ctx context.Context, store *db.Store) (map[string][]string, error) {
	agentsByTemplate := map[string][]string{}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	for _, agent := range agents {
		addTrainInitAgentTemplate(agentsByTemplate, agent.TemplateID, agent.Name)
	}
	instances, err := store.ListAgentInstances(ctx)
	if err != nil {
		return nil, err
	}
	for _, instance := range instances {
		addTrainInitAgentTemplate(agentsByTemplate, instance.TemplateID, instance.Name)
	}
	for templateID, agents := range agentsByTemplate {
		agentsByTemplate[templateID] = sortedUniqueStrings(agents)
	}
	return agentsByTemplate, nil
}

func addTrainInitAgentTemplate(agentsByTemplate map[string][]string, templateID string, agentName string) {
	templateID, _ = db.SplitAgentTemplateReference(templateID)
	templateID = strings.TrimSpace(templateID)
	agentName = strings.TrimSpace(agentName)
	if templateID == "" || agentName == "" {
		return
	}
	agentsByTemplate[templateID] = append(agentsByTemplate[templateID], agentName)
}

func sortedUniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	unique := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
