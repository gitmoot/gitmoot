package proof

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// RenderTree writes the stable, human-readable graded tree used by the CLI.
func RenderTree(w io.Writer, manifest Manifest) error {
	if err := VerifyManifest(manifest); err != nil {
		return err
	}
	fmt.Fprintf(w, "proof %s\n", manifest.ProofID)
	fmt.Fprintf(w, "root %s [%s]\n", dash(manifest.Root.Ref), GradeVerified)

	sessions := directChildren(manifest, manifest.Root, KindSession)
	models := map[string]bool{}
	testCount, reviewCount := 0, 0
	approvedReviews := 0
	prLabels := map[string]bool{}
	for _, session := range sessions {
		attrs := session.Attrs
		model := dash(attrs["model"])
		if model != "-" {
			models[model] = true
		}
		fmt.Fprintf(w, "  session %s\n", dash(session.Ref))
		fmt.Fprintf(w, "    %s · %s · %s · tokens %s/%s · %s · ref %s\n",
			dash(attrs["agent"]), dash(attrs["runtime"]), model,
			dash(attrs["input_tokens"]), dash(attrs["output_tokens"]),
			dash(attrs["state"]), dash(attrs["runtime_ref"]))
		if attrs["payload_unparseable"] == "true" {
			fmt.Fprintln(w, "    payload: - (stored payload is not valid JSON)")
		}

		delegations := directChildren(manifest, session, KindDelegation)
		if len(delegations) == 0 {
			fmt.Fprintln(w, "    lineage: -")
		} else {
			fmt.Fprintln(w, "    lineage:")
			for _, delegation := range delegations {
				d := delegation.Attrs
				fmt.Fprintf(w, "      %s → %s · depth %s · synthesis %s · quorum %s · deps %s\n",
					dash(d["delegation_id"]), dash(d["child_job_id"]),
					dash(d["delegation_depth"]), dash(d["synthesis_rule"]),
					dash(zeroAsDash(d["quorum"])), dash(d["deps"]))
			}
		}

		commits := directChildren(manifest, session, KindCommit)
		if len(commits) == 0 {
			fmt.Fprintln(w, "    commits: -")
		} else {
			fmt.Fprintln(w, "    commits:")
			for _, commit := range commits {
				valid := dash(commit.Attrs["result_hash_valid"])
				fmt.Fprintf(w, "      %s · result_hash %s · integrity %s\n",
					dash(commit.Attrs["head_sha"]), dash(commit.Attrs["result_hash"]), valid)
			}
		}

		tests := directChildren(manifest, session, KindTest)
		testCount += len(tests)
		if len(tests) == 0 {
			fmt.Fprintln(w, "    tests: - (CI verification deferred)")
		} else {
			fmt.Fprintln(w, "    tests:")
			for _, test := range tests {
				fmt.Fprintf(w, "      %s [%s; CI verification deferred]\n", dash(test.Attrs["command"]), GradeReported)
			}
		}

		reviews := directChildren(manifest, session, KindReview)
		reviewCount += len(reviews)
		if len(reviews) == 0 {
			fmt.Fprintln(w, "    reviews: -")
		} else {
			fmt.Fprintln(w, "    reviews:")
			for _, review := range reviews {
				independence := "-"
				switch review.Attrs["independent"] {
				case "true":
					independence = "independent"
				case "false":
					independence = "self"
				}
				// The projection keeps attrs["decision"] at the RAW verdict and records
				// the folded outcome alongside it, so counting/printing the raw value
				// alone renders "0 approved" for a manifest that carries a review.approved
				// claim. Count the EFFECTIVE decision and show both: the reader sees the
				// verdict the reviewer wrote and the outcome gitmoot acted on.
				decision := dash(review.Attrs["decision"])
				effective := strings.TrimSpace(review.Attrs["effective_decision"])
				outcome := strings.TrimSpace(review.Attrs["review_outcome"])
				switch {
				case effective == NoVerdictDecision:
					// A coordinator fan-out announced a panel. It is not a verdict, so it
					// is neither counted nor allowed to READ as one at a glance — the
					// classification leads and the announced decision trails it.
					decision = fmt.Sprintf("no verdict · %s · announced %s", dash(outcome), dash(review.Attrs["decision"]))
				case effective != "":
					if outcome != "" {
						decision = fmt.Sprintf("%s (%s)", decision, outcome)
					}
					if effective == "approved" {
						approvedReviews++
					}
				default:
					if review.Attrs["decision"] == "approved" {
						approvedReviews++
					}
				}
				fmt.Fprintf(w, "      %s · %s · findings %s · %s [%s]\n",
					dash(review.Attrs["agent"]), decision,
					dash(review.Attrs["findings_count"]), independence, reviewClaimGrade(review))
			}
		}

		prs := directChildren(manifest, session, KindPR)
		if len(prs) == 0 {
			fmt.Fprintln(w, "    PR: -")
		} else {
			fmt.Fprintln(w, "    PR:")
			for _, pr := range prs {
				label := "#" + dash(pr.Attrs["number"])
				prLabels[label] = true
				fmt.Fprintf(w, "      %s · head %s · opened %s · merged %s · state %s\n",
					label, dash(pr.Attrs["head_sha"]), dash(pr.Attrs["opened_at"]),
					dash(pr.Attrs["merged_at"]), dash(pr.Attrs["state"]))
			}
		}
	}
	artifacts, err := ArtifactEntries(manifest)
	if err != nil {
		return err
	}
	if len(artifacts) > 0 {
		fmt.Fprintln(w, "artifacts:")
		for _, artifact := range artifacts {
			fmt.Fprintf(w, "  %s · %d bytes · sha256:%s [verified integrity]\n",
				artifact.Relpath, artifact.Size, artifact.SHA256)
		}
	}

	modelCount := len(models)
	fmt.Fprintf(w, "summary: root %s · %d sessions / %d models · %d tests reported, CI verification deferred · %d reviews / %d approved · PR %s\n",
		dash(manifest.Root.Ref), len(sessions), modelCount, testCount,
		reviewCount, approvedReviews, joinSet(prLabels))
	tally := GradeTally(manifest)
	fmt.Fprintf(w, "grades: reported=%d · observed=%d · verified=%d\n",
		tally[GradeReported], tally[GradeObserved], tally[GradeVerified])
	nodes, resultHashes := integrityTally(manifest)
	artifactCount, artifactBytes := artifactIntegrityTally(manifest)
	delegationDAG := "gap"
	if manifest.Root.Attrs["dag_consistent"] == "true" {
		delegationDAG = "consistent"
	}
	fmt.Fprintf(w, "integrity: all %d nodes hash-consistent · manifest DAG acyclic · delegation DAG %s · %d result_hashes matched · %d artifact sha256 digests matched / %d bytes\n",
		nodes, delegationDAG, resultHashes, artifactCount, artifactBytes)
	return nil
}

