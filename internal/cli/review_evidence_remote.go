package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// remoteReviewEvidenceDir is where a remote review finds its pinned private
// evidence (#2476). It lives under the instance's runtime material, so it is
// destroyed with the sandbox and never touches the host or the workspace.
const remoteReviewEvidenceDir = execbackend.RuntimeMaterialDir + "/evidence/attachments"

const (
	reviewEvidenceVerifiedEventKind = "review_evidence_verified"
	reviewEvidenceAlteredEventKind  = "review_evidence_altered"
)

func remoteReviewEvidencePath(file workflow.ReviewEvidenceFile) string {
	return remoteReviewEvidenceDir + "/" + file.SandboxName
}

// reviewEvidenceReadback is one file's state as measured INSIDE the sandbox.
type reviewEvidenceReadback struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Match  bool   `json:"match"`
}

type reviewEvidenceReceipt struct {
	Phase               string                   `json:"phase"`
	JobID               string                   `json:"job_id"`
	LifecycleGeneration int64                    `json:"lifecycle_generation"`
	InstanceID          string                   `json:"instance_id"`
	HeadSHA             string                   `json:"head_sha"`
	ManifestSHA256      string                   `json:"manifest_sha256"`
	Files               []reviewEvidenceReadback `json:"files"`
}

// installRemoteReviewEvidence uploads each pinned file into the instance and
// proves the delivery by reading the bytes back inside the sandbox. A file that
// drifted on the host since the request, or that reads back differently, fails
// provisioning: the reviewer never starts on evidence nobody pinned.
func (w jobWorker) installRemoteReviewEvidence(ctx context.Context, lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance, job db.Job) ([]string, error) {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return nil, err
	}
	evidence := payload.ReviewEvidence
	if evidence == nil || len(evidence.Files) == 0 {
		return nil, nil
	}
	if !strings.EqualFold(evidence.HeadSHA, strings.TrimSpace(payload.HeadSHA)) {
		return nil, fmt.Errorf("review evidence is pinned to head %s, but the job reviews %s", evidence.HeadSHA, payload.HeadSHA)
	}
	// Second gate after every dispatch-time rewrite: evidence goes only to E2B.
	if !reviewEvidenceProviderAllowed(payload.ExecProvider) {
		return nil, fmt.Errorf("review evidence is delivered only to E2B reviews; this job targets provider %q", payload.ExecProvider)
	}
	installer, ok := lifecycle.(execbackend.InstanceFileInstaller)
	if !ok {
		return nil, fmt.Errorf("execution backend %q cannot install review evidence", lifecycle.Name())
	}
	for _, file := range evidence.Files {
		body, err := file.VerifiedBytes()
		if err != nil {
			return nil, fmt.Errorf("review evidence refused: %w", err)
		}
		if _, err := installer.InstallInstanceFile(ctx, instance, remoteReviewEvidencePath(file), bytes.NewReader(body), 0o600); err != nil {
			return nil, fmt.Errorf("install review evidence %s: %w", file.SandboxName, err)
		}
	}
	runner := execbackend.InstanceRunner{Backend: lifecycle, Instance: instance}
	paths := remoteReviewEvidencePaths(evidence)
	// Read-only for the reviewer by default. Not a guarantee on its own (the
	// sandbox user owns the files), which is why the bytes are re-verified after
	// every delivery turn as well.
	if _, err := runner.Run(ctx, instance.Workspace, "chmod", append([]string{"0400", "--"}, paths...)...); err != nil {
		return nil, fmt.Errorf("protect review evidence in the sandbox: %w", err)
	}
	if err := w.verifyRemoteReviewEvidence(ctx, runner, instance, job.ID, evidence, "delivered"); err != nil {
		return nil, err
	}
	return []string{"GITMOOT_REVIEW_EVIDENCE_DIR=" + remoteReviewEvidenceDir}, nil
}

func remoteReviewEvidencePaths(evidence *workflow.ReviewEvidence) []string {
	paths := make([]string, 0, len(evidence.Files))
	for _, file := range evidence.Files {
		paths = append(paths, remoteReviewEvidencePath(file))
	}
	return paths
}

