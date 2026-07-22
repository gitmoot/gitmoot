package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/skillopt"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

var skillOptTrainOptimizerRunner subprocess.Runner = subprocess.ExecRunner{}

func runSkillOptTrainRecover(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train recover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	sessionID := fs.String("session", "", "train session id")
	outRoot := fs.String("out-root", "", "optimizer output directory; defaults to the persisted train optimizer path")
	generation := fs.Bool("generation", false, "recover the option-generation phase: reclaim a stranded generation lock and salvage persisted options instead of the optimizer phase")
	abort := fs.Bool("abort", false, "with --generation, reclaim the stranded generation lock and leave the iteration at items_ready (does not advance state)")
	advanceState := fs.Bool("advance-state", false, "with --generation, advance the iteration to options_generated when every expected item is recovered")
	jsonOutput := fs.Bool("json", false, "print recovery result as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train recover does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*sessionID) == "" {
		fmt.Fprintln(stderr, "skillopt train recover requires --session")
		return 2
	}
	if !*generation && (*abort || *advanceState) {
		fmt.Fprintln(stderr, "skillopt train recover --abort and --advance-state require --generation")
		return 2
	}
	if *abort && *advanceState {
		fmt.Fprintln(stderr, "skillopt train recover --abort and --advance-state are mutually exclusive")
		return 2
	}
	var result skillOptTrainRecoverResult
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var recoverErr error
		if *generation {
			result, recoverErr = recoverSkillOptTrainGenerationLock(context.Background(), paths, store, *sessionID, *abort, *advanceState)
			return recoverErr
		}
		result, recoverErr = recoverSkillOptTrainOptimizerArtifacts(context.Background(), paths, store, *sessionID, *outRoot)
		return recoverErr
	}); err != nil {
		if result.SessionID != "" {
			if *jsonOutput {
				_ = writeJSON(stdout, result)
			} else {
				printSkillOptTrainRecoverResult(stdout, result)
			}
		}
		fmt.Fprintf(stderr, "skillopt train recover: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "skillopt train recover: %v\n", err)
			return 1
		}
		return 0
	}
	printSkillOptTrainRecoverResult(stdout, result)
	return 0
}

const skillOptTrainSkillOptWheelURL = "https://github.com/jerryfane/gitmoot-skillopt/releases/download/v0.3.1/gitmoot_skillopt-0.3.1-py3-none-any.whl"

type skillOptTrainOptimizerPreflightError struct {
	Executable string
	Step       string
	Result     subprocess.Result
	Err        error
}

func (e skillOptTrainOptimizerPreflightError) Error() string {
	executable := strings.TrimSpace(e.Executable)
	if executable == "" {
		executable = "gitmoot-skillopt"
	}
	step := strings.TrimSpace(e.Step)
	if step == "" {
		step = "preflight"
	}
	details := ""
	if e.Err != nil {
		details = ": " + e.Err.Error()
	}
	if diag := subprocessDiagnostics(e.Result); diag != "" {
		details += diag
	}
	return fmt.Sprintf(
		"gitmoot-skillopt is required for optimizer-backed train continue; %s failed for %q%s\n\n%s",
		step,
		executable,
		details,
		skillOptTrainSkillOptInstallHint(),
	)
}

func (e skillOptTrainOptimizerPreflightError) Unwrap() error {
	return e.Err
}

func skillOptTrainSkillOptInstallNextAction() string {
	return "install gitmoot-skillopt and rerun train continue"
}

func skillOptTrainSkillOptInstallHint() string {
	return "Install with pipx:\n" +
		"  python3 -m pip install --user pipx\n" +
		"  python3 -m pipx ensurepath\n" +
		"  pipx install " + skillOptTrainSkillOptWheelURL + "\n" +
		"  gitmoot-skillopt --version\n" +
		"  gitmoot-skillopt optimize --help\n\n" +
		"If pipx is unavailable, use a venv and pass --skillopt-bin:\n" +
		"  python3 -m venv ~/.gitmoot/skillopt-venv\n" +
		"  ~/.gitmoot/skillopt-venv/bin/python -m pip install --upgrade pip\n" +
		"  ~/.gitmoot/skillopt-venv/bin/python -m pip install " + skillOptTrainSkillOptWheelURL + "\n" +
		"  gitmoot skillopt train continue --session <id> --skillopt-bin ~/.gitmoot/skillopt-venv/bin/gitmoot-skillopt"
}

type skillOptTrainOptimizerResult struct {
	TrainingPackagePath   string
	OutRoot               string
	CandidatePackagePath  string
	ArtifactDir           string
	OptimizerRoot         string
	OptimizerAttempt      string
	OptimizerAttemptPath  string
	Command               string
	Args                  []string
	DryRun                bool
	Request               skillOptTrainOptimizerRequest
	BackendResolution     skillOptTrainBackendResolution
	RecoveryAvailable     bool
	OptimizerLockState    string
	CandidateVersionID    string
	NoCandidateReason     string
	NoCandidateNextAction string
	NextAction            string
	ExportedOnly          bool
}

type skillOptTrainRecoverResult struct {
	SessionID            string   `json:"session_id"`
	IterationID          string   `json:"iteration_id"`
	Classification       string   `json:"classification"`
	CurrentPhase         string   `json:"current_phase"`
	CandidateVersionID   string   `json:"candidate_version_id,omitempty"`
	NoCandidateReason    string   `json:"no_candidate_reason,omitempty"`
	NextAction           string   `json:"next_action,omitempty"`
	RecoveryAvailable    bool     `json:"recovery_available"`
	OutRoot              string   `json:"out_root,omitempty"`
	OptimizerRoot        string   `json:"optimizer_root,omitempty"`
	OptimizerAttempt     string   `json:"optimizer_attempt,omitempty"`
	OptimizerAttemptPath string   `json:"optimizer_attempt_path,omitempty"`
	CandidatePackagePath string   `json:"candidate_package,omitempty"`
	ArtifactDir          string   `json:"artifact_dir,omitempty"`
	Artifacts            []string `json:"artifacts,omitempty"`

	// Generation-recovery fields (populated by recover --generation). They
	// describe the stranded generation lock that was classified/reclaimed and
	// the per-item salvage outcome.
	Mode                  string   `json:"mode,omitempty"`
	LockState             string   `json:"lock_state,omitempty"`
	LockOwnerJobID        string   `json:"lock_owner_job_id,omitempty"`
	LockOwnerPID          int64    `json:"lock_owner_pid,omitempty"`
	LockOwnerHostname     string   `json:"lock_owner_hostname,omitempty"`
	LockReclaimed         bool     `json:"lock_reclaimed,omitempty"`
	ExpectedItems         int      `json:"expected_items,omitempty"`
	RecoveredItems        int      `json:"recovered_items,omitempty"`
	MissingItems          int      `json:"missing_items,omitempty"`
	PersistedOptions      int      `json:"persisted_options,omitempty"`
	MissingItemIDs        []string `json:"missing_item_ids,omitempty"`
	StateAdvanced         bool     `json:"state_advanced,omitempty"`
	GenerationLockBlocked bool     `json:"generation_lock_blocked,omitempty"`
}

type skillOptTrainBackendResolution struct {
	Backend               string
	OptimizerBackend      string
	TargetBackend         string
	InternalTargetAdapter string
	EvaluatorBackend      string
	ConfigStatus          string
}

type skillOptTrainOptimizerPaths struct {
	OutRoot              string
	OptimizerRoot        string
	OptimizerAttempt     string
	OptimizerAttemptPath string
	ArtifactRoot         string
	TrainingPackagePath  string
	CandidatePackagePath string
	ArtifactDir          string
}

func skillOptTrainContinueNeedsOptimizerPreflight(phase string, request skillOptTrainOptimizerRequest) bool {
	if request.ExportOnly {
		return false
	}
	switch strings.TrimSpace(phase) {
	case skillopt.TrainStateFeedbackSynced, skillopt.TrainStateTrainingPackageCreated:
		return true
	case skillopt.TrainStateOptimizerCompleted, skillopt.TrainStateOptimizerCompletedNoCandidate:
		return request.RerunOptimizer
	default:
		return false
	}
}

func preflightSkillOptTrainOptimizerForContinue(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest) (skillOptTrainOptimizerResult, error) {
	resolvedRequest, backendResolution, err := resolveSkillOptTrainBackendRequest(request)
	if err != nil {
		return skillOptTrainOptimizerResult{}, err
	}
	request = resolvedRequest
	optimizerPaths, err := resolveSkillOptTrainOptimizerPaths(paths, session, iteration, request)
	if err != nil {
		return skillOptTrainOptimizerResult{}, err
	}
	result := skillOptTrainOptimizerResult{
		TrainingPackagePath:  optimizerPaths.TrainingPackagePath,
		OutRoot:              optimizerPaths.OutRoot,
		CandidatePackagePath: optimizerPaths.CandidatePackagePath,
		ArtifactDir:          optimizerPaths.ArtifactDir,
		OptimizerRoot:        optimizerPaths.OptimizerRoot,
		OptimizerAttempt:     optimizerPaths.OptimizerAttempt,
		OptimizerAttemptPath: optimizerPaths.OptimizerAttemptPath,
		Command:              skillOptTrainOptimizerExecutable(request),
		DryRun:               request.DryRun,
		Request:              request,
		BackendResolution:    backendResolution,
		RecoveryAvailable:    skillOptTrainOptimizerRecoveryAvailable(optimizerPaths),
		OptimizerLockState:   skillOptTrainOptimizerLockState(request),
	}
	command, preflightResult, err := preflightSkillOptTrainOptimizerExecutable(ctx, request)
	if strings.TrimSpace(command) != "" {
		result.Command = command
	}
	if err != nil {
		result.NextAction = skillOptTrainSkillOptInstallNextAction()
		if metaErr := recordSkillOptTrainOptimizerFailure(ctx, store, session, iteration, request, optimizerPaths, result.Command, nil, preflightResult, err); metaErr != nil {
			return result, fmt.Errorf("%w; failed to record optimizer failure: %v", err, metaErr)
		}
		return result, err
	}
	return result, nil
}

