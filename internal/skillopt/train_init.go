package skillopt

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	TrainInitScaffoldDirName = "skillopt"
	TrainInitConfigFileName  = "config.toml"
	TrainInitTaskFileName    = "task.md"
	TrainInitReviewItemsName = "review-items.yml"

	TrainInitGenerationSourceCurrentSkill = "current_skill"
	TrainInitEvaluatorModeJudge           = "judge"
	TrainInitEvaluatorDimensionsAuto      = "auto"
	TrainInitSkillUpdateModeFullRewrite   = "full_rewrite_minibatch"
	TrainInitRetryOptimizerViewsAuto      = "auto"
	TrainInitBackendCodex                 = "codex"
	TrainInitInternalTargetAdapterCodex   = "codex_exec"
)

var trainInitSafeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type TrainInitConfig struct {
	Name                  string
	Template              string
	TemplateVersion       string
	ReviewRepo            string
	TaskKind              string
	ArtifactKind          string
	Preview               string
	Mode                  string
	ExplorationLevel      string
	Options               int
	Generation            TrainInitGenerationConfig
	Evaluator             TrainInitEvaluatorConfig
	Optimizer             TrainInitOptimizerConfig
	FinalEvaluatorEnabled bool
}

type TrainInitGenerationConfig struct {
	Source string
}

type TrainInitEvaluatorConfig struct {
	Mode       string
	Dimensions string
}

type TrainInitOptimizerConfig struct {
	SkillUpdateMode           string
	OptimizerViews            int
	RetryOptimizerViews       string
	NoopRetryBudget           *int
	GateRejectRetryBudget     *int
	WrongArtifactRetryBudget  *int
	TargetArtifactRetryBudget *int
	HardFailureRetryBudget    *int
	OptimizerBackend          string
	TargetBackend             string
	EvaluatorBackend          string
	OptimizerModel            string
	TargetModel               string
	InternalTargetAdapter     string
}

type TrainInitScaffold struct {
	Config          TrainInitConfig
	TaskMarkdown    string
	ReviewItemsYAML []byte
}

type TrainInitScaffoldPaths struct {
	Root            string
	ConfigPath      string
	TaskPath        string
	ReviewItemsPath string
}

func DefaultTrainInitConfig() TrainInitConfig {
	return TrainInitConfig{
		Mode:             "explore",
		ExplorationLevel: "high",
		Options:          4,
		Generation: TrainInitGenerationConfig{
			Source: TrainInitGenerationSourceCurrentSkill,
		},
		Evaluator: TrainInitEvaluatorConfig{
			Mode:       TrainInitEvaluatorModeJudge,
			Dimensions: TrainInitEvaluatorDimensionsAuto,
		},
		Optimizer: TrainInitOptimizerConfig{
			SkillUpdateMode:           TrainInitSkillUpdateModeFullRewrite,
			OptimizerViews:            4,
			RetryOptimizerViews:       TrainInitRetryOptimizerViewsAuto,
			NoopRetryBudget:           trainInitIntPtr(1),
			GateRejectRetryBudget:     trainInitIntPtr(3),
			WrongArtifactRetryBudget:  trainInitIntPtr(1),
			TargetArtifactRetryBudget: trainInitIntPtr(2),
			HardFailureRetryBudget:    trainInitIntPtr(3),
			OptimizerBackend:          TrainInitBackendCodex,
			TargetBackend:             TrainInitBackendCodex,
			EvaluatorBackend:          TrainInitBackendCodex,
			InternalTargetAdapter:     TrainInitInternalTargetAdapterCodex,
		},
		FinalEvaluatorEnabled: false,
	}
}