// verifyRemoteReviewEvidence measures every evidence file inside the sandbox
// and compares it with the pin. Size and hash come from ONE read of each file
// (`wc -c` and `sha256sum` over the same bytes via a single shell pass per
// file), so a receipt never pairs a size and a hash from two different
// snapshots. It always records a receipt event; any missing, resized or
// rehashed file is an error.
func (w jobWorker) verifyRemoteReviewEvidence(ctx context.Context, runner execbackend.InstanceRunner, instance *execbackend.Instance, jobID string, evidence *workflow.ReviewEvidence, phase string) error {
	paths := remoteReviewEvidencePaths(evidence)
	// For each file: copy it once to a private temp file, then measure that
	// copy. Later writes to the evidence file cannot change what was measured.
	const measure = `set -e; t=$(mktemp); trap 'rm -f "$t"' EXIT
for f in "$@"; do cat -- "$f" > "$t"; printf '%s %s %s\n' "$(wc -c < "$t" | tr -d ' ')" "$(sha256sum < "$t" | cut -d' ' -f1)" "${f##*/}"; done`
	result, measureErr := runner.Run(ctx, instance.Workspace, "sh", append([]string{"-c", measure, "measure"}, paths...)...)
	measured := parseReviewEvidenceMeasurements(result.Stdout)
	receipt := reviewEvidenceReceipt{
		Phase: phase, JobID: jobID, LifecycleGeneration: instance.LifecycleGeneration, InstanceID: instance.ID,
		HeadSHA: evidence.HeadSHA, ManifestSHA256: evidence.ManifestSHA256,
	}
	var mismatched []string
	for _, file := range evidence.Files {
		readback := measured[file.SandboxName]
		readback.Name = file.SandboxName
		readback.Match = readback.SHA256 == file.SHA256 && readback.Bytes == file.Bytes
		if !readback.Match {
			mismatched = append(mismatched, file.SandboxName)
		}
		receipt.Files = append(receipt.Files, readback)
	}
	kind := reviewEvidenceVerifiedEventKind
	var failure error
	switch {
	case measureErr != nil:
		kind = reviewEvidenceAlteredEventKind
		failure = fmt.Errorf("review evidence could not be read back in the sandbox (%s): %w", phase, measureErr)
	case len(mismatched) > 0:
		kind = reviewEvidenceAlteredEventKind
		failure = fmt.Errorf("review evidence differs from its pin in the sandbox (%s): %s", phase, strings.Join(mismatched, ", "))
	}
	w.recordReviewEvidenceReceipt(ctx, jobID, kind, receipt)
	return failure
}

// reviewEvidenceProviderAllowed reports whether a review's execution provider
// may receive private evidence: E2B only ("" is the E2B default).
func reviewEvidenceProviderAllowed(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return provider == "" || provider == "e2b"
}

func (w jobWorker) recordReviewEvidenceReceipt(ctx context.Context, jobID, kind string, receipt reviewEvidenceReceipt) {
	if w.Store == nil {
		return
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		body = []byte(err.Error())
	}
	eventCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := w.Store.AddJobEvent(eventCtx, db.JobEvent{JobID: jobID, Kind: kind, Message: string(body)}); err != nil {
		writeLine(w.Stdout, "job %s %s event failed: %v", jobID, kind, err)
	}
}

// parseReviewEvidenceMeasurements reads "<bytes> <sha256> <name>" lines from
// the single-snapshot measure script. Sandbox names are unique and contain no
// spaces. A missing or malformed line leaves a zero measurement, which can
// never equal a pin, so verification fails closed.
func parseReviewEvidenceMeasurements(output string) map[string]reviewEvidenceReadback {
	values := map[string]reviewEvidenceReadback{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		size, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		values[fields[2]] = reviewEvidenceReadback{Bytes: size, SHA256: strings.ToLower(fields[1])}
	}
	return values
}

// reviewEvidenceGuardAdapter re-verifies the evidence inside the sandbox after
// every delivery turn. A turn that returns while the files no longer match
// their pin fails, so a verdict can never rest on evidence that was changed
// under it (#2476: change detection with rejection).
type reviewEvidenceGuardAdapter struct {
	inner  workflow.DeliveryAdapter
	verify func(context.Context) error
}

func (a reviewEvidenceGuardAdapter) Deliver(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	result, err := a.inner.Deliver(ctx, agent, job)
	if err != nil {
		return result, err
	}
	if verifyErr := a.verify(ctx); verifyErr != nil {
		return runtime.Result{}, verifyErr
	}
	return result, nil
}

// guardRemoteReviewEvidence wraps adapter when this run delivered evidence.
func (w jobWorker) guardRemoteReviewEvidence(adapter workflow.DeliveryAdapter, lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance, jobID string, payload workflow.JobPayload) workflow.DeliveryAdapter {
	evidence := payload.ReviewEvidence
	if evidence == nil || len(evidence.Files) == 0 || lifecycle == nil || instance == nil {
		return adapter
	}
	runner := execbackend.InstanceRunner{Backend: lifecycle, Instance: instance}
	return reviewEvidenceGuardAdapter{inner: adapter, verify: func(ctx context.Context) error {
		return w.verifyRemoteReviewEvidence(ctx, runner, instance, jobID, evidence, "after_delivery")
	}}
}

// reviewEvidenceInstructions tells the reviewer where the pinned files are and
// what it must do with them. Host paths are deliberately omitted.
func reviewEvidenceInstructions(evidence *workflow.ReviewEvidence) string {
	if evidence == nil || len(evidence.Files) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PRIVATE REVIEW EVIDENCE (#2476): %d hash-pinned files for head %s are in %s (also $GITMOOT_REVIEW_EVIDENCE_DIR). ", len(evidence.Files), evidence.HeadSHA, remoteReviewEvidenceDir)
	b.WriteString("Gitmoot verified each file's size and sha256 inside this sandbox before you started and re-verifies them after you finish; do not modify them. Open every image and read every log you rely on, and name in your verdict which files you inspected.\n")
	for _, file := range evidence.Files {
		fmt.Fprintf(&b, "- %s (%d bytes, sha256 %s)\n", remoteReviewEvidencePath(file), file.Bytes, file.SHA256)
	}
	return strings.TrimRight(b.String(), "\n")
}
