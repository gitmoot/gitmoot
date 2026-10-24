package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func runJobCleanup(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printJobCleanupUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runJobCleanupList(args[1:], stdout, stderr)
	case "reopen":
		return runJobCleanupReopen(args[1:], stdout, stderr)
	case "inspect", "apply":
		return runJobCleanupResource(args[0], args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown job cleanup command %q\n\n", args[0])
		printJobCleanupUsage(stderr)
		return 2
	}
}

func printJobCleanupUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot job cleanup list [--state pending|retryable|removed|quarantined] [--json]")
	fmt.Fprintln(w, "  gitmoot job cleanup reopen <resource-id>")
	fmt.Fprintln(w, "  gitmoot job cleanup inspect [--json] <job-id>")
	fmt.Fprintln(w, "  gitmoot job cleanup apply --resource <resource-id> --token <eligibility-token> [--json] <job-id>")
}

func runJobCleanupList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job cleanup list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	state := fs.String("state", "", "cleanup state filter")
	jsonOutput := fs.Bool("json", false, "print cleanup obligations as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "job cleanup list does not accept positional arguments")
		return 2
	}
	if value := strings.TrimSpace(*state); value != "" && value != db.CleanupObligationPending && value != db.CleanupObligationRetryable && value != db.CleanupObligationRemoved && value != db.CleanupObligationQuarantined {
		fmt.Fprintf(stderr, "job cleanup list: invalid state %q\n", value)
		return 2
	}
	var obligations []db.CleanupObligation
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		obligations, err = store.ListCleanupObligations(context.Background(), *state)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job cleanup list: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if obligations == nil {
			obligations = []db.CleanupObligation{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(obligations); err != nil {
			fmt.Fprintf(stderr, "job cleanup list: %v\n", err)
			return 1
		}
		return 0
	}
	for _, obligation := range obligations {
		fmt.Fprintf(stdout, "%s  %-11s attempts=%d job=%s path=%s reason=%s next=%s\n",
			obligation.ResourceID, obligation.State, obligation.AttemptCount,
			obligation.OwnerJobID, obligation.ExpectedPath, obligation.Reason,
			obligation.NextAttemptAt)
	}
	return 0
}

func runJobCleanupReopen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job cleanup reopen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		fmt.Fprintln(stderr, "job cleanup reopen requires exactly one resource id")
		return 2
	}
	resourceID := strings.TrimSpace(fs.Arg(0))
	var obligation db.CleanupObligation
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		obligation, err = store.ReopenCleanupObligation(context.Background(), resourceID, time.Now().UTC())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job cleanup reopen: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "reopened cleanup obligation %s for job %s\n", obligation.ResourceID, obligation.OwnerJobID)
	return 0
}

func runJobCleanupResource(operation string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job cleanup "+operation, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the versioned producer cleanup decision")
	resourceID := fs.String("resource", "", "producer-derived resource identity from inspect")
	token := fs.String("token", "", "eligibility token from inspect")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		fmt.Fprintln(stderr, "job cleanup "+operation+" requires exactly one job id")
		return 2
	}
	if operation == "apply" && (strings.TrimSpace(*resourceID) == "" || strings.TrimSpace(*token) == "") {
		fmt.Fprintln(stderr, "job cleanup apply requires --resource and --token from inspect")
		return 2
	}
	if operation == "inspect" && (*resourceID != "" || *token != "") {
		fmt.Fprintln(stderr, "job cleanup inspect derives the resource; --resource and --token are apply-only")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "job cleanup %s: %v\n", operation, err)
		return 1
	}
	ctx := context.Background()
	var decision workflow.CleanupInspection
	var result workflow.CleanupApplyResult
	withCleanupStore := withReadOnlyStore
	if operation == "apply" {
		withCleanupStore = withStore
	}
	err = withCleanupStore(*home, func(store *db.Store) error {
		job, err := store.GetJob(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil {
			return err
		}
		repo, err := store.GetRepo(ctx, payload.Repo)
		if err != nil {
			return err
		}
		engine := daemonWorkflowEngine(store, nil, repo.CheckoutPath, paths.Home)
		if operation == "inspect" {
			decision, err = engine.InspectProducerCleanup(ctx, job.ID, workflow.CleanupPolicy{})
			return err
		}
		result, err = engine.ApplyProducerCleanup(ctx, job.ID, *resourceID, *token, workflow.CleanupPolicy{})
		decision = result.Decision
		return err
	})
	if *jsonOutput {
		var value any = decision
		if operation == "apply" {
			value = result
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if encodeErr := encoder.Encode(value); encodeErr != nil {
			err = errors.Join(err, encodeErr)
		}
	} else if operation == "inspect" {
		fmt.Fprintf(stdout, "resource=%s job=%s generation=%d eligible=%t reasons=%s token=%s\n",
			decision.ResourceID, decision.OwnerJobID, decision.LifecycleGeneration,
			decision.Eligible, strings.Join(decision.ReasonCodes, ","), decision.EligibilityToken)
	} else {
		fmt.Fprintf(stdout, "state=%s resource=%s reasons=%s\n", result.State, *resourceID, strings.Join(decision.ReasonCodes, ","))
		if result.Receipt != nil {
			fmt.Fprintf(stdout, "receipt=%s generation=%d removed_at=%s\n",
				result.Receipt.ReceiptID, result.Receipt.LifecycleGeneration, result.Receipt.RemovedAt)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "job cleanup %s: %v\n", operation, err)
		return 1
	}
	if operation == "apply" && result.State != "removed" && result.State != "already_removed" {
		return 3
	}
	return 0
}
