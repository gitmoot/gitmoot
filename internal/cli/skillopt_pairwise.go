package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// Live-pairwise ingestion (#77b) source tags. They are DISTINCT from the
// single-prompt Mode B (#473) tags (skillopt-ab / preference_ab) so validation-set
// live-pairwise feedback is trivially separable from single-prompt A/B in the
// canonical ranked_feedback_events store and in the downstream export/optimizer.
const (
	skillOptPairwiseSource          = "live-pairwise"
	skillOptPairwiseFeedbackSource  = "pairwise_valset"
	skillOptPairwiseRunIDPrefix     = "skillopt-pairwise:"
	skillOptPairwiseDefaultReviewer = "human"

	// Conventional filenames #77a writes into the packet/artifact dir
	// (gitmoot_skillopt/pairwise.py:write_pairwise_artifacts). The reviewer's
	// picks file is added at review time.
	pairwisePacketFileName    = "pairwise-review.json"
	pairwiseSecretMapFileName = "pairwise-secret-map.json"
	pairwisePicksFileName     = "pairwise-picks.json"
)

func runSkillOptPairwise(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptPairwiseUsage(stdout)
		return 0
	}
	switch args[0] {
	case "import":
		return runSkillOptPairwiseImport(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt pairwise command %q\n\n", args[0])
		printSkillOptPairwiseUsage(stderr)
		return 2
	}
}

func printSkillOptPairwiseUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt pairwise import <packet-dir> [--packet path] [--secret-map path] [--picks path] [--reviewer name] [--home path] [--json]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Ingest a REVIEWED #77a blinded live-pairwise packet on review close: the")
	fmt.Fprintln(w, "per-item anonymized A/B options, the reviewer's per-item pick, and the SEPARATE")
	fmt.Fprintln(w, "secret map. Each pick is UNBLINDED back to champion vs challenger via the secret")
	fmt.Fprintln(w, "map ONLY, then written as a canonical RankedFeedbackEvent through the Mode B")
	fmt.Fprintln(w, "recording path with source=live-pairwise (feedback_source=pairwise_valset).")
	fmt.Fprintln(w, "Ingestion writes feedback ONLY: it NEVER promotes or auto-promotes. Re-importing")
	fmt.Fprintln(w, "the same reviewed packet is idempotent (stable per-item conflict key). A")
	fmt.Fprintln(w, "missing/garbage secret entry or missing pick is reported per item without")
	fmt.Fprintln(w, "aborting the rest of the import.")
}

type skillOptPairwiseImportOptions struct {
	home      string
	packet    string
	secretMap string
	picks     string
	reviewer  string
	asJSON    bool
}

func runSkillOptPairwiseImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt pairwise import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	packet := fs.String("packet", "", "blinded review packet JSON (defaults to <packet-dir>/"+pairwisePacketFileName+")")
	secretMap := fs.String("secret-map", "", "secret unblinding map JSON (defaults to <packet-dir>/"+pairwiseSecretMapFileName+")")
	picks := fs.String("picks", "", "reviewer picks JSON (defaults to <packet-dir>/"+pairwisePicksFileName+")")
	reviewer := fs.String("reviewer", skillOptPairwiseDefaultReviewer, "reviewer id recorded on each feedback event")
	asJSON := fs.Bool("json", false, "emit a JSON summary instead of human-readable lines")
	// Separate the leading packet-dir positional from flags. flag.Parse stops at
	// the first non-flag, so collect positionals manually to allow the packet-dir
	// to appear before or after flags.
	positionals := []string{}
	rest := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			if !strings.Contains(arg, "=") && (arg == "--home" || arg == "--packet" || arg == "--secret-map" || arg == "--picks" || arg == "--reviewer") && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	if err := fs.Parse(rest); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	options := skillOptPairwiseImportOptions{
		home:      strings.TrimSpace(*home),
		packet:    strings.TrimSpace(*packet),
		secretMap: strings.TrimSpace(*secretMap),
		picks:     strings.TrimSpace(*picks),
		reviewer:  strings.TrimSpace(*reviewer),
		asJSON:    *asJSON,
	}
	if options.reviewer == "" {
		options.reviewer = skillOptPairwiseDefaultReviewer
	}

	// Resolve the three artifacts: a positional packet-dir supplies defaults, and
	// the explicit flags override any of them.
	if len(positionals) > 1 {
		fmt.Fprintln(stderr, "skillopt pairwise import accepts at most one packet-dir positional")
		return 2
	}
	packetDir := ""
	if len(positionals) == 1 {
		packetDir = strings.TrimSpace(positionals[0])
	}
	if options.packet == "" {
		if packetDir == "" {
			fmt.Fprintln(stderr, "skillopt pairwise import requires a packet-dir or --packet")
			return 2
		}
		options.packet = filepath.Join(packetDir, pairwisePacketFileName)
	}
	if options.secretMap == "" {
		if packetDir == "" {
			fmt.Fprintln(stderr, "skillopt pairwise import requires a packet-dir or --secret-map")
			return 2
		}
		options.secretMap = filepath.Join(packetDir, pairwiseSecretMapFileName)
	}
	if options.picks == "" {
		if packetDir == "" {
			fmt.Fprintln(stderr, "skillopt pairwise import requires a packet-dir or --picks")
			return 2
		}
		options.picks = filepath.Join(packetDir, pairwisePicksFileName)
	}

	packetData, err := os.ReadFile(options.packet)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt pairwise import: read packet: %v\n", err)
		return 1
	}
	secretData, err := os.ReadFile(options.secretMap)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt pairwise import: read secret map: %v\n", err)
		return 1
	}
	picksData, err := os.ReadFile(options.picks)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt pairwise import: read picks: %v\n", err)
		return 1
	}

	reviewPacket, err := skillopt.ParsePairwiseReviewPacket(packetData)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt pairwise import: %v\n", err)
		return 1
	}
	secret, err := skillopt.ParsePairwiseSecretMap(secretData)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt pairwise import: %v\n", err)
		return 1
	}
	pairwisePicks, err := skillopt.ParsePairwisePicks(picksData)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt pairwise import: %v\n", err)
		return 1
	}
	// The secret map and packet MUST describe the same run; mixing a packet with a
	// foreign secret map would unblind against the wrong mapping.
	if strings.TrimSpace(secret.RunID) != "" && strings.TrimSpace(secret.RunID) != strings.TrimSpace(reviewPacket.RunID) {
		fmt.Fprintf(stderr, "skillopt pairwise import: secret map run_id %q does not match packet run_id %q\n", secret.RunID, reviewPacket.RunID)
		return 1
	}
	// The picks are the artifact that decides each winner, so they MUST also describe
	// the same run as the packet; a foreign picks file whose items happen to share
	// generic ids (item-1, …) would otherwise be joined by item_id alone and silently
	// unblind the WRONG reviewer preferences for THIS run. Picks supplied as a bare
	// map shape carry no run_id (RunID stays empty) and therefore skip this check.
	if strings.TrimSpace(pairwisePicks.RunID) != "" && strings.TrimSpace(pairwisePicks.RunID) != strings.TrimSpace(reviewPacket.RunID) {
		fmt.Fprintf(stderr, "skillopt pairwise import: picks run_id %q does not match packet run_id %q\n", pairwisePicks.RunID, reviewPacket.RunID)
		return 1
	}

	exit := 0
	if err := withStoreAndPaths(options.home, func(paths config.Paths, store *db.Store) error {
		exit = importPairwisePacket(context.Background(), store, paths, reviewPacket, secret, pairwisePicks, options, stdout, stderr)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt pairwise import: %v\n", err)
		return 1
	}
	return exit
}

type pairwiseImportItemResult struct {
	ItemID  string `json:"item_id"`
	Status  string `json:"status"` // imported | skipped
	Winner  string `json:"winner,omitempty"`
	Pick    string `json:"pick,omitempty"`
	Message string `json:"message,omitempty"`
}

type pairwiseImportSummary struct {
	RunID    string                     `json:"run_id"`
	Source   string                     `json:"source"`
	Reviewer string                     `json:"reviewer"`
	Imported int                        `json:"imported"`
	Skipped  int                        `json:"skipped"`
	Items    []pairwiseImportItemResult `json:"items"`
}