func continueSkillOptTrainOptimizer(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest, progress io.Writer) (skillOptTrainOptimizerResult, error) {
	if strings.TrimSpace(iteration.EvalRunID) == "" {
		return skillOptTrainOptimizerResult{}, fmt.Errorf("train iteration %s has no eval run id", iteration.ID)
	}
	resolvedRequest, backendResolution, err := resolveSkillOptTrainBackendRequest(request)
	if err != nil {
		return skillOptTrainOptimizerResult{}, err
	}
	request = resolvedRequest
	optimizerPaths, err := resolveSkillOptTrainOptimizerPaths(paths, session, iteration, request)
	if err != nil {
		return skillOptTrainOptimizerResult{}, err
	}
	result := skillOptTrainOptimizerResult{
		TrainingPackagePath:  optimizerPaths.TrainingPackagePath,
		OutRoot:              optimizerPaths.OutRoot,
		CandidatePackagePath: optimizerPaths.CandidatePackagePath,
		ArtifactDir:          optimizerPaths.ArtifactDir,
		OptimizerRoot:        optimizerPaths.OptimizerRoot,
		OptimizerAttempt:     optimizerPaths.OptimizerAttempt,
		OptimizerAttemptPath: optimizerPaths.OptimizerAttemptPath,
		DryRun:               request.DryRun,
		Request:              request,
		BackendResolution:    backendResolution,
		RecoveryAvailable:    skillOptTrainOptimizerRecoveryAvailable(optimizerPaths),
		OptimizerLockState:   skillOptTrainOptimizerLockState(request),
	}
	state := skillopt.NormalizeTrainState(iteration.State)
	rerunFromCompletedOptimizer := request.RerunOptimizer && (state == skillopt.TrainStateOptimizerCompleted || state == skillopt.TrainStateOptimizerCompletedNoCandidate)
	if rerunFromCompletedOptimizer {
		state = skillopt.TrainStateTrainingPackageCreated
	}
	if state == skillopt.TrainStateFeedbackSynced {
		if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateTrainingPackageCreated); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		exportMetadata, err := exportSkillOptTrainPackage(ctx, store, iteration, optimizerPaths, request)
		if err != nil {
			return result, err
		}
		session.State = skillopt.TrainStateTrainingPackageCreated
		iteration.State = skillopt.TrainStateTrainingPackageCreated
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", exportMetadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", exportMetadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return result, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
			return result, err
		}
		state = skillopt.TrainStateTrainingPackageCreated
	}
	if request.ExportOnly && state == skillopt.TrainStateTrainingPackageCreated {
		// The training package now exists (exported above or already created)
		// and the optimizer has not run yet. Stop here so the caller can inspect
		// it before paying for a real optimizer run. For later states (the
		// optimizer already ran) export-only is a no-op and the normal
		// candidate-import path below proceeds.
		result.ExportedOnly = true
		return result, nil
	}
	if state == skillopt.TrainStateTrainingPackageCreated {
		if !rerunFromCompletedOptimizer {
			if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateOptimizerCompleted); err != nil {
				return skillOptTrainOptimizerResult{}, err
			}
		}
		command, args, err := buildSkillOptTrainOptimizerCommand(iteration, request, optimizerPaths)
		if err != nil {
			if rerunFromCompletedOptimizer {
				return result, err
			}
			if metaErr := recordSkillOptTrainOptimizerFailure(ctx, store, session, iteration, request, optimizerPaths, command, args, subprocess.Result{}, err); metaErr != nil {
				return result, fmt.Errorf("%w; failed to record optimizer failure: %v", err, metaErr)
			}
			return result, err
		}
		if request.RerunOptimizer {
			exportMetadata, err := exportSkillOptTrainPackage(ctx, store, iteration, optimizerPaths, request)
			if err != nil {
				return result, err
			}
			session.State = skillopt.TrainStateTrainingPackageCreated
			iteration.State = skillopt.TrainStateTrainingPackageCreated
			session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", exportMetadata)
			iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", exportMetadata)
			if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
				return result, err
			}
			if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
				return result, err
			}
		}
		if err := recordSkillOptTrainOptimizerStarted(ctx, store, &session, &iteration, request, optimizerPaths, command, args); err != nil {
			return result, err
		}
		result.Command = command
		result.Args = args
		// One serialized writer shared by the launch banner, the heartbeat
		// ticker, and the optimizer's streamed output: they write concurrently
		// and the destination is not necessarily concurrency-safe.
		sharedProgress := subprocess.SyncWriter(progress)
		announceSkillOptTrainOptimizerLaunch(sharedProgress, request)
		stopHeartbeat := startSkillOptTrainOptimizerHeartbeat(sharedProgress)
		runResult, err := runSkillOptTrainOptimizer(ctx, sharedProgress, optimizerPaths, request, command, args)
		stopHeartbeat()
		result.RecoveryAvailable = skillOptTrainOptimizerRecoveryAvailable(optimizerPaths)
		if err != nil {
			if metaErr := recordSkillOptTrainOptimizerFailure(ctx, store, session, iteration, request, optimizerPaths, command, args, runResult, err); metaErr != nil {
				return result, fmt.Errorf("%w; failed to record optimizer failure: %v", err, metaErr)
			}
			return result, err
		}
		metadata := skillOptTrainOptimizerMetadata(request, optimizerPaths, command, args, runResult, "succeeded", nil)
		session.State = skillopt.TrainStateOptimizerCompleted
		iteration.State = skillopt.TrainStateOptimizerCompleted
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", metadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return result, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
			return result, err
		}
		state = skillopt.TrainStateOptimizerCompleted
	}
	if state == skillopt.TrainStateOptimizerCompleted {
		if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateCandidateCreated); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		version, err := importSkillOptTrainCandidate(ctx, paths, store, session, iteration, optimizerPaths)
		if err != nil {
			if errors.Is(err, skillopt.ErrNoCandidate) {
				reason, nextAction := skillOptNoCandidateReasonAndNextAction(err, optimizerPaths.CandidatePackagePath)
				if metaErr := recordSkillOptTrainNoCandidate(ctx, store, session, iteration, optimizerPaths, reason); metaErr != nil {
					return skillOptTrainOptimizerResult{}, fmt.Errorf("%w; failed to record no-candidate result: %v", err, metaErr)
				}
				result.NoCandidateReason = reason
				result.NoCandidateNextAction = nextAction
				return result, nil
			}
			if metaErr := recordSkillOptTrainCandidateImportFailure(ctx, store, session, iteration, optimizerPaths, err); metaErr != nil {
				return skillOptTrainOptimizerResult{}, fmt.Errorf("%w; failed to record candidate import failure: %v", err, metaErr)
			}
			return skillOptTrainOptimizerResult{}, err
		}
		result.CandidateVersionID = version.ID
		metadata := map[string]any{
			"status":                 "succeeded",
			"candidate_version":      version.ID,
			"candidate_package":      optimizerPaths.CandidatePackagePath,
			"artifact_dir":           optimizerPaths.ArtifactDir,
			"optimizer_root":         optimizerPaths.OptimizerRoot,
			"optimizer_attempt":      optimizerPaths.OptimizerAttempt,
			"optimizer_attempt_path": optimizerPaths.OptimizerAttemptPath,
			"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
			"source":                 "gitmoot skillopt train continue",
		}
		session.State = skillopt.TrainStateCandidateCreated
		iteration.State = skillopt.TrainStateCandidateCreated
		iteration.CandidateVersionID = version.ID
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		return result, nil
	}
	return skillOptTrainOptimizerResult{}, fmt.Errorf("train iteration %s is at %s; expected %s, %s, or %s", iteration.ID, iteration.State, skillopt.TrainStateFeedbackSynced, skillopt.TrainStateTrainingPackageCreated, skillopt.TrainStateOptimizerCompleted)
}

func skillOptTrainOptimizerLockState(request skillOptTrainOptimizerRequest) string {
	state := strings.TrimSpace(request.OptimizerLockState)
	if state == "" {
		return "acquired"
	}
	return state
}

func applySkillOptTrainOptimizerDefaultsFromMetadata(metadataJSON string, request *skillOptTrainOptimizerRequest) {
	if request == nil {
		return
	}
	metadata := decodedSkillOptMetadataValue(decodedSkillOptMetadata(metadataJSON)["optimizer_defaults"])
	if len(metadata) == 0 {
		return
	}
	// Captured before backend inheritance below: stored model names are
	// backend-specific, so they are only inherited when the backends are too.
	backendOverridden := request.Backend != "" || request.OptimizerBackend != "" ||
		request.TargetBackend != "" || request.EvaluatorBackend != ""
	if request.Backend == "" && request.OptimizerBackend == "" && request.TargetBackend == "" && request.EvaluatorBackend == "" {
		request.Backend = metadataString(metadata, "backend")
	}
	if request.Backend == "" {
		if request.OptimizerBackend == "" {
			request.OptimizerBackend = metadataString(metadata, "optimizer_backend")
		}
		if request.TargetBackend == "" {
			request.TargetBackend = metadataString(metadata, "target_backend")
		}
		if request.EvaluatorBackend == "" {
			request.EvaluatorBackend = metadataString(metadata, "evaluator_backend")
		}
	}
	if !backendOverridden && request.Model == "" {
		if request.OptimizerModel == "" {
			request.OptimizerModel = metadataString(metadata, "optimizer_model")
		}
		if request.TargetModel == "" {
			request.TargetModel = metadataString(metadata, "target_model")
		}
	}
	if request.EvaluatorID == "" {
		request.EvaluatorID = metadataString(metadata, "evaluator_id")
	}
	if request.SkillUpdateMode == "" {
		request.SkillUpdateMode = metadataString(metadata, "skill_update_mode")
	}
	if !request.OptimizerViewsSet {
		if value, ok := metadataInt(metadata, "optimizer_views"); ok {
			request.OptimizerViews = value
			request.OptimizerViewsSet = true
		}
	}
	if !request.RetryOptimizerViewsSet {
		if value := metadataString(metadata, "retry_optimizer_views"); value != "" {
			request.RetryOptimizerViews = value
			request.RetryOptimizerViewsSet = true
		}
	}
	if !request.NoopRetryBudgetSet {
		if value, ok := metadataInt(metadata, "noop_retry_budget"); ok {
			request.NoopRetryBudget = value
			request.NoopRetryBudgetSet = true
		}
	}
	if !request.GateRejectRetryBudgetSet {
		if value, ok := metadataInt(metadata, "gate_reject_retry_budget"); ok {
			request.GateRejectRetryBudget = value
			request.GateRejectRetryBudgetSet = true
		}
	}
	if !request.WrongArtifactRetryBudgetSet {
		if value, ok := metadataInt(metadata, "wrong_artifact_retry_budget"); ok {
			request.WrongArtifactRetryBudget = value
			request.WrongArtifactRetryBudgetSet = true
		}
	}
	if !request.TargetArtifactRetryBudgetSet {
		if value, ok := metadataInt(metadata, "target_artifact_retry_budget"); ok {
			request.TargetArtifactRetryBudget = value
			request.TargetArtifactRetryBudgetSet = true
		}
	}
	if !request.HardFailureRetryBudgetSet {
		if value, ok := metadataInt(metadata, "hard_failure_retry_budget"); ok {
			request.HardFailureRetryBudget = value
			request.HardFailureRetryBudgetSet = true
		}
	}
	if !request.FinalEvalSet {
		request.FinalEval = metadataBool(metadata, "final_eval")
	}
}

func validateSkillOptTrainOptimizerRequestAfterDefaults(request *skillOptTrainOptimizerRequest) error {
	if request == nil {
		return nil
	}
	if request.OptimizerViewsSet && request.OptimizerViews <= 0 {
		return errors.New("--optimizer-views must be greater than zero")
	}
	if request.RetryOptimizerViewsSet {
		normalized, err := normalizeSkillOptRetryOptimizerViews(request.RetryOptimizerViews)
		if err != nil {
			return err
		}
		request.RetryOptimizerViews = normalized
		if request.OptimizerViewsSet {
			if retryViews, ok := parseSkillOptRetryOptimizerViewsNumber(normalized); ok && retryViews > request.OptimizerViews {
				return errors.New("--retry-optimizer-views cannot exceed --optimizer-views")
			}
		}
	}
	return nil
}

func resolveSkillOptTrainBackendRequest(request skillOptTrainOptimizerRequest) (skillOptTrainOptimizerRequest, skillOptTrainBackendResolution, error) {
	preset := strings.TrimSpace(strings.ToLower(request.Backend))
	switch preset {
	case "":
		optimizerBackend := strings.TrimSpace(request.OptimizerBackend)
		if optimizerBackend == "" {
			optimizerBackend = "openai_chat"
		}
		internalTargetAdapter := strings.TrimSpace(request.TargetBackend)
		if internalTargetAdapter == "" {
			internalTargetAdapter = "openai_chat"
		}
		evaluatorBackend := strings.TrimSpace(request.EvaluatorBackend)
		if evaluatorBackend == "" {
			evaluatorBackend = optimizerBackend
		}
		resolution := skillOptTrainBackendResolution{
			Backend:               "custom",
			OptimizerBackend:      optimizerBackend,
			TargetBackend:         skillOptTrainDisplayTargetBackend(internalTargetAdapter),
			InternalTargetAdapter: internalTargetAdapter,
			EvaluatorBackend:      evaluatorBackend,
		}
		resolution.ConfigStatus = skillOptTrainBackendConfigStatus(resolution)
		return request, resolution, nil
	case "codex":
		if err := skillOptTrainBackendPresetConflict("--optimizer-backend", request.OptimizerBackend, "codex"); err != nil {
			return skillOptTrainOptimizerRequest{}, skillOptTrainBackendResolution{}, err
		}
		if err := skillOptTrainCodexTargetBackendConflict(request.TargetBackend); err != nil {
			return skillOptTrainOptimizerRequest{}, skillOptTrainBackendResolution{}, err
		}
		if err := skillOptTrainBackendPresetConflict("--evaluator-backend", request.EvaluatorBackend, "codex"); err != nil {
			return skillOptTrainOptimizerRequest{}, skillOptTrainBackendResolution{}, err
		}
		request.OptimizerBackend = "codex"
		request.TargetBackend = "codex_exec"
		request.EvaluatorBackend = "codex"
		resolution := skillOptTrainBackendResolution{
			Backend:               "codex",
			OptimizerBackend:      "codex",
			TargetBackend:         "codex",
			InternalTargetAdapter: "codex_exec",
			EvaluatorBackend:      "codex",
			ConfigStatus:          "codex_no_azure_or_openai_required",
		}
		return request, resolution, nil
	default:
		return skillOptTrainOptimizerRequest{}, skillOptTrainBackendResolution{}, fmt.Errorf("backend preset %q is not supported; use codex or explicit backend flags", preset)
	}
}