func ValidateTrainInitName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("train init name is required")
	}
	if trimmed != name {
		return fmt.Errorf("train init name %q must not contain surrounding whitespace", name)
	}
	name = trimmed
	if !trainInitSafeNamePattern.MatchString(name) {
		return fmt.Errorf("train init name %q must start with a letter or number and contain only letters, numbers, dot, underscore, or hyphen", name)
	}
	if name == "." || name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("train init name %q must not contain path traversal", name)
	}
	if strings.ContainsAny(name, `/\`) || filepath.IsAbs(name) {
		return fmt.Errorf("train init name %q must be a single path segment", name)
	}
	return nil
}

func TrainInitScaffoldRoot(workspaceRoot string, name string) (string, error) {
	if err := ValidateTrainInitName(name); err != nil {
		return "", err
	}
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		return "", errors.New("workspace root is required")
	}
	base, err := filepath.Abs(filepath.Join(workspaceRoot, ".gitmoot", TrainInitScaffoldDirName))
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(base, name))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return "", fmt.Errorf("verify scaffold path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("train init scaffold path %q escapes %s", target, base)
	}
	return target, nil
}

func RenderTrainInitConfig(config TrainInitConfig) ([]byte, error) {
	config = normalizeTrainInitConfig(config)
	if err := validateTrainInitConfigForRender(config); err != nil {
		return nil, err
	}
	var b strings.Builder
	writeTrainInitString(&b, "name", config.Name)
	writeTrainInitString(&b, "template", config.Template)
	writeTrainInitString(&b, "template_version", config.TemplateVersion)
	writeTrainInitString(&b, "review_repo", config.ReviewRepo)
	writeTrainInitString(&b, "task_kind", config.TaskKind)
	writeTrainInitString(&b, "artifact_kind", config.ArtifactKind)
	writeTrainInitString(&b, "preview", config.Preview)
	writeTrainInitString(&b, "mode", config.Mode)
	writeTrainInitString(&b, "exploration_level", config.ExplorationLevel)
	writeTrainInitInt(&b, "options", config.Options)
	b.WriteString("\n[generation]\n")
	writeTrainInitString(&b, "source", config.Generation.Source)
	b.WriteString("\n[evaluator]\n")
	writeTrainInitString(&b, "mode", config.Evaluator.Mode)
	writeTrainInitString(&b, "dimensions", config.Evaluator.Dimensions)
	b.WriteString("\n[optimizer]\n")
	writeTrainInitString(&b, "skill_update_mode", config.Optimizer.SkillUpdateMode)
	writeTrainInitInt(&b, "optimizer_views", config.Optimizer.OptimizerViews)
	writeTrainInitString(&b, "retry_optimizer_views", config.Optimizer.RetryOptimizerViews)
	writeTrainInitInt(&b, "noop_retry_budget", trainInitIntValue(config.Optimizer.NoopRetryBudget))
	writeTrainInitInt(&b, "gate_reject_retry_budget", trainInitIntValue(config.Optimizer.GateRejectRetryBudget))
	writeTrainInitInt(&b, "wrong_artifact_retry_budget", trainInitIntValue(config.Optimizer.WrongArtifactRetryBudget))
	writeTrainInitInt(&b, "target_artifact_retry_budget", trainInitIntValue(config.Optimizer.TargetArtifactRetryBudget))
	writeTrainInitInt(&b, "hard_failure_retry_budget", trainInitIntValue(config.Optimizer.HardFailureRetryBudget))
	writeTrainInitString(&b, "optimizer_backend", config.Optimizer.OptimizerBackend)
	writeTrainInitString(&b, "target_backend", config.Optimizer.TargetBackend)
	writeTrainInitString(&b, "evaluator_backend", config.Optimizer.EvaluatorBackend)
	writeTrainInitString(&b, "optimizer_model", config.Optimizer.OptimizerModel)
	writeTrainInitString(&b, "target_model", config.Optimizer.TargetModel)
	writeTrainInitString(&b, "internal_target_adapter", config.Optimizer.InternalTargetAdapter)
	b.WriteString("\n[final_evaluator]\n")
	writeTrainInitBool(&b, "enabled", config.FinalEvaluatorEnabled)
	return []byte(b.String()), nil
}

func ParseTrainInitConfig(content []byte) (TrainInitConfig, error) {
	values, err := parseTrainInitTOML(content)
	if err != nil {
		return TrainInitConfig{}, err
	}
	config := DefaultTrainInitConfig()
	var required []string
	getString := func(key string, requiredField bool) string {
		value, ok := values[key]
		if !ok {
			if requiredField {
				required = append(required, key)
			}
			return ""
		}
		return value
	}
	config.Name = getString("name", true)
	config.Template = getString("template", true)
	config.TemplateVersion = getString("template_version", true)
	config.ReviewRepo = getString("review_repo", true)
	config.TaskKind = getString("task_kind", true)
	config.ArtifactKind = getString("artifact_kind", true)
	config.Preview = getString("preview", true)
	config.Mode = getString("mode", true)
	if value := getString("exploration_level", false); value != "" {
		config.ExplorationLevel = value
	}
	if value := getString("options", false); value != "" {
		options, err := strconv.Atoi(value)
		if err != nil {
			return TrainInitConfig{}, fmt.Errorf("options must be an integer: %w", err)
		}
		config.Options = options
	}
	if value := getString("generation.source", false); value != "" {
		config.Generation.Source = value
	}
	if value := getString("evaluator.mode", false); value != "" {
		config.Evaluator.Mode = value
	}
	if value := getString("evaluator.dimensions", false); value != "" {
		config.Evaluator.Dimensions = value
	}
	if value := getString("optimizer.skill_update_mode", false); value != "" {
		config.Optimizer.SkillUpdateMode = value
	}
	parseInt := func(key string, dest *int) error {
		value := getString(key, false)
		if value == "" {
			return nil
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s must be an integer: %w", key, err)
		}
		*dest = parsed
		return nil
	}
	parseIntPtr := func(key string, dest **int) error {
		value := getString(key, false)
		if value == "" {
			return nil
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s must be an integer: %w", key, err)
		}
		*dest = trainInitIntPtr(parsed)
		return nil
	}
	if err := parseInt("optimizer.optimizer_views", &config.Optimizer.OptimizerViews); err != nil {
		return TrainInitConfig{}, err
	}
	if value := getString("optimizer.retry_optimizer_views", false); value != "" {
		config.Optimizer.RetryOptimizerViews = value
	}
	if err := parseIntPtr("optimizer.noop_retry_budget", &config.Optimizer.NoopRetryBudget); err != nil {
		return TrainInitConfig{}, err
	}
	if err := parseIntPtr("optimizer.gate_reject_retry_budget", &config.Optimizer.GateRejectRetryBudget); err != nil {
		return TrainInitConfig{}, err
	}
	if err := parseIntPtr("optimizer.wrong_artifact_retry_budget", &config.Optimizer.WrongArtifactRetryBudget); err != nil {
		return TrainInitConfig{}, err
	}
	if err := parseIntPtr("optimizer.target_artifact_retry_budget", &config.Optimizer.TargetArtifactRetryBudget); err != nil {
		return TrainInitConfig{}, err
	}
	if err := parseIntPtr("optimizer.hard_failure_retry_budget", &config.Optimizer.HardFailureRetryBudget); err != nil {
		return TrainInitConfig{}, err
	}
	if value := getString("optimizer.optimizer_backend", false); value != "" {
		config.Optimizer.OptimizerBackend = value
	}
	if value := getString("optimizer.target_backend", false); value != "" {
		config.Optimizer.TargetBackend = value
	}
	if value := getString("optimizer.evaluator_backend", false); value != "" {
		config.Optimizer.EvaluatorBackend = value
	}
	if value := getString("optimizer.optimizer_model", false); value != "" {
		config.Optimizer.OptimizerModel = value
	}
	if value := getString("optimizer.target_model", false); value != "" {
		config.Optimizer.TargetModel = value
	}
	if value := getString("optimizer.internal_target_adapter", false); value != "" {
		config.Optimizer.InternalTargetAdapter = value
	}
	if value := getString("final_evaluator.enabled", false); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return TrainInitConfig{}, fmt.Errorf("final_evaluator.enabled must be a boolean: %w", err)
		}
		config.FinalEvaluatorEnabled = enabled
	}
	if len(required) > 0 {
		sort.Strings(required)
		return TrainInitConfig{}, fmt.Errorf("train init config missing required fields: %s", strings.Join(required, ", "))
	}
	if err := validateTrainInitConfigForRender(config); err != nil {
		return TrainInitConfig{}, err
	}
	return config, nil
}

func LoadTrainInitConfig(path string) (TrainInitConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return TrainInitConfig{}, err
	}
	return ParseTrainInitConfig(content)
}

func WriteTrainInitScaffold(workspaceRoot string, scaffold TrainInitScaffold) (TrainInitScaffoldPaths, error) {
	scaffold.Config = normalizeTrainInitConfig(scaffold.Config)
	root, err := TrainInitScaffoldRoot(workspaceRoot, scaffold.Config.Name)
	if err != nil {
		return TrainInitScaffoldPaths{}, err
	}
	configContent, err := RenderTrainInitConfig(scaffold.Config)
	if err != nil {
		return TrainInitScaffoldPaths{}, err
	}
	if strings.TrimSpace(scaffold.TaskMarkdown) == "" {
		return TrainInitScaffoldPaths{}, errors.New("train init task.md content is required")
	}
	if err := rejectExistingTrainInitSymlinks(workspaceRoot, scaffold.Config.Name); err != nil {
		return TrainInitScaffoldPaths{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return TrainInitScaffoldPaths{}, err
	}
	if err := rejectExistingTrainInitSymlinks(workspaceRoot, scaffold.Config.Name); err != nil {
		return TrainInitScaffoldPaths{}, err
	}
	paths := TrainInitScaffoldPaths{
		Root:            root,
		ConfigPath:      filepath.Join(root, TrainInitConfigFileName),
		TaskPath:        filepath.Join(root, TrainInitTaskFileName),
		ReviewItemsPath: filepath.Join(root, TrainInitReviewItemsName),
	}
	if err := writeTrainInitScaffoldFile(paths.ConfigPath, configContent); err != nil {
		return TrainInitScaffoldPaths{}, err
	}
	if err := writeTrainInitScaffoldFile(paths.TaskPath, []byte(strings.TrimRight(scaffold.TaskMarkdown, "\n")+"\n")); err != nil {
		return TrainInitScaffoldPaths{}, err
	}
	if len(bytes.TrimSpace(scaffold.ReviewItemsYAML)) > 0 {
		content := append(bytes.TrimRight(scaffold.ReviewItemsYAML, "\n"), '\n')
		if err := writeTrainInitScaffoldFile(paths.ReviewItemsPath, content); err != nil {
			return TrainInitScaffoldPaths{}, err
		}
	} else if err := removeTrainInitScaffoldFile(paths.ReviewItemsPath); err != nil {
		return TrainInitScaffoldPaths{}, err
	}
	return paths, nil
}

func normalizeTrainInitConfig(config TrainInitConfig) TrainInitConfig {
	defaults := DefaultTrainInitConfig()
	config.Template = strings.TrimSpace(config.Template)
	config.TemplateVersion = strings.TrimSpace(config.TemplateVersion)
	config.ReviewRepo = strings.TrimSpace(config.ReviewRepo)
	config.TaskKind = strings.TrimSpace(config.TaskKind)
	config.ArtifactKind = strings.TrimSpace(config.ArtifactKind)
	config.Preview = strings.TrimSpace(config.Preview)
	config.Mode = strings.TrimSpace(config.Mode)
	if config.Mode == "" {
		config.Mode = defaults.Mode
	}
	config.ExplorationLevel = strings.TrimSpace(config.ExplorationLevel)
	if config.ExplorationLevel == "" {
		config.ExplorationLevel = defaults.ExplorationLevel
	}
	if config.Options == 0 {
		config.Options = defaults.Options
	}
	config.Generation.Source = strings.TrimSpace(config.Generation.Source)
	if config.Generation.Source == "" {
		config.Generation.Source = defaults.Generation.Source
	}
	config.Evaluator.Mode = strings.TrimSpace(config.Evaluator.Mode)
	if config.Evaluator.Mode == "" {
		config.Evaluator.Mode = defaults.Evaluator.Mode
	}
	config.Evaluator.Dimensions = strings.TrimSpace(config.Evaluator.Dimensions)
	if config.Evaluator.Dimensions == "" {
		config.Evaluator.Dimensions = defaults.Evaluator.Dimensions
	}
	config.Optimizer.SkillUpdateMode = strings.TrimSpace(config.Optimizer.SkillUpdateMode)
	if config.Optimizer.SkillUpdateMode == "" {
		config.Optimizer.SkillUpdateMode = defaults.Optimizer.SkillUpdateMode
	}
	if config.Optimizer.OptimizerViews == 0 {
		config.Optimizer.OptimizerViews = defaults.Optimizer.OptimizerViews
	}
	config.Optimizer.RetryOptimizerViews = strings.TrimSpace(config.Optimizer.RetryOptimizerViews)
	if config.Optimizer.RetryOptimizerViews == "" {
		config.Optimizer.RetryOptimizerViews = defaults.Optimizer.RetryOptimizerViews
	}
	if config.Optimizer.NoopRetryBudget == nil {
		config.Optimizer.NoopRetryBudget = defaults.Optimizer.NoopRetryBudget
	}
	if config.Optimizer.GateRejectRetryBudget == nil {
		config.Optimizer.GateRejectRetryBudget = defaults.Optimizer.GateRejectRetryBudget
	}
	if config.Optimizer.WrongArtifactRetryBudget == nil {
		config.Optimizer.WrongArtifactRetryBudget = defaults.Optimizer.WrongArtifactRetryBudget
	}
	if config.Optimizer.TargetArtifactRetryBudget == nil {
		config.Optimizer.TargetArtifactRetryBudget = defaults.Optimizer.TargetArtifactRetryBudget
	}
	if config.Optimizer.HardFailureRetryBudget == nil {
		config.Optimizer.HardFailureRetryBudget = defaults.Optimizer.HardFailureRetryBudget
	}
	config.Optimizer.OptimizerBackend = strings.TrimSpace(config.Optimizer.OptimizerBackend)
	if config.Optimizer.OptimizerBackend == "" {
		config.Optimizer.OptimizerBackend = defaults.Optimizer.OptimizerBackend
	}
	config.Optimizer.TargetBackend = strings.TrimSpace(config.Optimizer.TargetBackend)
	if config.Optimizer.TargetBackend == "" {
		config.Optimizer.TargetBackend = defaults.Optimizer.TargetBackend
	}
	config.Optimizer.EvaluatorBackend = strings.TrimSpace(config.Optimizer.EvaluatorBackend)
	if config.Optimizer.EvaluatorBackend == "" {
		config.Optimizer.EvaluatorBackend = defaults.Optimizer.EvaluatorBackend
	}
	config.Optimizer.InternalTargetAdapter = strings.TrimSpace(config.Optimizer.InternalTargetAdapter)
	if config.Optimizer.InternalTargetAdapter == "" {
		config.Optimizer.InternalTargetAdapter = defaults.Optimizer.InternalTargetAdapter
	}
	return config
}

func validateTrainInitConfigForRender(config TrainInitConfig) error {
	if err := ValidateTrainInitName(config.Name); err != nil {
		return err
	}
	missing := []string{}
	for field, value := range map[string]string{
		"template":         config.Template,
		"template_version": config.TemplateVersion,
		"review_repo":      config.ReviewRepo,
		"task_kind":        config.TaskKind,
		"artifact_kind":    config.ArtifactKind,
		"preview":          config.Preview,
		"mode":             config.Mode,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("train init config missing required fields: %s", strings.Join(missing, ", "))
	}
	if config.Options < 2 {
		return errors.New("train init options must be at least 2")
	}
	if config.Optimizer.OptimizerViews < 1 {
		return errors.New("train init optimizer_views must be at least 1")
	}
	for field, value := range map[string]int{
		"noop_retry_budget":            trainInitIntValue(config.Optimizer.NoopRetryBudget),
		"gate_reject_retry_budget":     trainInitIntValue(config.Optimizer.GateRejectRetryBudget),
		"wrong_artifact_retry_budget":  trainInitIntValue(config.Optimizer.WrongArtifactRetryBudget),
		"target_artifact_retry_budget": trainInitIntValue(config.Optimizer.TargetArtifactRetryBudget),
		"hard_failure_retry_budget":    trainInitIntValue(config.Optimizer.HardFailureRetryBudget),
	} {
		if value < 0 {
			return fmt.Errorf("train init %s must be zero or greater", field)
		}
	}
	return nil
}

func rejectExistingTrainInitSymlinks(workspaceRoot string, name string) error {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		return errors.New("workspace root is required")
	}
	workspaceRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return err
	}
	components := []string{".gitmoot", TrainInitScaffoldDirName, name}
	current := workspaceRoot
	for _, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("inspect train init scaffold path %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("train init scaffold path %s is a symlink; refusing to write outside .gitmoot/%s", current, TrainInitScaffoldDirName)
		}
	}
	return nil
}

func writeTrainInitScaffoldFile(path string, content []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("train init scaffold file %s is a symlink; refusing to follow it", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect train init scaffold file %s: %w", path, err)
	}
	file, err := openTrainInitScaffoldFileNoFollow(path)
	if err != nil {
		return fmt.Errorf("write train init scaffold file %s: %w", path, err)
	}
	defer file.Close()
	if _, err := file.Write(content); err != nil {
		return fmt.Errorf("write train init scaffold file %s: %w", path, err)
	}
	return nil
}

func removeTrainInitScaffoldFile(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("train init scaffold file %s is a symlink; refusing to follow it", path)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		return nil
	} else {
		return fmt.Errorf("inspect train init scaffold file %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale train init scaffold file %s: %w", path, err)
	}
	return nil
}

func trainInitIntPtr(value int) *int {
	return &value
}

func trainInitIntValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func writeTrainInitString(builder *strings.Builder, key string, value string) {
	builder.WriteString(key)
	builder.WriteString(" = ")
	builder.WriteString(strconv.Quote(value))
	builder.WriteByte('\n')
}

func writeTrainInitInt(builder *strings.Builder, key string, value int) {
	builder.WriteString(key)
	builder.WriteString(" = ")
	builder.WriteString(strconv.Itoa(value))
	builder.WriteByte('\n')
}

func writeTrainInitBool(builder *strings.Builder, key string, value bool) {
	builder.WriteString(key)
	builder.WriteString(" = ")
	builder.WriteString(strconv.FormatBool(value))
	builder.WriteByte('\n')
}

func parseTrainInitTOML(content []byte) (map[string]string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	values := map[string]string{}
	section := ""
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			if section == "" || strings.Contains(section, ".") {
				return nil, fmt.Errorf("line %d: unsupported section %q", lineNumber, section)
			}
			continue
		}
		key, rawValue, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", lineNumber)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: key is required", lineNumber)
		}
		if section != "" {
			key = section + "." + key
		}
		value, err := parseTrainInitTOMLValue(stripTrainInitInlineComment(strings.TrimSpace(rawValue)))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func stripTrainInitInlineComment(raw string) string {
	inString := false
	escaped := false
	for index, r := range raw {
		switch {
		case escaped:
			escaped = false
		case inString && r == '\\':
			escaped = true
		case r == '"':
			inString = !inString
		case !inString && r == '#':
			return strings.TrimSpace(raw[:index])
		}
	}
	return strings.TrimSpace(raw)
}

func parseTrainInitTOMLValue(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("value is required")
	}
	if strings.HasPrefix(raw, `"`) {
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", err
		}
		return value, nil
	}
	if strings.EqualFold(raw, "true") || strings.EqualFold(raw, "false") {
		return strings.ToLower(raw), nil
	}
	if _, err := strconv.Atoi(raw); err == nil {
		return raw, nil
	}
	return "", fmt.Errorf("unsupported value %q", raw)
}