// importPairwisePacket unblinds every item and writes the resolved preference via
// the Mode B recording path with the distinct live-pairwise source. It is
// fail-safe per item: a missing/garbage secret entry, a missing pick, or a write
// failure for one item is reported in that item's result and does NOT abort the
// rest of the import. It NEVER promotes — only canonical feedback rows are
// written. The exit code is 0 when every item imported, 1 when any item was
// skipped (so an automated caller can detect a partial import) while still having
// persisted every good item.
func importPairwisePacket(ctx context.Context, store *db.Store, paths config.Paths, packet skillopt.PairwiseReviewPacket, secret skillopt.PairwiseSecretMap, picks skillopt.PairwisePicks, options skillOptPairwiseImportOptions, stdout, stderr io.Writer) int {
	runID := skillOptPairwiseRunIDPrefix + strings.TrimSpace(packet.RunID)
	templateID := strings.TrimSpace(packet.TemplateID)
	championVersionID := strings.TrimSpace(packet.BaseVersionID)
	// The candidate (challenger) is supplied to #77a as content, not necessarily a
	// registered version; the secret map's candidate content hash is the stable
	// challenger identifier for the run metadata. It is metadata only and is never
	// used for the unblind.
	challengerVersionID := firstNonEmpty(strings.TrimSpace(secret.CandidateContentHash), strings.TrimSpace(packet.RunID)+":candidate")

	champion := skillOptABVariant{version: db.AgentTemplateVersion{ID: championVersionID}, label: skillOptABChampionLabel}
	challenger := skillOptABVariant{version: db.AgentTemplateVersion{ID: challengerVersionID}, label: skillOptABChallengerLabel}

	summary := pairwiseImportSummary{
		RunID:    runID,
		Source:   skillOptPairwiseSource,
		Reviewer: options.reviewer,
	}

	for _, result := range skillopt.UnblindPairwisePacket(packet, secret, picks) {
		itemResult := pairwiseImportItemResult{ItemID: result.ItemID, Pick: result.Pick}
		if result.Err != nil {
			itemResult.Status = "skipped"
			itemResult.Message = result.Err.Error()
			summary.Skipped++
			summary.Items = append(summary.Items, itemResult)
			continue
		}

		championDelivery := skillOptABDelivery{label: skillOptABChampionLabel, answer: result.ChampionResponse}
		challengerDelivery := skillOptABDelivery{label: skillOptABChallengerLabel, answer: result.ChallengerResponse}
		title := firstNonEmpty(result.Title, result.Prompt, result.ItemID)

		// Reuse the Mode B run-row scaffold (eval_run + per-item eval_review_item +
		// champion/challenger eval_review_options backed by the answer blobs), tagged
		// with the distinct live-pairwise source/feedback_source.
		if err := ensureSkillOptABRunRows(ctx, store, paths, runID, result.ItemID, templateID, champion, challenger, championDelivery, challengerDelivery, title, skillOptPairwiseSource, skillOptPairwiseFeedbackSource); err != nil {
			itemResult.Status = "skipped"
			itemResult.Message = fmt.Sprintf("ensure run rows: %v", err)
			summary.Skipped++
			summary.Items = append(summary.Items, itemResult)
			continue
		}

		// A STABLE per-item source_url makes re-import idempotent: the
		// (run_id,item_id,reviewer,source,source_url) conflict key is identical on a
		// re-import of the same reviewed packet, so the ranked event is upserted in
		// place and never double-counted.
		sourceURL := skillOptPairwiseSourceURL(packet.RunID, result.ItemID)
		if err := upsertSkillOptABRankedEvent(ctx, store, runID, result.ItemID, result.WinnerLabel, result.LoserLabel, options.reviewer, skillOptPairwiseSource, sourceURL); err != nil {
			itemResult.Status = "skipped"
			itemResult.Message = fmt.Sprintf("record pick: %v", err)
			summary.Skipped++
			summary.Items = append(summary.Items, itemResult)
			continue
		}

		itemResult.Status = "imported"
		itemResult.Winner = result.WinnerLabel
		summary.Imported++
		summary.Items = append(summary.Items, itemResult)
	}

	if options.asJSON {
		encoded, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "skillopt pairwise import: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
	} else {
		for _, item := range summary.Items {
			if item.Status == "imported" {
				fmt.Fprintf(stdout, "imported %s: %s (pick %s)\n", item.ItemID, item.Winner, item.Pick)
			} else {
				fmt.Fprintf(stdout, "skipped %s: %s\n", item.ItemID, item.Message)
			}
		}
		fmt.Fprintf(stdout, "pairwise import complete: run %s, %d imported, %d skipped (source=%s)\n", runID, summary.Imported, summary.Skipped, skillOptPairwiseSource)
	}

	if summary.Skipped > 0 {
		return 1
	}
	return 0
}

// skillOptPairwiseSourceURL mints the STABLE per-item SourceURL for the
// live-pairwise RankedFeedbackEvent. Unlike Mode B's monotonic per-pick token (a
// single prompt can be A/B'd repeatedly), one reviewed pairwise packet has exactly
// one preference per item, so the source_url is deterministic from (run_id,
// item_id). That determinism is precisely what makes re-importing the same packet
// idempotent: the conflict key resolves to the same row.
func skillOptPairwiseSourceURL(runID, itemID string) string {
	return fmt.Sprintf("%s%s:%s", skillOptPairwiseRunIDPrefix, strings.TrimSpace(runID), strings.TrimSpace(itemID))
}