func skillOptTrainBackendPresetConflict(flagName string, value string, expected string) error {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == expected {
		return nil
	}
	return fmt.Errorf("--backend codex conflicts with %s %q; omit %s or set it to %s", flagName, value, flagName, expected)
}

func skillOptTrainCodexTargetBackendConflict(value string) error {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "codex" || value == "codex_exec" {
		return nil
	}
	return fmt.Errorf("--backend codex conflicts with --target-backend %q; omit --target-backend or use codex/codex_exec", value)
}

func skillOptTrainDisplayTargetBackend(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "codex_exec") {
		return "codex"
	}
	return value
}

func skillOptTrainBackendConfigStatus(resolution skillOptTrainBackendResolution) string {
	backends := []string{
		resolution.OptimizerBackend,
		resolution.InternalTargetAdapter,
		resolution.EvaluatorBackend,
	}
	anyBackend := false
	externalCredentials := false
	for _, backend := range backends {
		backend = strings.TrimSpace(strings.ToLower(backend))
		if backend == "" {
			continue
		}
		anyBackend = true
		if backend == "openai_chat" || backend == "azure_openai" || strings.Contains(backend, "openai") || strings.Contains(backend, "azure") {
			externalCredentials = true
		}
	}
	if !anyBackend {
		return "using_optimizer_defaults"
	}
	if externalCredentials {
		return "external_credentials_may_be_required"
	}
	return "no_azure_or_openai_required"
}

func resolveSkillOptTrainOptimizerPaths(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest) (skillOptTrainOptimizerPaths, error) {
	outRoot := strings.TrimSpace(request.OutRoot)
	optimizerMetadata := decodedSkillOptMetadataValue(decodedSkillOptMetadata(iteration.MetadataJSON)["optimizer"])
	persistedOptimizerRoot := metadataString(optimizerMetadata, "optimizer_root")
	persistedAttempt := metadataString(optimizerMetadata, "optimizer_attempt")
	persistedAttemptPath := metadataString(optimizerMetadata, "optimizer_attempt_path")
	persistedOutRoot := metadataString(optimizerMetadata, "out_root")
	persistedTrainingPackage := metadataString(optimizerMetadata, "training_package")
	persistedCandidateOutput := metadataString(optimizerMetadata, "candidate_output")
	persistedArtifactDir := metadataString(optimizerMetadata, "artifact_dir")
	persistedBaseRoot := persistedOptimizerRoot
	if persistedBaseRoot == "" {
		persistedBaseRoot = inferSkillOptTrainOptimizerRoot(persistedOutRoot, persistedAttempt)
	}
	if persistedTrainingPackage != "" && skillopt.NormalizeTrainState(iteration.State) != skillopt.TrainStateFeedbackSynced {
		if outRoot != "" {
			absRequestedOutRoot, err := filepath.Abs(outRoot)
			if err != nil {
				return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve optimizer out-root: %w", err)
			}
			absPersistedRoot := persistedBaseRoot
			if absPersistedRoot == "" {
				absPersistedRoot = persistedOutRoot
			}
			if absPersistedRoot == "" {
				absPersistedRoot = filepath.Dir(persistedTrainingPackage)
			}
			if absPersistedRoot, err = filepath.Abs(absPersistedRoot); err != nil {
				return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve persisted optimizer out-root: %w", err)
			}
			absPersistedOutRoot := ""
			if persistedOutRoot != "" {
				if absPersistedOutRoot, err = filepath.Abs(persistedOutRoot); err != nil {
					return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve persisted optimizer attempt out-root: %w", err)
				}
			}
			if absRequestedOutRoot != absPersistedRoot && absRequestedOutRoot != absPersistedOutRoot {
				return skillOptTrainOptimizerPaths{}, fmt.Errorf("optimizer package already exported at %s; retry with the same --out-root or omit --out-root", persistedTrainingPackage)
			}
		}
		outRoot = persistedBaseRoot
		if outRoot == "" {
			outRoot = persistedOutRoot
		}
		if outRoot == "" {
			outRoot = filepath.Dir(filepath.Dir(filepath.Dir(persistedTrainingPackage)))
		}
	}
	if outRoot == "" {
		outRoot = persistedBaseRoot
	}
	if outRoot == "" {
		outRoot = inferSkillOptTrainOptimizerRoot(persistedOutRoot, persistedAttempt)
	}
	if outRoot == "" {
		outRoot = filepath.Join(paths.Evals, "train", session.ID, iteration.ID, "optimizer")
	}
	absOptimizerRoot, err := filepath.Abs(outRoot)
	if err != nil {
		return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve optimizer out-root: %w", err)
	}
	state := skillopt.NormalizeTrainState(iteration.State)
	attempt := persistedAttempt
	attemptPath := persistedAttemptPath
	if state == skillopt.TrainStateFeedbackSynced || persistedTrainingPackage == "" {
		attempt = "attempt-001"
		attemptPath = filepath.Join(absOptimizerRoot, "attempts", attempt)
	} else if request.RerunOptimizer {
		nextAttempt, err := nextSkillOptTrainOptimizerAttempt(absOptimizerRoot, attempt)
		if err != nil {
			return skillOptTrainOptimizerPaths{}, err
		}
		attempt = nextAttempt
		attemptPath = filepath.Join(absOptimizerRoot, "attempts", attempt)
	} else if attemptPath == "" {
		attemptPath = persistedOutRoot
	}
	if attemptPath == "" {
		attemptPath = filepath.Join(absOptimizerRoot, "attempts", firstNonEmpty(attempt, "attempt-001"))
	}
	absAttemptPath, err := filepath.Abs(attemptPath)
	if err != nil {
		return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve optimizer attempt path: %w", err)
	}
	if attempt == "" {
		attempt = filepath.Base(absAttemptPath)
	}
	trainingPackagePath := filepath.Join(absAttemptPath, "training.json")
	candidatePackagePath := filepath.Join(absAttemptPath, "candidate.json")
	artifactDir := filepath.Join(absAttemptPath, "artifacts")
	if persistedTrainingPackage != "" && state != skillopt.TrainStateFeedbackSynced {
		if !request.RerunOptimizer {
			trainingPackagePath = persistedTrainingPackage
			if persistedCandidateOutput != "" {
				candidatePackagePath = persistedCandidateOutput
			}
			if persistedArtifactDir != "" {
				artifactDir = persistedArtifactDir
			}
		}
	}
	return skillOptTrainOptimizerPaths{
		OutRoot:              absAttemptPath,
		OptimizerRoot:        absOptimizerRoot,
		OptimizerAttempt:     attempt,
		OptimizerAttemptPath: absAttemptPath,
		ArtifactRoot:         paths.ArtifactBlobs,
		TrainingPackagePath:  trainingPackagePath,
		CandidatePackagePath: candidatePackagePath,
		ArtifactDir:          artifactDir,
	}, nil
}

func inferSkillOptTrainOptimizerRoot(outRoot string, attempt string) string {
	outRoot = strings.TrimSpace(outRoot)
	attempt = strings.TrimSpace(attempt)
	if outRoot == "" || attempt == "" {
		return outRoot
	}
	clean := filepath.Clean(outRoot)
	if filepath.Base(clean) != attempt {
		return outRoot
	}
	attemptsDir := filepath.Dir(clean)
	if filepath.Base(attemptsDir) != "attempts" {
		return outRoot
	}
	return filepath.Dir(attemptsDir)
}