func artifactIntegrityTally(manifest Manifest) (count int, bytes int64) {
	entries, err := ArtifactEntries(manifest)
	if err != nil {
		return 0, 0
	}
	for _, entry := range entries {
		bytes += entry.Size
	}
	return len(entries), bytes
}

// GradeTally counts substantive claims by grade across nodes reachable from the
// proof root. integrity.* meta-claims are reported separately by RenderTree.
func GradeTally(manifest Manifest) map[Grade]int {
	tally := map[Grade]int{
		GradeReported: 0,
		GradeObserved: 0,
		GradeVerified: 0,
	}
	reachable, err := reachableNodeIDs(manifest)
	if err != nil {
		return tally
	}
	for id := range reachable {
		node := manifest.Nodes[id]
		for _, claim := range node.Claims {
			if strings.HasPrefix(claim.Type, "integrity.") {
				continue
			}
			tally[claim.Grade]++
		}
	}
	return tally
}

func integrityTally(manifest Manifest) (nodes, resultHashes int) {
	reachable, err := reachableNodeIDs(manifest)
	if err != nil {
		return 0, 0
	}
	for id := range reachable {
		for _, claim := range manifest.Nodes[id].Claims {
			if claim.Type == "integrity.result_hash" && claim.Grade == GradeVerified {
				resultHashes++
			}
		}
	}
	return len(reachable), resultHashes
}

func directChildren(manifest Manifest, parent Node, kind Kind) []Node {
	children := make([]Node, 0)
	seen := map[string]bool{}
	for _, id := range parent.Children {
		node, ok := manifest.Nodes[id]
		if !ok || node.Kind != kind || seen[id] {
			continue
		}
		seen[id] = true
		children = append(children, node)
	}
	sort.Slice(children, func(i, j int) bool {
		if children[i].Ref != children[j].Ref {
			return children[i].Ref < children[j].Ref
		}
		return children[i].ID < children[j].ID
	})
	return children
}

func reviewClaimGrade(node Node) Grade {
	grade := GradeReported
	for _, claim := range node.Claims {
		if strings.HasPrefix(claim.Type, "review.") && claim.Grade == GradeObserved {
			grade = GradeObserved
		}
	}
	return grade
}

func zeroAsDash(value string) string {
	if n, err := strconv.Atoi(value); err == nil && n == 0 {
		return ""
	}
	return value
}

func joinSet(values map[string]bool) string {
	if len(values) == 0 {
		return "-"
	}
	items := make([]string, 0, len(values))
	for value := range values {
		items = append(items, value)
	}
	sort.Strings(items)
	return strings.Join(items, ",")
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}
