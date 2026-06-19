package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jerryfane/gitmoot/internal/cli/tui"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/doctor"
	"github.com/jerryfane/gitmoot/internal/report"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// dashboardFlags is the subset of dashboard flags that decide whether the
// interactive TUI launches.
type dashboardFlags struct {
	plain      bool
	jsonOutput bool
	all        bool
	watch      bool
	answerID   string
	dismissID  string
}

// shouldLaunchTUI reports whether `gitmoot dashboard` should open the interactive
// TUI rather than print a one-shot snapshot. The TUI needs a real terminal on
// both stdout (to draw) and stdin (raw-mode keys), and must yield to every
// machine-output or one-shot mutation flag so existing behavior is preserved.
func shouldLaunchTUI(f dashboardFlags, stdoutTTY, stdinTTY bool) bool {
	if !stdoutTTY || !stdinTTY {
		return false
	}
	if f.plain || f.jsonOutput || f.all || f.watch {
		return false
	}
	if strings.TrimSpace(f.answerID) != "" || strings.TrimSpace(f.dismissID) != "" {
		return false
	}
	return true
}

// stdinIsCharDevice reports whether stdin is a terminal, so the TUI is not
// launched when stdin is piped or redirected (bubbletea needs raw-mode keys).
func stdinIsCharDevice() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// runDashboardTUI launches the bubbletea dashboard inside the Root router so
// pages can push full-screen child views (e.g. a train session's phase view).
// It returns a process exit code like the other dashboard paths.
func runDashboardTUI(home string, interval time.Duration, stdout, stderr io.Writer) int {
	deps := dashboardTUIDeps(home, interval)
	deps.OpenTrain = func(sessionID string) tea.Model {
		trainDeps := skillOptTrainRunDeps(home, func() string { return sessionID })
		trainDeps.Embedded = true
		return tui.NewTrainRun(trainDeps)
	}
	program := tea.NewProgram(
		tui.NewRoot(tui.New(deps)),
		tea.WithAltScreen(),
		tea.WithOutput(stdout),
	)
	_, err := program.Run()
	// Best-effort: a hard quit mid create-form leaves its prompt records
	// pending; sweep them so they do not haunt the attention list (mirrors
	// the standalone wizard's deferred cleanup).
	_ = withStore(home, func(store *db.Store) error {
		ids := append(tui.AgentCreatePromptIDs(), agentOptimizePromptIDs()...)
		for _, id := range ids {
			_ = store.DeleteInteractivePrompt(context.Background(), id)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return 1
	}
	return 0
}

// dashboardTUIDeps adapts the existing snapshot builder and prompt store APIs
// into the tui.Deps the model consumes. Each Load reopens the store (cheap, pure
// DB) so the TUI always reflects current state, including live train phases.
func dashboardTUIDeps(home string, interval time.Duration) tui.Deps {
	return tui.Deps{
		Interval:                interval,
		CollapseGroupsByDefault: true,
		Load: func() (tui.Snapshot, error) {
			paths, err := initializedPaths(home)
			if err != nil {
				return tui.Snapshot{}, err
			}
			snapshot, err := buildDashboardSnapshot(home, paths)
			if err != nil {
				return tui.Snapshot{}, err
			}
			return toTUISnapshot(snapshot), nil
		},
		Answer: func(id, value string) error {
			return withStore(home, func(store *db.Store) error {
				_, err := store.AnswerInteractivePrompt(context.Background(), id, value, "dashboard-tui")
				return err
			})
		},
		Dismiss: func(id string) error {
			return withStore(home, func(store *db.Store) error {
				return store.DeleteInteractivePrompt(context.Background(), id)
			})
		},
		JobEvents: func(id string) ([]tui.JobEventView, error) {
			var views []tui.JobEventView
			err := withStore(home, func(store *db.Store) error {
				events, err := store.ListJobEvents(context.Background(), id)
				if err != nil {
					return err
				}
				for _, event := range events {
					views = append(views, tui.JobEventView{Kind: event.Kind, Message: event.Message})
				}
				return nil
			})
			return views, err
		},
		JobDetail: func(id string) (tui.JobDetail, error) {
			// Lazy: parse the one opened job's payload, not every job per tick.
			var detail tui.JobDetail
			err := withStore(home, func(store *db.Store) error {
				job, err := store.GetJob(context.Background(), id)
				if err != nil {
					return err
				}
				payload, err := workflow.ParseJobPayload(job.Payload)
				if err != nil {
					return nil // malformed/absent payload → empty detail, no error
				}
				detail.Repo = payload.Repo
				detail.PullRequest = payload.PullRequest
				detail.Request = payload.Instructions
				if payload.Result != nil {
					detail.ResultDecision = payload.Result.Decision
					detail.ResultSummary = payload.Result.Summary
				}
				// Lazily load the delegation tree: child jobs this coordinator
				// spawned plus the continuation job, if any. Runs only for the
				// one opened job, never on the refresh tick.
				children, cerr := store.ListJobsByParent(context.Background(), id)
				if cerr != nil {
					return cerr
				}
				detail.Children, detail.ContinuationID, detail.ContinuationState =
					buildDelegationTree(payload, children)
				return nil
			})
			return detail, err
		},
		RetryJob: func(id string) error {
			// workflow.RetryJob is the same path `gitmoot job retry` takes: it
			// clears the stale payload.Result and emits the retry_queued event
			// the daemon's retry bookkeeping keys on.
			return withStore(home, func(store *db.Store) error {
				_, err := workflow.RetryJob(context.Background(), store, id)
				return err
			})
		},
		CancelJob: func(id string) error {
			return withStore(home, func(store *db.Store) error {
				_, err := workflow.CancelJob(context.Background(), store, id)
				return err
			})
		},
		BugReportPreview: func(id string) (tui.BugReportPreview, error) {
			var preview tui.BugReportPreview
			err := withStore(home, func(store *db.Store) error {
				draft, err := report.BuildJobReport(context.Background(), store, id, report.JobOptions{})
				if err != nil {
					return err
				}
				preview = tui.BugReportPreview{
					Title:       draft.Title,
					Body:        draft.Body,
					Labels:      draft.Labels,
					Fingerprint: draft.Fingerprint,
				}
				return nil
			})
			return preview, err
		},
		CreateBugReport: func(id string, preview tui.BugReportPreview) (tui.BugReportCreateResult, error) {
			if strings.TrimSpace(id) == "" {
				return tui.BugReportCreateResult{}, fmt.Errorf("job id is required")
			}
			draft := report.Report{
				Title:       preview.Title,
				Body:        preview.Body,
				Labels:      append([]string(nil), preview.Labels...),
				Fingerprint: preview.Fingerprint,
			}
			result, err := createBugReportIssueResult(context.Background(), newReportGitHubClient(), draft, nil)
			if err != nil {
				return tui.BugReportCreateResult{}, err
			}
			return tui.BugReportCreateResult{URL: result.URL, Existing: result.Existing}, nil
		},
		StopTrain: func(id, reason string) error {
			return withStore(home, func(store *db.Store) error {
				_, err := stopSkillOptTrainSession(context.Background(), store, id, reason)
				return err
			})
		},
		DeleteTrain: func(id string) ([]string, error) {
			var repos []string
			err := withStore(home, func(store *db.Store) error {
				var err error
				repos, err = deleteSkillOptTrainSession(context.Background(), store, id)
				return err
			})
			return repos, err
		},
		DeleteTrainRepo: func(repo string) error {
			return withStore(home, func(store *db.Store) error {
				return cleanupCreatedTrainRepo(context.Background(), store, repo)
			})
		},
		TemplateVersions: func(templateID string) ([]tui.TemplateVersion, error) {
			var views []tui.TemplateVersion
			err := withStore(home, func(store *db.Store) error {
				versions, err := store.ListAgentTemplateVersions(context.Background(), templateID)
				if err != nil {
					return err
				}
				for _, v := range versions {
					views = append(views, tui.TemplateVersion{
						ID:      v.ID,
						Number:  v.VersionNumber,
						State:   v.State,
						Name:    v.Name,
						Created: v.CreatedAt,
					})
				}
				return nil
			})
			return views, err
		},
		TemplateVersionContent: func(versionID string) (string, error) {
			var content string
			err := withStore(home, func(store *db.Store) error {
				version, err := store.GetAgentTemplateVersionByID(context.Background(), versionID)
				if err != nil {
					return err
				}
				content = version.Content
				return nil
			})
			return content, err
		},
		OpenAgentCreate: func() (tea.Model, error) {
			// One store for the form's lifetime: its 200ms external-answer poll
			// would otherwise open/migrate/close the database five times a
			// second. Closed from the Done hook; on a hard quit the process
			// exit releases it.
			paths, err := initializedPaths(home)
			if err != nil {
				return nil, err
			}
			store, err := db.Open(paths.Database)
			if err != nil {
				return nil, err
			}
			templates, err := skillopt.ListTrainInitTemplateChoices(context.Background(), store)
			if err != nil {
				store.Close()
				return nil, err
			}
			var choices []tui.Choice
			for _, t := range templates {
				// Only installed templates: registerAgentOnly persists the id
				// verbatim, and an uninstalled one yields an agent whose jobs
				// fail at delivery (the CLI register path gates the same way).
				if t.Installed {
					choices = append(choices, tui.Choice{Value: t.ID, Label: skillOptTrainInitTemplateChoiceLabel(t)})
				}
			}
			if len(choices) == 0 {
				store.Close()
				return nil, fmt.Errorf("no agent templates installed; add one with `gitmoot agent template add`")
			}
			form := tui.NewAgentCreateForm(formPromptStore{home: home, store: store}, choices)
			done := form.Done
			form.Done = func(res tui.Result) tea.Cmd {
				store.Close()
				return done(res)
			}
			return form, nil
		},
		CreateAgent: func(name, runtimeName, templateID string) error {
			return withStore(home, func(store *db.Store) error {
				return registerAgentOnly(context.Background(), store, name, runtimeName, templateID, "")
			})
		},
		DeleteAgent: func(name string) error {
			return withStore(home, func(store *db.Store) error {
				return store.DeleteAgentChecked(context.Background(), name)
			})
		},
		DeleteAgents: func(names []string) (int, []string, error) {
			deleted := 0
			var skipped, failed []string
			var firstErr error
			openErr := withStore(home, func(store *db.Store) error {
				for _, n := range names {
					switch e := store.DeleteAgentChecked(context.Background(), n); {
					case e == nil:
						deleted++
					case errors.Is(e, db.ErrAgentHasActiveJobs):
						// Still referenced by active jobs — skip, don't abort the batch.
						skipped = append(skipped, n)
					default:
						// Unexpected: record and keep going so the caller learns the full
						// outcome (each delete already committed in its own tx).
						failed = append(failed, n)
						if firstErr == nil {
							firstErr = e
						}
					}
				}
				return nil
			})
			if openErr != nil {
				return deleted, skipped, openErr
			}
			if len(failed) > 0 {
				return deleted, skipped, fmt.Errorf("%d agent(s) could not be deleted (e.g. %s: %v)", len(failed), failed[0], firstErr)
			}
			return deleted, skipped, nil
		},
		RevertTemplate: func(templateID, versionID string) error {
			return withStore(home, func(store *db.Store) error {
				_, err := store.RevertAgentTemplateVersion(context.Background(), templateID, versionID)
				return err
			})
		},
		SetAgentRuntime: func(name, runtimeName string) error {
			return withStore(home, func(store *db.Store) error {
				return store.UpdateAgentRuntime(context.Background(), name, runtimeName)
			})
		},
		StopSession: func(name string) error {
			return withStore(home, func(store *db.Store) error {
				return store.StopAgentInstance(context.Background(), name)
			})
		},
		EditAgentPrompt: func(seedTemplateID string) tea.Cmd {
			seed := agentPromptScaffold
			if strings.TrimSpace(seedTemplateID) != "" {
				_ = withStore(home, func(store *db.Store) error {
					tpl, err := store.GetAgentTemplate(context.Background(), seedTemplateID)
					if err == nil && strings.TrimSpace(tpl.Content) != "" {
						seed = tpl.Content
					}
					return nil
				})
			}
			return editAgentPromptCmd(seed)
		},
		CreateAgentWithPrompt: func(name, runtimeName, content string) error {
			return withStore(home, func(store *db.Store) error {
				templateID := slug(name)
				if templateID == "" {
					return fmt.Errorf("agent name %q has no usable characters for a template id", name)
				}
				if err := store.UpsertAgentTemplate(context.Background(), db.AgentTemplate{
					ID:      templateID,
					Name:    strings.TrimSpace(name),
					Content: content,
				}); err != nil {
					return err
				}
				return registerAgentOnly(context.Background(), store, name, runtimeName, templateID, "")
			})
		},
		OpenAgentOptimize: func(agent tui.Agent) (tea.Model, error) {
			// Same lifetime pattern as the create form: one store for the
			// form's 200ms poll, closed from the Done hook.
			paths, err := initializedPaths(home)
			if err != nil {
				return nil, err
			}
			store, err := db.Open(paths.Database)
			if err != nil {
				return nil, err
			}
			repoChoices := skillOptRepoPickerChoices(skillOptKnownRepoNames(context.Background(), store))
			form := tui.NewAgentOptimizeForm(formPromptStore{home: home, store: store},
				agent.TemplateID, buildAgentOptimizeFields(home, repoChoices),
				agentOptimizeSummaryRows(agent.TemplateID), agentOptimizeInterpret)
			done := form.Done
			form.Done = func(res tui.Result) tea.Cmd {
				store.Close()
				return done(res)
			}
			return form, nil
		},
		StartOptimize: func(templateID string, values map[string]string) (string, error) {
			return startAgentOptimizeSession(home, templateID, values)
		},
		StartDaemon: func() error {
			// Restart rather than start: it tolerates a stopped daemon and
			// restores the previously persisted flags (workers, poll, watch)
			// instead of silently reverting to defaults.
			var out bytes.Buffer
			if code := runDaemonRestart([]string{"--home", home}, &out, &out); code != 0 {
				return fmt.Errorf("daemon start failed: %s", strings.TrimSpace(out.String()))
			}
			return nil
		},
		EditConfig: func() tea.Cmd {
			paths, err := initializedPaths(home)
			if err != nil {
				return func() tea.Msg { return tui.ConfigEditedMsg{Err: err} }
			}
			return editConfigCmd(paths.ConfigFile)
		},
		ValidateConfig: func() []string {
			paths, err := initializedPaths(home)
			if err != nil {
				return []string{err.Error()}
			}
			return validateDashboardConfig(paths)
		},
		SetConfigScalar: func(keyPath []string, value string, kind tui.ConfigKind) error {
			paths, err := initializedPaths(home)
			if err != nil {
				return err
			}
			return config.SetConfigScalar(paths, keyPath, configScalarForKind(kind, value))
		},
		HealthChecks: func() ([]tui.HealthCheck, error) {
			ctx := context.Background()
			checker := doctor.Checker{}
			out := []tui.HealthCheck{}
			for _, c := range checker.GlobalChecks(ctx) {
				out = append(out, toTUIHealthCheck(c, ""))
			}
			// Per-repo checks run against each tracked repo's recorded checkout,
			// so the dashboard reports repo health from anywhere — not just the
			// current directory (#267).
			err := withStore(home, func(store *db.Store) error {
				repos, err := store.ListRepos(ctx)
				if err != nil {
					return err
				}
				for _, r := range repos {
					for _, c := range checker.RepoChecks(ctx, r.CheckoutPath) {
						out = append(out, toTUIHealthCheck(c, r.FullName()))
					}
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			return out, nil
		},
	}
}

// toTUIHealthCheck maps a doctor.Check to a tui.HealthCheck, deriving the status
// string and tagging it with scope ("" for global, "owner/repo" for per-repo).
func toTUIHealthCheck(c doctor.Check, scope string) tui.HealthCheck {
	status := "ok"
	if !c.OK {
		if c.Required {
			status = "fail"
		} else {
			status = "warn"
		}
	}
	return tui.HealthCheck{Name: c.Name, Status: status, Detail: c.Detail, Required: c.Required, Scope: scope}
}

// formPromptStore is the PromptStore for pushed forms: the hot 200ms answer
// poll reads through one held store, while the rare upserts/deletes go through
// withStore so they still work after the held store is closed from the form's
// Done hook (abort batches its prompt delete with the pop).
type formPromptStore struct {
	home  string
	store *db.Store
}

func (s formPromptStore) UpsertInteractivePrompt(ctx context.Context, prompt db.InteractivePrompt) error {
	return withStore(s.home, func(store *db.Store) error {
		return store.UpsertInteractivePrompt(ctx, prompt)
	})
}

func (s formPromptStore) GetInteractivePrompt(ctx context.Context, id string) (db.InteractivePrompt, error) {
	return s.store.GetInteractivePrompt(ctx, id)
}

func (s formPromptStore) DeleteInteractivePrompt(ctx context.Context, id string) error {
	return withStore(s.home, func(store *db.Store) error {
		return store.DeleteInteractivePrompt(ctx, id)
	})
}

// toTUISnapshot copies the cli dashboardSnapshot into the tui-facing Snapshot.
func toTUISnapshot(s dashboardSnapshot) tui.Snapshot {
	out := tui.Snapshot{
		Home:           s.Home,
		DatabaseExists: s.DatabaseExists,
		Daemon: tui.Daemon{
			Running:   s.Daemon.Running,
			PID:       s.Daemon.PID,
			LogFile:   s.Daemon.LogFile,
			Flags:     s.daemonDetail.Flags,
			WorkDir:   s.daemonDetail.WorkDir,
			StartedAt: s.daemonDetail.StartedAt,
			LogErrors: s.daemonDetail.LogErrors,
		},
		Jobs:    tui.Jobs{Total: s.Jobs.Total, ByState: s.Jobs.ByState},
		Prompts: s.promptDetails,
	}
	for _, r := range s.Repos {
		out.Repos = append(out.Repos, tui.Repo{Name: r.Name, Enabled: r.Enabled})
	}
	for _, a := range s.Agents {
		out.Agents = append(out.Agents, tui.Agent{Name: a.Name, Runtime: a.Runtime, Role: a.Role, Health: a.Health, TemplateID: a.templateID})
	}
	for _, sess := range s.RuntimeSessions {
		out.Sessions = append(out.Sessions, tui.Session{
			Name:     sess.Name,
			Runtime:  sess.Runtime,
			Repo:     sess.Repo,
			State:    sess.State,
			Type:     sess.sessionType,
			Role:     sess.role,
			Template: sess.templateID,
			LastUsed: sess.lastUsedAt,
			Expires:  sess.expiresAt,
		})
	}
	for _, w := range s.Worktrees {
		out.Worktrees = append(out.Worktrees, tui.Worktree{Task: w.Task, Repo: w.Repo, Path: w.Path})
	}
	for _, l := range s.BranchLocks {
		out.BranchLocks = append(out.BranchLocks, tui.BranchLock{Repo: l.Repo, Branch: l.Branch, Owner: l.Owner})
	}
	for _, t := range s.TrainSessions {
		out.Trains = append(out.Trains, tui.TrainSession{ID: t.ID, Phase: t.Phase, Candidate: t.Candidate, Repo: t.Repo})
	}
	for _, l := range s.ResourceLocks {
		out.ResourceLocks = append(out.ResourceLocks, tui.ResourceLock{Key: l.Key, Owner: l.Owner, Stale: l.Stale})
	}
	out.Config = s.configView
	jobs := make([]db.Job, 0, len(s.jobRows))
	for _, row := range s.jobRows {
		out.JobRows = append(out.JobRows, tui.JobRow{
			ID:          row.ID,
			Agent:       row.Agent,
			Type:        row.Type,
			State:       row.State,
			UpdatedAt:   row.UpdatedAt,
			LatestEvent: row.LatestEvent,
			Repo:        row.Repo,
		})
		jobs = append(jobs, row.Job)
	}
	out.Activity = buildDashboardActivity(jobs)
	return out
}

// buildDelegationTree maps a coordinator's child jobs into the detail view's
// delegation tree. Each child with a non-empty delegation id becomes a JobChild
// (action and deps read from the parent's settled result, keyed by delegation
// id); the lone child with an empty delegation id is the coordinator
// continuation job. DepsSatisfied is true when every dep's sibling child has
// reached a successful terminal state.
func buildDelegationTree(parent workflow.JobPayload, children []db.Job) ([]tui.JobChild, string, string) {
	// Delegation metadata (action, declared deps) lives on the parent result,
	// always available once the coordinator settles, so we stay independent of
	// the child payload shape.
	type delegationMeta struct {
		action string
		deps   []string
	}
	meta := map[string]delegationMeta{}
	if parent.Result != nil {
		for _, d := range parent.Result.Delegations {
			meta[d.ID] = delegationMeta{action: d.Action, deps: d.Deps}
		}
	}

	// Phase 2 retries create additional child rows that share a DelegationID
	// (only the job ID differs, ".../retry/<n>"). Collapse them by delegation id
	// keeping the latest attempt (highest RetryCount), mirroring the engine's
	// childDelegationJobs/delegationJobRetryCount latest-attempt logic, so the
	// DAG shows one node per delegation instead of a row per attempt.
	var (
		order             []string
		latest            = map[string]db.Job{}
		latestRetry       = map[string]int{}
		continuationID    string
		continuationState string
	)
	for _, child := range children {
		delegationID := strings.TrimSpace(child.DelegationID)
		if delegationID == "" {
			// The coordinator continuation job carries no delegation id.
			continuationID = child.ID
			continuationState = child.State
			continue
		}
		attempt := childRetryCount(child)
		if _, ok := latest[delegationID]; ok {
			if attempt < latestRetry[delegationID] {
				continue
			}
		} else {
			order = append(order, delegationID)
		}
		latest[delegationID] = child
		latestRetry[delegationID] = attempt
	}

	// Child state by delegation id, resolved from the latest attempt only (not
	// last-write-wins over ListJobsByParent's "delegation_id, id" ordering), so
	// DepsSatisfied reflects the current attempt's outcome.
	stateByDelegation := make(map[string]string, len(latest))
	for id, child := range latest {
		stateByDelegation[id] = child.State
	}

	out := make([]tui.JobChild, 0, len(order))
	for _, delegationID := range order {
		child := latest[delegationID]
		m := meta[delegationID]
		action := m.action
		if strings.TrimSpace(action) == "" {
			action = child.Type
		}
		out = append(out, tui.JobChild{
			ID:            child.ID,
			DelegationID:  delegationID,
			Agent:         child.Agent,
			Action:        action,
			State:         child.State,
			Deps:          m.deps,
			DepsSatisfied: delegationDepsSatisfied(m.deps, stateByDelegation),
		})
	}
	return out, continuationID, continuationState
}

// activityJobActive reports whether a job state counts as live (in-progress) work.
func activityJobActive(state string) bool {
	return state == "queued" || state == "running"
}

// buildDashboardActivity groups jobs into delegation trees and returns the
// originating roots that have queued/running work, newest first — the Activity
// page's live "what are agents working on" view. A root is an originating job
// (empty ParentJobID); its direct delegation children come from the same
// buildDelegationTree used by the job-detail view.
//
// Two invariants keep this cheap and consistent: (1) the active decision and the
// progress counts use the SAME scope — the root plus its direct delegation
// children (incl. the continuation) — so a surfaced tree always shows visible
// live work rather than a settled coordinator with "0 running" driven by a
// deeper descendant; (2) job payloads are parsed only for the few live roots and
// their direct children, never for every job on the refresh tick (matching the
// cheap-tick design of the Attention path).
//
// Scope is intentionally one level deep: the page renders direct delegations, so
// a tree whose root and direct children have all settled is not surfaced even if
// a deeper grandchild (a sub-coordinator's own delegation) is still running.
// Surfacing such a tree would show an all-settled, "0 running" coordinator with
// the live grandchild invisible. Rendering the full recursive tree is a separate,
// larger change.
func buildDashboardActivity(jobs []db.Job) []tui.ActivityRoot {
	jobByID := make(map[string]db.Job, len(jobs))
	childrenByParent := map[string][]db.Job{}
	var rootIDs []string
	for _, j := range jobs {
		jobByID[j.ID] = j
		if parent := strings.TrimSpace(j.ParentJobID); parent != "" {
			childrenByParent[parent] = append(childrenByParent[parent], j)
		} else {
			// An originating job (user-launched coordinator or standalone agent)
			// roots its own delegation tree.
			rootIDs = append(rootIDs, j.ID)
		}
	}

	var roots []tui.ActivityRoot
	// Sort key per root: the most recent update across the tree (root + direct
	// children + continuation). A coordinator freezes its own UpdatedAt once it
	// fans out, so its actively-running children must drive "newest first".
	latestByRoot := map[string]string{}
	for _, rootID := range rootIDs {
		rootJob := jobByID[rootID]
		directChildren := childrenByParent[rootID]
		// Decide activeness from raw job states (no payload parse) so a fully
		// settled tree costs nothing on the tick. Scope = root + direct children
		// (the continuation is a direct child), matching what the page renders.
		active := activityJobActive(rootJob.State)
		for _, c := range directChildren {
			if active {
				break
			}
			active = activityJobActive(c.State)
		}
		if !active {
			continue
		}
		// Only for the few live roots: parse the root payload (Repo + delegation
		// metadata) and, via buildDelegationTree, the direct children.
		payload, err := workflow.ParseJobPayload(rootJob.Payload)
		if err != nil {
			payload = workflow.JobPayload{}
		}
		children, contID, contState := buildDelegationTree(payload, directChildren)
		root := tui.ActivityRoot{
			JobID:             rootJob.ID,
			Agent:             rootJob.Agent,
			Action:            rootJob.Type,
			State:             rootJob.State,
			Repo:              strings.TrimSpace(payload.Repo),
			UpdatedAt:         rootJob.UpdatedAt,
			Children:          children,
			ContinuationID:    contID,
			ContinuationState: contState,
			Total:             len(children),
		}
		for _, c := range children {
			switch c.State {
			case "running":
				root.Running++
			case "queued":
				root.Queued++
			case "blocked":
				root.Blocked++
			case "succeeded", "failed", "cancelled":
				root.Done++
			}
		}
		latest := rootJob.UpdatedAt
		for _, c := range directChildren {
			if c.UpdatedAt > latest {
				latest = c.UpdatedAt
			}
		}
		latestByRoot[rootJob.ID] = latest
		roots = append(roots, root)
	}
	sort.SliceStable(roots, func(i, j int) bool {
		li, lj := latestByRoot[roots[i].JobID], latestByRoot[roots[j].JobID]
		if li != lj {
			return li > lj // newest activity first (ISO lexical)
		}
		return roots[i].JobID < roots[j].JobID
	})
	return roots
}

// childRetryCount reads a delegation child's RetryCount from its stored payload,
// returning 0 when the payload is missing or cannot be parsed. Mirrors the
// engine's delegationJobRetryCount so the dashboard dedups attempts identically.
func childRetryCount(child db.Job) int {
	payload, err := workflow.ParseJobPayload(child.Payload)
	if err != nil {
		return 0
	}
	return payload.RetryCount
}

// delegationDepsSatisfied reports whether every dep delegation has a sibling
// child in a successful terminal state (the TUI's success notion).
func delegationDepsSatisfied(deps []string, stateByDelegation map[string]string) bool {
	for _, dep := range deps {
		if stateByDelegation[strings.TrimSpace(dep)] != "succeeded" {
			return false
		}
	}
	return true
}