func nextSkillOptTrainOptimizerAttempt(root string, currentAttempt string) (string, error) {
	maxAttempt := skillOptTrainOptimizerAttemptNumber(currentAttempt)
	entries, err := os.ReadDir(filepath.Join(root, "attempts"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read optimizer attempts: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if number := skillOptTrainOptimizerAttemptNumber(entry.Name()); number > maxAttempt {
			maxAttempt = number
		}
	}
	return fmt.Sprintf("attempt-%03d", maxAttempt+1), nil
}

func skillOptTrainOptimizerAttemptNumber(attempt string) int {
	attempt = strings.TrimSpace(attempt)
	if !strings.HasPrefix(attempt, "attempt-") {
		return 0
	}
	number, err := strconv.Atoi(strings.TrimPrefix(attempt, "attempt-"))
	if err != nil || number < 0 {
		return 0
	}
	return number
}

func exportSkillOptTrainPackage(ctx context.Context, store *db.Store, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths, request skillOptTrainOptimizerRequest) (map[string]any, error) {
	pkg, err := skillopt.ExportTrainingPackage(ctx, store, iteration.EvalRunID)
	if err != nil {
		return nil, fmt.Errorf("export training package: %w", err)
	}
	if profile := skillopt.BuildEvaluatorProfile(request.EvaluatorID, request.EvaluatorModel, pkg.EvaluatorConfig); profile != nil {
		pkg.EvaluatorProfile = profile
	}
	encoded, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode training package: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := writeSkillOptFile(paths.TrainingPackagePath, encoded); err != nil {
		return nil, fmt.Errorf("write training package: %w", err)
	}
	return map[string]any{
		"status":                 "package_created",
		"training_package":       paths.TrainingPackagePath,
		"out_root":               paths.OutRoot,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"artifact_root":          paths.ArtifactRoot,
		"candidate_output":       paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"created_at":             time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train continue",
	}, nil
}

func skillOptTrainOptimizerRecoveryAvailable(paths skillOptTrainOptimizerPaths) bool {
	for _, path := range []string{
		paths.CandidatePackagePath,
		filepath.Join(paths.OutRoot, "summary.json"),
		filepath.Join(paths.OutRoot, "runtime_state.json"),
		filepath.Join(paths.OutRoot, "history.json"),
		filepath.Join(paths.OutRoot, "best_skill.md"),
	} {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func recoverSkillOptTrainOptimizerArtifacts(ctx context.Context, paths config.Paths, store *db.Store, sessionID string, outRoot string) (skillOptTrainRecoverResult, error) {
	session, iteration, _, err := loadSkillOptTrainStatus(ctx, store, sessionID)
	if err != nil {
		return skillOptTrainRecoverResult{}, err
	}
	if iteration == nil {
		return skillOptTrainRecoverResult{SessionID: strings.TrimSpace(session.ID), Classification: "unrecoverable"}, errors.New("train session has no iteration to recover")
	}
	optimizerPaths, err := resolveSkillOptTrainOptimizerPaths(paths, session, *iteration, skillOptTrainOptimizerRequest{OutRoot: outRoot})
	if err != nil {
		return skillOptTrainRecoverResult{}, err
	}
	result := skillOptTrainRecoverResult{
		SessionID:            strings.TrimSpace(session.ID),
		IterationID:          strings.TrimSpace(iteration.ID),
		CurrentPhase:         skillopt.NormalizeTrainState(iteration.State),
		RecoveryAvailable:    skillOptTrainOptimizerRecoveryAvailable(optimizerPaths),
		OutRoot:              optimizerPaths.OutRoot,
		OptimizerRoot:        optimizerPaths.OptimizerRoot,
		OptimizerAttempt:     optimizerPaths.OptimizerAttempt,
		OptimizerAttemptPath: optimizerPaths.OptimizerAttemptPath,
		CandidatePackagePath: optimizerPaths.CandidatePackagePath,
		ArtifactDir:          optimizerPaths.ArtifactDir,
		Artifacts:            existingSkillOptTrainOptimizerArtifacts(optimizerPaths),
	}
	switch skillopt.NormalizeTrainState(iteration.State) {
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished, skillopt.TrainStateCandidatePromoted, skillopt.TrainStateCandidateRejected:
		result.Classification = "already_completed_candidate"
		result.CandidateVersionID = strings.TrimSpace(iteration.CandidateVersionID)
		result.NextAction = "candidate already exists; continue with candidate review or decision"
		return result, nil
	case skillopt.TrainStateOptimizerCompletedNoCandidate:
		result.Classification = "already_completed_no_candidate"
		result.NoCandidateReason = skillOptMetadataString(iteration.MetadataJSON, "candidate_import", "no_candidate_reason")
		result.NextAction = skillOptNoCandidateNextAction()
		return result, nil
	}
	releaseOptimizerLock, _, err := acquireSkillOptTrainOptimizerLock(ctx, store, session.ID, iteration.ID, skillOptTrainOptimizerLockTTL, skillOptTrainOptimizerRequest{OutRoot: optimizerPaths.OutRoot})
	if err != nil {
		result.Classification = "optimizer_active"
		result.NextAction = "wait for the active optimizer run to finish before recovering artifacts"
		return result, err
	}
	defer func() {
		_ = releaseOptimizerLock(context.Background())
	}()
	session, iteration, _, err = loadSkillOptTrainStatus(ctx, store, sessionID)
	if err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	if iteration == nil {
		result.CurrentPhase = ""
		result.Classification = "unrecoverable"
		return result, errors.New("train session has no iteration to recover")
	}
	optimizerPaths, err = resolveSkillOptTrainOptimizerPaths(paths, session, *iteration, skillOptTrainOptimizerRequest{OutRoot: outRoot})
	if err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	result.CurrentPhase = skillopt.NormalizeTrainState(iteration.State)
	result.RecoveryAvailable = skillOptTrainOptimizerRecoveryAvailable(optimizerPaths)
	result.OutRoot = optimizerPaths.OutRoot
	result.OptimizerRoot = optimizerPaths.OptimizerRoot
	result.OptimizerAttempt = optimizerPaths.OptimizerAttempt
	result.OptimizerAttemptPath = optimizerPaths.OptimizerAttemptPath
	result.CandidatePackagePath = optimizerPaths.CandidatePackagePath
	result.ArtifactDir = optimizerPaths.ArtifactDir
	result.Artifacts = existingSkillOptTrainOptimizerArtifacts(optimizerPaths)
	switch skillopt.NormalizeTrainState(iteration.State) {
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished, skillopt.TrainStateCandidatePromoted, skillopt.TrainStateCandidateRejected:
		result.Classification = "already_completed_candidate"
		result.CandidateVersionID = strings.TrimSpace(iteration.CandidateVersionID)
		result.NextAction = "candidate already exists; continue with candidate review or decision"
		return result, nil
	case skillopt.TrainStateOptimizerCompletedNoCandidate:
		result.Classification = "already_completed_no_candidate"
		result.NoCandidateReason = skillOptMetadataString(iteration.MetadataJSON, "candidate_import", "no_candidate_reason")
		result.NextAction = skillOptNoCandidateNextAction()
		return result, nil
	}
	candidateContent, err := os.ReadFile(optimizerPaths.CandidatePackagePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			result.Classification = "corrupted_unrecoverable"
			return result, fmt.Errorf("read optimizer candidate package: %w", err)
		}
		if reason := noCandidateReasonFromSkillOptTrainOptimizerArtifacts(optimizerPaths); reason != "" {
			session, iteration, err = markSkillOptTrainOptimizerRecoveredComplete(ctx, store, session, *iteration, optimizerPaths)
			if err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			if err := recordSkillOptTrainNoCandidate(ctx, store, session, *iteration, optimizerPaths, reason); err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			result.Classification = "completed_no_candidate"
			result.CurrentPhase = skillopt.TrainStateOptimizerCompletedNoCandidate
			result.NoCandidateReason = reason
			result.NextAction = skillOptNoCandidateNextAction()
			return result, nil
		}
		if result.RecoveryAvailable {
			result.Classification = "incomplete_resumable"
			result.NextAction = "candidate package is missing; rerun the optimizer with --rerun-optimizer or inspect the artifact directory"
			return result, errors.New("optimizer artifacts are present but candidate.json is missing")
		}
		result.Classification = "unavailable"
		result.NextAction = "run the optimizer before attempting recovery"
		return result, errors.New("no recoverable optimizer artifacts found")
	}
	var candidate skillopt.CandidatePackage
	if err := json.Unmarshal(candidateContent, &candidate); err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, fmt.Errorf("decode optimizer candidate package: %w", err)
	}
	if err := validateSkillOptTrainCandidatePackage(ctx, store, session, *iteration, candidate); err != nil {
		if errors.Is(err, skillopt.ErrNoCandidate) {
			reason, nextAction := skillOptNoCandidateReasonAndNextAction(err, optimizerPaths.CandidatePackagePath)
			session, iteration, err = markSkillOptTrainOptimizerRecoveredComplete(ctx, store, session, *iteration, optimizerPaths)
			if err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			if err := recordSkillOptTrainNoCandidate(ctx, store, session, *iteration, optimizerPaths, reason); err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			result.Classification = "completed_no_candidate"
			result.CurrentPhase = skillopt.TrainStateOptimizerCompletedNoCandidate
			result.NoCandidateReason = reason
			result.NextAction = nextAction
			return result, nil
		}
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	if err := validateSkillOptTrainOptimizerRecoverableCompleteState(*iteration); err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	version, err := importSkillOptTrainCandidate(ctx, paths, store, session, *iteration, optimizerPaths)
	if err != nil {
		if errors.Is(err, skillopt.ErrNoCandidate) {
			reason, nextAction := skillOptNoCandidateReasonAndNextAction(err, optimizerPaths.CandidatePackagePath)
			session, iteration, err = markSkillOptTrainOptimizerRecoveredComplete(ctx, store, session, *iteration, optimizerPaths)
			if err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			if err := recordSkillOptTrainNoCandidate(ctx, store, session, *iteration, optimizerPaths, reason); err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			result.Classification = "completed_no_candidate"
			result.CurrentPhase = skillopt.TrainStateOptimizerCompletedNoCandidate
			result.NoCandidateReason = reason
			result.NextAction = nextAction
			return result, nil
		}
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	session, iteration, err = markSkillOptTrainOptimizerRecoveredComplete(ctx, store, session, *iteration, optimizerPaths)
	if err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	metadata := map[string]any{
		"status":                 "recovered",
		"candidate_version":      version.ID,
		"candidate_package":      optimizerPaths.CandidatePackagePath,
		"artifact_dir":           optimizerPaths.ArtifactDir,
		"optimizer_root":         optimizerPaths.OptimizerRoot,
		"optimizer_attempt":      optimizerPaths.OptimizerAttempt,
		"optimizer_attempt_path": optimizerPaths.OptimizerAttemptPath,
		"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train recover",
	}
	session.State = skillopt.TrainStateCandidateCreated
	iteration.State = skillopt.TrainStateCandidateCreated
	iteration.CandidateVersionID = version.ID
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, *iteration); err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	result.Classification = "completed_candidate"
	result.CurrentPhase = skillopt.TrainStateCandidateCreated
	result.CandidateVersionID = version.ID
	result.NextAction = "publish candidate diff and preview review with train continue"
	return result, nil
}

func validateSkillOptTrainOptimizerRecoverableCompleteState(iteration db.SkillOptTrainIteration) error {
	state := skillopt.NormalizeTrainState(iteration.State)
	switch state {
	case skillopt.TrainStateOptimizerCompleted, skillopt.TrainStateTrainingPackageCreated:
		return nil
	default:
		return fmt.Errorf("cannot recover optimizer artifacts while iteration is in state %s", state)
	}
}

// recoverSkillOptTrainGenerationLock reclaims a generation lock stranded by a
// crashed/killed train continue (the lock's deferred release never ran) and
// salvages the already-persisted per-item options, optionally advancing the
// iteration to options_generated.
//
// It is a liveness-gated steal-then-own: the stranded lock is reclaimed ONLY
// when its owner PID is provably dead AND the lock was held on this same host
// (owner_hostname == this host, OR owner_hostname is empty — a pre-#303 legacy
// strand from a binary that did not record the host, treated as local since
// skillopt train is local-first). A live owner is never stolen — recovery
// refuses with the busy error so the operator stops the running process first. A
// cross-host owner (a different, recorded host) cannot be liveness-checked
// locally, so recovery requires the lock's TTL to have expired before reclaiming
// it. The recover process then re-acquires the lock for itself so the recovery
// is also crash-safe.
//
// Salvage is import-only: completed items are already durable (per-item commits
// from #311), so recovery just classifies what is persisted vs. missing.
// Regenerating missing items is deferred to a future --regenerate. The iteration
// advances to options_generated only when advanceState is set and every expected
// item is recovered. With abort set, the lock is reclaimed and the phase is left
// at items_ready (persisted items are kept).
func recoverSkillOptTrainGenerationLock(ctx context.Context, paths config.Paths, store *db.Store, sessionID string, abort bool, advanceState bool) (skillOptTrainRecoverResult, error) {
	session, iteration, _, err := loadSkillOptTrainStatus(ctx, store, sessionID)
	if err != nil {
		return skillOptTrainRecoverResult{}, err
	}
	result := skillOptTrainRecoverResult{
		SessionID: strings.TrimSpace(session.ID),
		Mode:      "generation",
	}
	if iteration == nil {
		result.Classification = "unrecoverable"
		return result, errors.New("train session has no iteration to recover")
	}
	result.IterationID = strings.TrimSpace(iteration.ID)
	result.CurrentPhase = skillopt.NormalizeTrainState(iteration.State)

	// Classify the stranded generation lock (if any) and decide whether it is
	// safe to reclaim. A live owner is refused; a cross-host owner needs TTL
	// expiry; a same-host dead owner is reclaimable.
	lockKey := skillOptTrainGenerationLockKey(session.ID, iteration.ID)
	now := time.Now().UTC()
	thisHost, _ := os.Hostname()
	lock, lockErr := store.GetResourceLock(ctx, lockKey)
	hasLock := false
	switch {
	case lockErr == nil:
		hasLock = true
	case errors.Is(lockErr, sql.ErrNoRows):
		hasLock = false
	default:
		return result, lockErr
	}
	if hasLock {
		result.LockState = skillOptTrainOptimizerLockStatus(lock, now)
		result.LockOwnerJobID = strings.TrimSpace(lock.OwnerJobID)
		result.LockOwnerPID = lock.OwnerPID
		result.LockOwnerHostname = strings.TrimSpace(lock.OwnerHostname)
		host := strings.TrimSpace(lock.OwnerHostname)
		// Treat an empty/unrecorded owner_hostname as same-host-eligible: skillopt
		// train is a local-first workflow (one local SQLite home), and an empty
		// owner_hostname is the pre-#303 legacy case — the lock was written by a
		// binary that didn't record the host. Those legacy strands are exactly the
		// ones #303 exists to clear, so we treat an unknown host as this host. The
		// PID-dead gate still applies: a LIVE owner is always refused (never steal a
		// live owner); only a DEAD PID with an empty/this-host hostname is reclaimable.
		sameHost := host == "" || strings.EqualFold(host, strings.TrimSpace(thisHost))
		// Boot-aware liveness (#651): on the SAME host, an owner boot id that differs
		// from the current boot proves the host rebooted since the lock was taken, so
		// the owning process is dead — and its pid may since have been reused, so the
		// bare kill(2) probe (skillOptOwnerPIDLive) could wrongly report a reused pid
		// as the still-live owner. Short-circuit to "dead" BEFORE the syscall in that
		// case. The boot check is scoped to same-host: a different host has an
		// unrelated boot id, so cross-host still relies on the TTL rule below.
		ownerBoot, bootErr := store.ResourceLockOwnerBootID(ctx, lockKey)
		if bootErr != nil {
			return result, bootErr
		}
		currentBoot := db.BootID()
		bootProvesDead := sameHost && ownerBoot != "" && currentBoot != "" && ownerBoot != currentBoot
		ownerLive := !bootProvesDead && skillOptOwnerPIDLive(lock.OwnerPID)
		expired := !skillOptResourceLockActive(lock, now)
		switch {
		case ownerLive:
			// Never steal a live owner — its deferred release will run. When the
			// host is unrecorded (legacy lock) the owner is, by the local-first
			// invariant above, on this host, so say so rather than implying a
			// foreign host we cannot verify.
			result.Classification = "generation_active"
			result.GenerationLockBlocked = true
			result.NextAction = "stop the running generation process before recovering"
			return result, fmt.Errorf("%w: %s (owner pid %d still running)", errSkillOptTrainGenerationBusy, lockKey, lock.OwnerPID)
		case sameHost:
			// Same-host (or unrecorded/legacy host) dead owner: provably crashed,
			// safe to reclaim.
		case expired:
			// Genuinely cross-host owner we cannot liveness-check: only reclaim
			// once the lease has actually expired.
		default:
			// Genuinely cross-host (non-empty, different host) owner with an
			// unexpired lease: cannot verify liveness, so refuse until TTL expiry.
			result.Classification = "generation_active"
			result.GenerationLockBlocked = true
			result.NextAction = "owner is on another host; wait for the generation lock TTL to expire before recovering"
			return result, fmt.Errorf("%w: %s (owner on host %q; cannot verify liveness, lease not expired)", errSkillOptTrainGenerationBusy, lockKey, host)
		}
		// Reclaim by the stored owner job id (the deterministic identity of the
		// crashed holder) and emit an audit event — there is no
		// ForceReleaseLockWithEvent for resource locks, so the event is emitted
		// manually.
		released, err := store.DeleteResourceLocksByOwner(ctx, strings.TrimSpace(lock.OwnerJobID))
		if err != nil {
			return result, err
		}
		if released > 0 {
			result.LockReclaimed = true
			auditMessage := fmt.Sprintf("reclaimed stranded skillopt generation lock %s (owner pid %d, host %q, state %s) during recover --generation", lockKey, lock.OwnerPID, strings.TrimSpace(lock.OwnerHostname), result.LockState)
			if eventErr := store.AddJobEvent(ctx, db.JobEvent{
				JobID:   strings.TrimSpace(lock.OwnerJobID),
				Kind:    "lock_reclaimed",
				Message: auditMessage,
			}); eventErr != nil {
				return result, eventErr
			}
		}
	}

	if abort {
		// --abort: reclaim only, keep persisted items, leave phase at items_ready.
		if result.LockReclaimed {
			result.Classification = "generation_lock_reclaimed"
		} else {
			result.Classification = "generation_no_lock"
		}
		result.CurrentPhase = skillopt.NormalizeTrainState(iteration.State)
		result.NextAction = "rerun train continue to regenerate the remaining items"
		return result, nil
	}

	// Steal-then-own: re-acquire the generation lock for THIS recover process so
	// the salvage itself is crash-safe (a crash here strands our own lock, which a
	// subsequent recover reclaims the same way).
	releaseGenerationLock, _, acquired, err := acquireSkillOptTrainGenerationLock(ctx, store, session.ID, iteration.ID, skillOptTrainGenerationLockTTL)
	if err != nil {
		return result, err
	}
	if !acquired {
		result.Classification = "generation_active"
		result.GenerationLockBlocked = true
		result.NextAction = "another process re-acquired the generation lock; retry recovery"
		return result, fmt.Errorf("%w: %s", errSkillOptTrainGenerationBusy, lockKey)
	}
	defer func() {
		_ = releaseGenerationLock(context.Background())
	}()

	// Reload after taking ownership so we classify against the freshest state.
	session, reloaded, _, err := loadSkillOptTrainStatus(ctx, store, session.ID)
	if err != nil {
		return result, err
	}
	if reloaded == nil {
		result.Classification = "unrecoverable"
		return result, errors.New("train session has no iteration to recover")
	}
	iteration = reloaded
	result.CurrentPhase = skillopt.NormalizeTrainState(iteration.State)

	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			result.Classification = "unrecoverable"
			return result, fmt.Errorf("eval run %s not found", iteration.EvalRunID)
		}
		return result, err
	}
	rankedRun := skillOptRunUsesRankedOptions(run)
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return result, err
	}
	if len(items) == 0 {
		result.Classification = "unrecoverable"
		return result, fmt.Errorf("eval run %s has no review items to recover", run.ID)
	}
	roles := skillOptTrainGenerationRoles(run)
	if len(roles) < 2 {
		result.Classification = "unrecoverable"
		return result, fmt.Errorf("eval run %s expects at least 2 options", run.ID)
	}
	plan, err := classifySkillOptTrainGenerationItems(ctx, store, run, items, roles, rankedRun)
	if err != nil {
		// A partially persisted single item is corruption, not a normal resume
		// state — surface it without advancing.
		result.Classification = "generation_partial"
		result.ExpectedItems = len(items)
		result.NextAction = "inspect or clear the partially persisted item before recovering"
		return result, err
	}
	result.ExpectedItems = len(items)
	result.RecoveredItems = len(plan.CompleteItemIDs)
	result.MissingItems = len(plan.ToGenerate)
	result.MissingItemIDs = plan.MissingItemIDs
	result.PersistedOptions = plan.ExistingGenerated

	if len(plan.ToGenerate) == 0 {
		// Every expected item is persisted — recovery imported nothing new (the
		// per-item commits already did the work) but the run is salvageable.
		result.Classification = "generation_complete"
		if advanceState {
			if err := advanceSkillOptTrainToOptionsGenerated(ctx, store, session, *iteration, plan.ExistingGenerated); err != nil {
				return result, err
			}
			result.StateAdvanced = true
			result.CurrentPhase = skillopt.TrainStateOptionsGenerated
			result.NextAction = "publish the human review packet"
			return result, nil
		}
		result.NextAction = "rerun with --advance-state to advance the iteration to options_generated"
		return result, nil
	}

	// Some items are still missing. Import-only recovery does not regenerate them
	// (deferred to a future --regenerate), so it never advances state.
	result.Classification = "generation_incomplete"
	result.NextAction = "rerun train continue to regenerate the missing items"
	return result, nil
}

// advanceSkillOptTrainToOptionsGenerated moves an iteration whose options are all
// persisted to options_generated, recording recovered generation metadata. It is
// used by recover --generation --advance-state.
func advanceSkillOptTrainToOptionsGenerated(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, generatedOptions int) error {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateOptionsGenerated); err != nil {
		return err
	}
	metadata := map[string]any{
		"status":            "recovered",
		"generated_options": generatedOptions,
		"recovered_via":     "recover --generation --advance-state",
		"completed_at":      time.Now().UTC().Format(time.RFC3339Nano),
	}
	session.State = skillopt.TrainStateOptionsGenerated
	iteration.State = skillopt.TrainStateOptionsGenerated
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "generation", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "generation", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func markSkillOptTrainOptimizerRecoveredComplete(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths) (db.SkillOptTrainSession, *db.SkillOptTrainIteration, error) {
	if err := validateSkillOptTrainOptimizerRecoverableCompleteState(iteration); err != nil {
		return db.SkillOptTrainSession{}, nil, err
	}
	if skillopt.NormalizeTrainState(iteration.State) == skillopt.TrainStateOptimizerCompleted {
		return session, &iteration, nil
	}
	metadata := map[string]any{
		"status":                 "recovered",
		"training_package":       paths.TrainingPackagePath,
		"out_root":               paths.OutRoot,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"artifact_root":          paths.ArtifactRoot,
		"candidate_output":       paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train recover",
	}
	session.State = skillopt.TrainStateOptimizerCompleted
	iteration.State = skillopt.TrainStateOptimizerCompleted
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return db.SkillOptTrainSession{}, nil, err
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
		return db.SkillOptTrainSession{}, nil, err
	}
	return session, &iteration, nil
}

func existingSkillOptTrainOptimizerArtifacts(paths skillOptTrainOptimizerPaths) []string {
	candidates := []string{
		paths.CandidatePackagePath,
		filepath.Join(paths.OutRoot, "summary.json"),
		filepath.Join(paths.OutRoot, "runtime_state.json"),
		filepath.Join(paths.OutRoot, "history.json"),
		filepath.Join(paths.OutRoot, "best_skill.md"),
	}
	existing := []string{}
	for _, path := range candidates {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		}
	}
	return existing
}

func noCandidateReasonFromSkillOptTrainOptimizerArtifacts(paths skillOptTrainOptimizerPaths) string {
	for _, path := range []string{
		filepath.Join(paths.OutRoot, "summary.json"),
		filepath.Join(paths.OutRoot, "runtime_state.json"),
		filepath.Join(paths.OutRoot, "history.json"),
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var data any
		if err := json.Unmarshal(content, &data); err != nil {
			continue
		}
		if reason := noCandidateReasonFromValue(data); reason != "" {
			return reason
		}
	}
	return ""
}

func noCandidateReasonFromValue(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if raw, ok := typed["no_candidate_reason"]; ok {
			if text, ok := raw.(string); ok {
				reason := strings.TrimSpace(text)
				if reason != "" {
					return reason
				}
			}
		}
		for _, nested := range typed {
			if reason := noCandidateReasonFromValue(nested); reason != "" {
				return reason
			}
		}
	case []any:
		for _, nested := range typed {
			if reason := noCandidateReasonFromValue(nested); reason != "" {
				return reason
			}
		}
	}
	return ""
}

func printSkillOptTrainRecoverResult(stdout io.Writer, result skillOptTrainRecoverResult) {
	writeLine(stdout, "session: %s", result.SessionID)
	writeLine(stdout, "iteration: %s", emptyText(result.IterationID))
	writeLine(stdout, "recovery_state: %s", emptyText(result.Classification))
	writeLine(stdout, "current_phase: %s", emptyText(result.CurrentPhase))
	if result.Mode == "generation" {
		printSkillOptTrainGenerationRecoverResult(stdout, result)
		return
	}
	writeLine(stdout, "recovery_available: %t", result.RecoveryAvailable)
	writeLine(stdout, "optimizer_out_root: %s", emptyText(result.OutRoot))
	writeLine(stdout, "optimizer_root: %s", emptyText(result.OptimizerRoot))
	writeLine(stdout, "optimizer_attempt: %s", emptyText(result.OptimizerAttempt))
	writeLine(stdout, "optimizer_attempt_path: %s", emptyText(result.OptimizerAttemptPath))
	writeLine(stdout, "candidate_package: %s", emptyText(result.CandidatePackagePath))
	writeLine(stdout, "artifact_dir: %s", emptyText(result.ArtifactDir))
	writeLine(stdout, "candidate: %s", emptyText(result.CandidateVersionID))
	if result.NoCandidateReason != "" {
		writeLine(stdout, "no_candidate_reason: %s", result.NoCandidateReason)
	}
	if len(result.Artifacts) > 0 {
		writeLine(stdout, "artifacts: %s", strings.Join(result.Artifacts, ","))
	}
	writeLine(stdout, "next: %s", emptyText(result.NextAction))
}

func printSkillOptTrainGenerationRecoverResult(stdout io.Writer, result skillOptTrainRecoverResult) {
	writeLine(stdout, "mode: generation")
	writeLine(stdout, "lock_state: %s", emptyText(result.LockState))
	writeLine(stdout, "lock_reclaimed: %t", result.LockReclaimed)
	if result.LockOwnerJobID != "" {
		writeLine(stdout, "lock_owner_job_id: %s", result.LockOwnerJobID)
	}
	if result.LockOwnerPID > 0 {
		writeLine(stdout, "lock_owner_pid: %d", result.LockOwnerPID)
	}
	if result.LockOwnerHostname != "" {
		writeLine(stdout, "lock_owner_hostname: %s", result.LockOwnerHostname)
	}
	writeLine(stdout, "expected_items: %d", result.ExpectedItems)
	writeLine(stdout, "recovered_items: %d", result.RecoveredItems)
	writeLine(stdout, "missing_items: %d", result.MissingItems)
	writeLine(stdout, "persisted_options: %d", result.PersistedOptions)
	writeLine(stdout, "state_advanced: %t", result.StateAdvanced)
	if len(result.MissingItemIDs) > 0 {
		writeLine(stdout, "missing_item_ids: %s", strings.Join(result.MissingItemIDs, ","))
	}
	writeLine(stdout, "next: %s", emptyText(result.NextAction))
}

// isCodexFamilyBackend reports whether a gitmoot-skillopt backend runs through
// the codex CLI and would otherwise default to gpt-4o: the "codex" chat backend
// (optimizer/evaluator) and the "codex_exec" target backend.
func isCodexFamilyBackend(backend string) bool {
	switch strings.TrimSpace(backend) {
	case runtime.CodexRuntime, "codex_exec":
		return true
	default:
		return false
	}
}

func skillOptTrainOptimizerExecutable(request skillOptTrainOptimizerRequest) string {
	executable := strings.TrimSpace(request.SkillOptBin)
	if executable == "" {
		executable = "gitmoot-skillopt"
	}
	return executable
}

func resolveSkillOptTrainOptimizerExecutable(request skillOptTrainOptimizerRequest) (string, error) {
	executable := skillOptTrainOptimizerExecutable(request)
	resolved, err := skillOptTrainOptimizerRunner.LookPath(executable)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", fmt.Errorf("resolve gitmoot-skillopt executable %q: %w", resolved, err)
		}
	}
	return resolved, nil
}

func preflightSkillOptTrainOptimizerExecutable(ctx context.Context, request skillOptTrainOptimizerRequest) (string, subprocess.Result, error) {
	requested := skillOptTrainOptimizerExecutable(request)
	resolved, err := resolveSkillOptTrainOptimizerExecutable(request)
	if err != nil {
		return requested, subprocess.Result{}, skillOptTrainOptimizerPreflightError{
			Executable: requested,
			Step:       "find executable",
			Err:        err,
		}
	}
	result, err := skillOptTrainOptimizerRunner.Run(ctx, "", resolved, "--version")
	if err != nil {
		return resolved, result, skillOptTrainOptimizerPreflightError{
			Executable: resolved,
			Step:       "version check",
			Result:     result,
			Err:        err,
		}
	}
	result, err = skillOptTrainOptimizerRunner.Run(ctx, "", resolved, "optimize", "--help")
	if err != nil {
		return resolved, result, skillOptTrainOptimizerPreflightError{
			Executable: resolved,
			Step:       "optimize help check",
			Result:     result,
			Err:        err,
		}
	}
	return resolved, result, nil
}

func buildSkillOptTrainOptimizerCommand(iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths) (string, []string, error) {
	resolvedRequest, _, err := resolveSkillOptTrainBackendRequest(request)
	if err != nil {
		return "", nil, err
	}
	request = resolvedRequest
	resolved, err := resolveSkillOptTrainOptimizerExecutable(request)
	if err != nil {
		return "", nil, fmt.Errorf("find gitmoot-skillopt executable %q: %w", skillOptTrainOptimizerExecutable(request), err)
	}
	gate, err := skillOptTrainOptimizerGate(iteration, request)
	if err != nil {
		return "", nil, err
	}
	args := []string{
		"optimize",
		"--training-package", paths.TrainingPackagePath,
		"--artifact-root", paths.ArtifactRoot,
		"--out-root", paths.OutRoot,
		"--candidate-output", paths.CandidatePackagePath,
		"--artifact-dir", paths.ArtifactDir,
		"--gate-metric", gate,
	}
	optimizerModel := strings.TrimSpace(request.OptimizerModel)
	targetModel := strings.TrimSpace(request.TargetModel)
	if model := strings.TrimSpace(request.Model); model != "" {
		if optimizerModel == "" {
			optimizerModel = model
		}
		if targetModel == "" {
			targetModel = model
		}
	}
	// When a role runs on a codex-family backend with no model set, default to the
	// model the codex CLI is configured with. gitmoot-skillopt would otherwise
	// fall back to gpt-4o (its OPTIMIZER/TARGET_DEPLOYMENT default), which a
	// ChatGPT-account codex login rejects. The codex preset resolves the optimizer
	// to "codex" and the target to "codex_exec" — both pass an explicit model to
	// codex, so both need the override. The evaluator is left alone: it inherits
	// the optimizer model in gitmoot-skillopt when --evaluator-model is omitted.
	optimizerBackend := firstNonEmpty(strings.TrimSpace(request.OptimizerBackend), strings.TrimSpace(request.Backend))
	targetBackend := firstNonEmpty(strings.TrimSpace(request.TargetBackend), strings.TrimSpace(request.Backend))
	wantOptimizerCodexModel := optimizerModel == "" && isCodexFamilyBackend(optimizerBackend)
	wantTargetCodexModel := targetModel == "" && isCodexFamilyBackend(targetBackend)
	if wantOptimizerCodexModel || wantTargetCodexModel {
		if codexModel, _ := runtime.ConfiguredCodexModel(); codexModel != "" {
			if wantOptimizerCodexModel {
				optimizerModel = codexModel
			}
			if wantTargetCodexModel {
				targetModel = codexModel
			}
		}
	}
	if optimizerModel != "" {
		args = append(args, "--optimizer-model", optimizerModel)
	}
	if targetModel != "" {
		args = append(args, "--target-model", targetModel)
	}
	if optimizerBackend := strings.TrimSpace(request.OptimizerBackend); optimizerBackend != "" {
		args = append(args, "--optimizer-backend", optimizerBackend)
	}
	if targetBackend := strings.TrimSpace(request.TargetBackend); targetBackend != "" {
		args = append(args, "--target-backend", targetBackend)
	}
	if evaluatorID := strings.TrimSpace(request.EvaluatorID); evaluatorID != "" {
		args = append(args, "--evaluator-id", evaluatorID)
	}
	if evaluatorModel := strings.TrimSpace(request.EvaluatorModel); evaluatorModel != "" {
		args = append(args, "--evaluator-model", evaluatorModel)
	}
	if evaluatorBackend := strings.TrimSpace(request.EvaluatorBackend); evaluatorBackend != "" {
		args = append(args, "--evaluator-backend", evaluatorBackend)
	}
	if skillUpdateMode := strings.TrimSpace(request.SkillUpdateMode); skillUpdateMode != "" {
		args = append(args, "--skill-update-mode", skillUpdateMode)
	}
	if request.NumEpochs > 0 {
		args = append(args, "--num-epochs", strconv.Itoa(request.NumEpochs))
	}
	if request.BatchSize > 0 {
		args = append(args, "--batch-size", strconv.Itoa(request.BatchSize))
	}
	if request.OptimizerViewsSet {
		args = append(args, "--optimizer-views", strconv.Itoa(request.OptimizerViews))
	}
	if request.RetryOptimizerViewsSet {
		args = append(args, "--retry-optimizer-views", strings.TrimSpace(request.RetryOptimizerViews))
	}
	if request.NoopRetryBudgetSet {
		args = append(args, "--noop-retry-budget", strconv.Itoa(request.NoopRetryBudget))
	}
	if request.GateRejectRetryBudgetSet {
		args = append(args, "--gate-reject-retry-budget", strconv.Itoa(request.GateRejectRetryBudget))
	}
	if request.WrongArtifactRetryBudgetSet {
		args = append(args, "--wrong-artifact-retry-budget", strconv.Itoa(request.WrongArtifactRetryBudget))
	}
	if request.TargetArtifactRetryBudgetSet {
		args = append(args, "--target-artifact-retry-budget", strconv.Itoa(request.TargetArtifactRetryBudget))
	}
	if request.HardFailureRetryBudgetSet {
		args = append(args, "--hard-failure-retry-budget", strconv.Itoa(request.HardFailureRetryBudget))
	}
	if feedbackDirectMode := strings.TrimSpace(request.FeedbackDirectMode); feedbackDirectMode != "" {
		args = append(args, "--feedback-direct-mode", feedbackDirectMode)
	}
	if request.FinalEval {
		args = append(args, "--eval-test")
	}
	if request.DryRun {
		args = append(args, "--dry-run")
	}
	return resolved, args, nil
}

func skillOptTrainOptimizerGate(iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest) (string, error) {
	gate := strings.TrimSpace(strings.ToLower(request.Gate))
	if gate == "" {
		gate = skillOptMetadataString(iteration.MetadataJSON, "evaluation", "preferred_gate")
	}
	gate = strings.TrimSpace(strings.ToLower(gate))
	if gate == "hard_then_soft" {
		gate = "mixed"
	}
	if gate == "" {
		gate = "hard"
	}
	switch gate {
	case "hard", "soft", "mixed":
		return gate, nil
	default:
		return "", fmt.Errorf("optimizer gate %q is not supported; use hard, soft, or mixed", gate)
	}
}

// announceSkillOptTrainOptimizerLaunch notifies the operator that a long-lived
// optimizer run is about to start, since the run blocks with no streamed output
// until it completes. It is a notice only and never blocks for confirmation, so
// automated continue flows are unaffected. progress is nil for callers without a
// terminal.
func announceSkillOptTrainOptimizerLaunch(progress io.Writer, request skillOptTrainOptimizerRequest) {
	if progress == nil {
		return
	}
	if request.DryRun {
		fmt.Fprintln(progress, "skillopt train continue: launching optimizer dry run; this skips model calls but may still take a while")
		return
	}
	fmt.Fprintln(progress, "skillopt train continue: launching optimizer; this runs long-lived model calls and will not stream output until it finishes")
}

// skillOptTrainOptimizerProgressInterval is how often the optimizer heartbeat
// reports elapsed time; it is a var so tests can shorten it.
var skillOptTrainOptimizerProgressInterval = 30 * time.Second

// startSkillOptTrainOptimizerHeartbeat prints an elapsed-time line every
// interval while the (output-buffered, long-lived) optimizer subprocess runs,
// so the operator can tell it is alive. It returns a stop func; a nil progress
// writer makes it a no-op.
func startSkillOptTrainOptimizerHeartbeat(progress io.Writer) func() {
	if progress == nil {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(skillOptTrainOptimizerProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(progress, "optimizer running - %s\n", formatShortDuration(time.Since(start)))
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

func runSkillOptTrainOptimizer(ctx context.Context, progress io.Writer, paths skillOptTrainOptimizerPaths, request skillOptTrainOptimizerRequest, command string, args []string) (subprocess.Result, error) {
	timeout := strings.TrimSpace(request.Timeout)
	if timeout != "" {
		duration, err := time.ParseDuration(timeout)
		if err != nil {
			return subprocess.Result{}, fmt.Errorf("parse optimizer timeout: %w", err)
		}
		if duration <= 0 {
			return subprocess.Result{}, errors.New("optimizer timeout must be greater than zero")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	if err := os.MkdirAll(paths.OutRoot, 0o755); err != nil {
		return subprocess.Result{}, fmt.Errorf("create optimizer out-root: %w", err)
	}
	// Stream the optimizer's progress lines live when the runner supports it:
	// in the detached phase-view child, progress IS the tailed session log, so
	// the minibatch/analyst/accept lines appear as they happen instead of only
	// after exit. The buffered Result is unchanged either way.
	var result subprocess.Result
	var err error
	if streamer, ok := skillOptTrainOptimizerRunner.(subprocess.StreamRunner); ok && progress != nil {
		result, err = streamer.RunStream(ctx, paths.OutRoot, progress, command, args...)
	} else {
		result, err = skillOptTrainOptimizerRunner.Run(ctx, paths.OutRoot, command, args...)
	}
	if err != nil {
		return result, fmt.Errorf("optimizer failed: %w%s", err, subprocessDiagnostics(result))
	}
	return result, nil
}

func subprocessDiagnostics(result subprocess.Result) string {
	stderr := strings.TrimSpace(result.Stderr)
	stdout := strings.TrimSpace(result.Stdout)
	switch {
	case stderr != "" && stdout != "":
		return fmt.Sprintf(" (stderr: %s; stdout: %s)", truncateForMetadata(stderr), truncateForMetadata(stdout))
	case stderr != "":
		return fmt.Sprintf(" (stderr: %s)", truncateForMetadata(stderr))
	case stdout != "":
		return fmt.Sprintf(" (stdout: %s)", truncateForMetadata(stdout))
	default:
		return ""
	}
}

func skillOptTrainOptimizerMetadata(request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths, command string, args []string, result subprocess.Result, status string, failure error) map[string]any {
	metadata := map[string]any{
		"status":                 status,
		"command":                command,
		"args":                   args,
		"training_package":       paths.TrainingPackagePath,
		"out_root":               paths.OutRoot,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"candidate_output":       paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"dry_run":                request.DryRun,
		"stdout":                 truncateForMetadata(result.Stdout),
		"stderr":                 truncateForMetadata(result.Stderr),
		"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train continue",
	}
	if failure != nil {
		metadata["error"] = failure.Error()
		if nextAction := skillOptTrainOptimizerFailureNextAction(failure); nextAction != "" {
			metadata["next_action"] = nextAction
		}
	}
	addSkillOptTrainOptimizerConfigMetadata(metadata, request)
	return metadata
}

func skillOptTrainOptimizerFailureNextAction(failure error) string {
	var preflightErr skillOptTrainOptimizerPreflightError
	if errors.As(failure, &preflightErr) {
		return skillOptTrainSkillOptInstallNextAction()
	}
	return ""
}

func addSkillOptTrainOptimizerConfigMetadata(metadata map[string]any, request skillOptTrainOptimizerRequest) {
	if metadata == nil {
		return
	}
	if mode := strings.TrimSpace(request.FeedbackDirectMode); mode != "" {
		metadata["feedback_direct_mode"] = mode
	}
	// Resolved backend/model identify WHAT is running; the phase view shows
	// them in its header.
	if value := firstNonEmpty(strings.TrimSpace(request.OptimizerBackend), strings.TrimSpace(request.Backend)); value != "" {
		metadata["run_optimizer_backend"] = value
	}
	if value := firstNonEmpty(strings.TrimSpace(request.OptimizerModel), strings.TrimSpace(request.Model)); value != "" {
		metadata["run_optimizer_model"] = value
	}
	if request.FinalEvalSet {
		metadata["final_eval"] = request.FinalEval
	} else if request.FinalEval {
		metadata["final_eval"] = true
	}
	if request.OptimizerViewsSet {
		metadata["optimizer_views"] = request.OptimizerViews
	}
	if request.RetryOptimizerViewsSet {
		metadata["retry_optimizer_views"] = strings.TrimSpace(request.RetryOptimizerViews)
	}
	if request.TargetArtifactRetryBudgetSet {
		metadata["target_artifact_retry_budget"] = request.TargetArtifactRetryBudget
	}
	if request.HardFailureRetryBudgetSet {
		metadata["hard_failure_retry_budget"] = request.HardFailureRetryBudget
	}
	if request.NoopRetryBudgetSet {
		metadata["noop_retry_budget"] = request.NoopRetryBudget
	}
	if request.GateRejectRetryBudgetSet {
		metadata["gate_reject_retry_budget"] = request.GateRejectRetryBudget
	}
	if request.WrongArtifactRetryBudgetSet {
		metadata["wrong_artifact_retry_budget"] = request.WrongArtifactRetryBudget
	}
}

func recordSkillOptTrainOptimizerStarted(ctx context.Context, store *db.Store, session *db.SkillOptTrainSession, iteration *db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths, command string, args []string) error {
	metadata := map[string]any{
		"status":                 "running",
		"command":                command,
		"args":                   args,
		"training_package":       paths.TrainingPackagePath,
		"out_root":               paths.OutRoot,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"candidate_output":       paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"dry_run":                request.DryRun,
		"started_at":             time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train continue",
	}
	addSkillOptTrainOptimizerConfigMetadata(metadata, request)
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, *session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, *iteration)
}

func recordSkillOptTrainOptimizerFailure(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths, command string, args []string, result subprocess.Result, failure error) error {
	metadata := skillOptTrainOptimizerMetadata(request, paths, command, args, result, "failed", failure)
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func recordSkillOptTrainCandidateImportFailure(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths, failure error) error {
	metadata := map[string]any{
		"status":                 "failed",
		"candidate_package":      paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"error":                  failure.Error(),
		"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train continue",
	}
	session.State = skillopt.TrainStateOptimizerCompleted
	iteration.State = skillopt.TrainStateOptimizerCompleted
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func recordSkillOptTrainNoCandidate(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths, reason string) error {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateOptimizerCompletedNoCandidate); err != nil {
		return err
	}
	packageReason, packageNextAction, packageDetails := skillOptNoCandidatePackageMetadata(paths.CandidatePackagePath)
	if strings.TrimSpace(packageReason) != "" {
		reason = packageReason
	}
	nextAction := skillOptNoCandidateNextAction()
	if strings.TrimSpace(packageNextAction) != "" {
		nextAction = packageNextAction
	}
	metadata := map[string]any{
		"status":                 "no_candidate",
		"candidate_package":      paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"no_candidate_reason":    reason,
		"next_action":            nextAction,
		"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train continue",
	}
	if len(packageDetails) > 0 {
		metadata["no_candidate_details"] = packageDetails
	}
	session.State = skillopt.TrainStateOptimizerCompletedNoCandidate
	iteration.State = skillopt.TrainStateOptimizerCompletedNoCandidate
	iteration.CandidateVersionID = ""
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func skillOptNoCandidateReason(err error) string {
	text := strings.TrimSpace(err.Error())
	marker := skillopt.ErrNoCandidate.Error() + ":"
	if index := strings.LastIndex(text, marker); index >= 0 {
		return strings.TrimSpace(text[index+len(marker):])
	}
	return text
}

func skillOptNoCandidateReasonAndNextAction(err error, candidatePackagePath string) (string, string) {
	reason := skillOptNoCandidateReason(err)
	packageReason, packageNextAction, _ := skillOptNoCandidatePackageMetadata(candidatePackagePath)
	if strings.TrimSpace(packageReason) != "" {
		reason = packageReason
	}
	nextAction := skillOptNoCandidateNextAction()
	if strings.TrimSpace(packageNextAction) != "" {
		nextAction = packageNextAction
	}
	return reason, nextAction
}

func skillOptNoCandidateNextAction() string {
	return "do not publish a candidate review; revise feedback and start another iteration, rerun the optimizer with --rerun-optimizer, or stop"
}

func skillOptNoCandidatePackageMetadata(candidatePackagePath string) (string, string, map[string]any) {
	candidatePackagePath = strings.TrimSpace(candidatePackagePath)
	if candidatePackagePath == "" {
		return "", "", nil
	}
	content, err := os.ReadFile(candidatePackagePath)
	if err != nil {
		return "", "", nil
	}
	var raw map[string]any
	if err := json.Unmarshal(content, &raw); err != nil {
		return "", "", nil
	}
	summary := decodedSkillOptMetadataValue(raw["summary"])
	metadata := decodedSkillOptMetadataValue(summary["metadata"])
	evalReport := decodedSkillOptMetadataValue(raw["eval_report"])
	reason := ""
	nextAction := ""
	var details map[string]any
	for _, source := range []map[string]any{evalReport, metadata} {
		if skillOptCandidateReviewExplicitPromotable(source) {
			continue
		}
		if reason == "" {
			reason = metadataString(source, "no_candidate_reason")
		}
		if nextAction == "" {
			nextAction = metadataString(source, "next_action")
		}
		if len(details) == 0 {
			details = decodedSkillOptMetadataValue(source["no_candidate_details"])
		}
		details = skillOptNoCandidateDetailsWithDiagnostics(details, source)
	}
	for _, gateRejection := range []map[string]any{
		decodedSkillOptMetadataValue(summary["gate_rejection"]),
		decodedSkillOptMetadataValue(decodedSkillOptMetadataValue(summary["evaluator_score"])["gate_rejection"]),
		decodedSkillOptMetadataValue(evalReport["gate_rejection"]),
	} {
		if len(gateRejection) == 0 {
			continue
		}
		if reason == "" {
			reason = metadataString(gateRejection, "rejection_type")
		}
		if reason == "" {
			reason = metadataString(gateRejection, "primary_reason")
		}
		if nextAction == "" {
			nextAction = metadataString(gateRejection, "next_action")
		}
		if len(details) == 0 {
			details = skillOptNoCandidateDetailsFromGateRejection(gateRejection)
		}
		details = skillOptNoCandidateDetailsWithDiagnostics(details, gateRejection)
	}
	if reason == "" {
		return "", "", nil
	}
	return reason, nextAction, details
}

func skillOptNoCandidateDetailsWithDiagnostics(details map[string]any, source map[string]any) map[string]any {
	if len(source) == 0 {
		return skillOptNoCandidateDetailsWithFeedbackContext(details, nil)
	}
	diagnostics := decodedSkillOptMetadataValue(source["no_candidate_diagnostics"])
	if len(diagnostics) == 0 {
		diagnostics = decodedSkillOptMetadataValue(source["diagnostics"])
	}
	if len(diagnostics) == 0 {
		diagnostics = map[string]any{}
	}
	if categories := metadataStringSlice(source, "diagnostic_categories"); len(categories) > 0 && len(metadataStringSlice(diagnostics, "categories")) == 0 {
		diagnostics["categories"] = categories
	}
	for _, key := range []string{"selection_gate_relation", "stop_reason"} {
		if value := metadataString(source, key); value != "" && metadataString(diagnostics, key) == "" {
			diagnostics[key] = value
		}
	}
	if value := metadataBoolPtr(source, "retry_budget_exhausted"); value != nil && metadataBoolPtr(diagnostics, "retry_budget_exhausted") == nil {
		diagnostics["retry_budget_exhausted"] = *value
	}
	if stopReasons := metadataStringSlice(source, "retry_stop_reasons"); len(stopReasons) > 0 && len(metadataStringSlice(diagnostics, "retry_stop_reasons")) == 0 {
		diagnostics["retry_stop_reasons"] = stopReasons
	}
	if len(diagnostics) == 0 && len(metadataStringSlice(source, "feedback_themes")) == 0 {
		return skillOptNoCandidateDetailsWithFeedbackContext(details, source)
	}
	if details == nil {
		details = map[string]any{}
	}
	if len(diagnostics) > 0 {
		details["diagnostics"] = diagnostics
		if categories := metadataStringSlice(diagnostics, "categories"); len(categories) > 0 {
			details["diagnostic_categories"] = categories
		}
		for _, key := range []string{"selection_gate_relation", "stop_reason"} {
			if value := metadataString(diagnostics, key); value != "" {
				details[key] = value
			}
		}
		if value := metadataBoolPtr(diagnostics, "retry_budget_exhausted"); value != nil {
			details["retry_budget_exhausted"] = *value
		}
		if stopReasons := metadataStringSlice(diagnostics, "retry_stop_reasons"); len(stopReasons) > 0 {
			details["retry_stop_reasons"] = stopReasons
		}
	}
	if themes := metadataStringSlice(source, "feedback_themes"); len(themes) > 0 {
		details["feedback_themes"] = themes
	}
	if retryBudget := skillOptRetryBudgetFromAttempts(metadataString(details, "retry_attempts")); retryBudget != "" && metadataString(details, "retry_budget") == "" {
		details["retry_budget"] = retryBudget
	}
	return skillOptNoCandidateDetailsWithFeedbackContext(details, source)
}

func skillOptNoCandidateDetailsFromGateRejection(gateRejection map[string]any) map[string]any {
	if len(gateRejection) == 0 {
		return nil
	}
	details := map[string]any{}
	for _, key := range []string{"attempted_patch", "retry_attempts"} {
		if value := metadataString(gateRejection, key); value != "" {
			details[key] = value
		}
	}
	nextActions := metadataStringSlice(gateRejection, "next_actions")
	if len(nextActions) == 0 {
		nextActions = metadataStringSlice(gateRejection, "next_action")
	}
	if len(nextActions) > 0 {
		details["next_action"] = nextActions[0]
		details["next_actions"] = nextActions
	}
	if retryBudget := skillOptRetryBudgetFromAttempts(metadataString(gateRejection, "retry_attempts")); retryBudget != "" {
		details["retry_budget"] = retryBudget
	}
	if value := metadataBoolPtr(gateRejection, "duplicate_retry_detected"); value != nil {
		details["duplicate_retry_detected"] = *value
	}
	rejection := map[string]any{}
	for _, key := range []string{"baseline", "candidate"} {
		if value := decodedSkillOptMetadataValue(gateRejection[key]); len(value) > 0 {
			rejection[key] = value
			if gateScore := metadataString(value, "gate_score"); gateScore != "" {
				details[key+"_gate"] = gateScore
			}
			if hard := metadataString(value, "hard"); hard != "" {
				details[key+"_hard"] = hard
			}
			if soft := metadataString(value, "soft"); soft != "" {
				details[key+"_soft"] = soft
			}
		}
	}
	if evaluatorReason := skillOptNoCandidateEvaluatorReason(gateRejection, rejection); evaluatorReason != "" {
		details["evaluator_reason"] = evaluatorReason
	}
	for _, key := range []string{"primary_reason", "human_reason", "optimizer_hint"} {
		if value := metadataString(gateRejection, key); value != "" {
			rejection[key] = value
			if key == "optimizer_hint" {
				details["optimizer_hint"] = value
			}
		}
	}
	for _, key := range []string{"failed_dimensions", "evidence"} {
		if value := metadataStringSlice(gateRejection, key); len(value) > 0 {
			rejection[key] = value
			if key == "failed_dimensions" {
				details["failed_dimensions"] = value
			}
		}
	}
	if humanFeedbackContext := decodedSkillOptMetadataValue(gateRejection["human_feedback_context"]); len(humanFeedbackContext) > 0 {
		rejection["human_feedback_context"] = humanFeedbackContext
	}
	if len(rejection) > 0 {
		details["rejection"] = rejection
	}
	return skillOptNoCandidateDetailsWithFeedbackContext(details, gateRejection)
}

func skillOptRetryBudgetFromAttempts(retryAttempts string) string {
	retryAttempts = strings.TrimSpace(retryAttempts)
	if retryAttempts == "" {
		return ""
	}
	parts := strings.Split(retryAttempts, "/")
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func skillOptNoCandidateDetailsWithFeedbackContext(details map[string]any, source map[string]any) map[string]any {
	context := decodedSkillOptMetadataValue(nil)
	if len(details) > 0 {
		context = decodedSkillOptMetadataValue(details["human_feedback_context"])
	}
	if len(context) == 0 && len(source) > 0 {
		context = decodedSkillOptMetadataValue(source["human_feedback_context"])
	}
	if len(context) == 0 {
		return details
	}
	if details == nil {
		details = map[string]any{}
	}
	details["human_feedback_context"] = context
	for _, key := range []string{"feedback_source", "feedback_target", "review_issue", "review_run_id", "reviewed_skill_version"} {
		if value := metadataString(context, key); value != "" && metadataString(details, key) == "" {
			details[key] = value
		}
	}
	if metadataString(details, "score_basis") == "" && strings.EqualFold(metadataString(context, "feedback_target"), "baseline_review_outputs") {
		details["score_basis"] = "feedback_resolution"
	}
	if len(metadataStringSlice(details, "feedback_themes")) == 0 {
		if themes := metadataStringSlice(context, "themes"); len(themes) > 0 {
			details["feedback_themes"] = themes
		}
	}
	return details
}

func skillOptNoCandidateEvaluatorReason(gateRejection map[string]any, rejection map[string]any) string {
	for _, source := range []map[string]any{
		decodedSkillOptMetadataValue(rejection["candidate"]),
		decodedSkillOptMetadataValue(rejection["baseline"]),
		gateRejection,
	} {
		for _, key := range []string{"evaluator_reason", "evaluator_reasoning", "reasoning", "human_reason", "optimizer_hint", "primary_reason"} {
			if value := metadataString(source, key); value != "" {
				return value
			}
		}
	}
	return ""
}

func skillOptTrainOptimizerReportLines(result skillOptTrainOptimizerResult) []string {
	lines := []string{
		fmt.Sprintf("training_package: %s", result.TrainingPackagePath),
		fmt.Sprintf("optimizer_out_root: %s", result.OutRoot),
		fmt.Sprintf("optimizer_root: %s", result.OptimizerRoot),
		fmt.Sprintf("optimizer_attempt: %s", result.OptimizerAttempt),
		fmt.Sprintf("optimizer_attempt_path: %s", result.OptimizerAttemptPath),
		fmt.Sprintf("candidate_package: %s", result.CandidatePackagePath),
		fmt.Sprintf("artifact_dir: %s", result.ArtifactDir),
		fmt.Sprintf("backend: %s", result.BackendResolution.Backend),
		fmt.Sprintf("optimizer_backend: %s", emptyText(result.BackendResolution.OptimizerBackend)),
		fmt.Sprintf("target_backend: %s", emptyText(result.BackendResolution.TargetBackend)),
		fmt.Sprintf("internal_target_adapter: %s", emptyText(result.BackendResolution.InternalTargetAdapter)),
		fmt.Sprintf("evaluator_backend: %s", emptyText(result.BackendResolution.EvaluatorBackend)),
		fmt.Sprintf("backend_config_status: %s", result.BackendResolution.ConfigStatus),
		fmt.Sprintf("optimizer_lock: %s", result.OptimizerLockState),
		fmt.Sprintf("recovery_available: %t", result.RecoveryAvailable),
	}
	if strings.TrimSpace(result.Command) != "" {
		lines = append(lines, fmt.Sprintf("optimizer_command: %s", shellArgs(append([]string{result.Command}, result.Args...))))
	} else {
		lines = append(lines, "optimizer_command: -")
	}
	if mode := strings.TrimSpace(result.Request.FeedbackDirectMode); mode != "" {
		lines = append(lines, fmt.Sprintf("feedback_direct_mode: %s", mode))
	}
	if result.Request.OptimizerViewsSet {
		lines = append(lines, fmt.Sprintf("optimizer_views: %d", result.Request.OptimizerViews))
	}
	if result.Request.RetryOptimizerViewsSet {
		lines = append(lines, fmt.Sprintf("retry_optimizer_views: %s", strings.TrimSpace(result.Request.RetryOptimizerViews)))
	}
	if result.Request.FinalEval {
		lines = append(lines, "final_eval: true")
	}
	if result.Request.TargetArtifactRetryBudgetSet {
		lines = append(lines, fmt.Sprintf("target_artifact_retry_budget: %d", result.Request.TargetArtifactRetryBudget))
	}
	if result.Request.HardFailureRetryBudgetSet {
		lines = append(lines, fmt.Sprintf("hard_failure_retry_budget: %d", result.Request.HardFailureRetryBudget))
	}
	lines = append(lines, fmt.Sprintf("optimizer_dry_run: %t", result.DryRun))
	if next := strings.TrimSpace(result.NextAction); next != "" {
		lines = append(lines, fmt.Sprintf("next: %s", next))
	}
	return lines
}

func skillOptTrainOptimizerResultHasReport(result skillOptTrainOptimizerResult) bool {
	return strings.TrimSpace(result.TrainingPackagePath) != "" ||
		strings.TrimSpace(result.OutRoot) != "" ||
		strings.TrimSpace(result.CandidatePackagePath) != "" ||
		strings.TrimSpace(result.ArtifactDir) != "" ||
		strings.TrimSpace(result.BackendResolution.Backend) != ""
}

func importSkillOptTrainCandidate(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, optimizerPaths skillOptTrainOptimizerPaths) (db.AgentTemplateVersion, error) {
	content, err := os.ReadFile(optimizerPaths.CandidatePackagePath)
	if err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("read optimizer candidate package: %w", err)
	}
	var candidate skillopt.CandidatePackage
	if err := json.Unmarshal(content, &candidate); err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("decode optimizer candidate package: %w", err)
	}
	if err := validateSkillOptTrainCandidatePackage(ctx, store, session, iteration, candidate); err != nil {
		return db.AgentTemplateVersion{}, err
	}
	version, err := skillopt.ImportCandidatePackageWithOptions(ctx, store, candidate, skillopt.CandidateImportOptions{
		SourcePath:  optimizerPaths.CandidatePackagePath,
		ArtifactDir: optimizerPaths.ArtifactDir,
		BlobStore:   artifact.NewStore(paths.ArtifactBlobs),
	})
	if err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("import optimizer candidate package: %w", err)
	}
	// Notify + (config-gated) auto-promote on the just-pending version (#471). The
	// import write above stays side-effect-pure; this is the separate post-import
	// step both CLI callers share. The train iteration carries the eval_run id whose
	// feedback events feed the min_samples / external-CI guardrails.
	if err := notifyAndMaybeAutoPromoteCandidate(ctx, store, paths.Home, candidate, version, iteration.EvalRunID); err != nil {
		return db.AgentTemplateVersion{}, err
	}
	return version, nil
}

func validateSkillOptTrainCandidatePackage(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidate skillopt.CandidatePackage) error {
	templateID := strings.TrimSpace(candidate.TemplateID)
	if templateID != strings.TrimSpace(session.TemplateID) {
		return fmt.Errorf("optimizer candidate template_id %q does not match train session template %q", templateID, strings.TrimSpace(session.TemplateID))
	}
	expectedBase := strings.TrimSpace(iteration.BaseTemplateVersionID)
	if expectedBase == "" {
		expectedBase = strings.TrimSpace(session.TemplateVersionID)
	}
	baseRef := strings.TrimSpace(candidate.BaseVersionID)
	if baseRef == "" {
		base, err := store.GetAgentTemplate(ctx, templateID)
		if err != nil {
			return fmt.Errorf("load optimizer candidate current base for %q: %w", templateID, err)
		}
		if base.VersionID != expectedBase {
			return fmt.Errorf("optimizer candidate omitted base_version_id and current base is %q, want active train base %q", base.VersionID, expectedBase)
		}
		return nil
	}
	base, err := store.GetAgentTemplateReference(ctx, baseRef)
	if err != nil {
		return fmt.Errorf("load optimizer candidate base version %q: %w", baseRef, err)
	}
	if base.ID != templateID {
		return fmt.Errorf("optimizer candidate base_version_id %q belongs to template %q, want %q", baseRef, base.ID, templateID)
	}
	if base.VersionID != expectedBase {
		return fmt.Errorf("optimizer candidate base_version_id %q resolved to %q, want active train base %q", baseRef, base.VersionID, expectedBase)
	}
	return nil
}

func truncateForMetadata(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 2000 {
		return value
	}
	return value[:2000] + "...<truncated>"
}
