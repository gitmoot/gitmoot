package proof

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// PRReceipt is structured store evidence about a pull request and its daemon
// workflow-journal receipts. Empty fields are honest evidence gaps.
type PRReceipt struct {
	Repo           string
	Number         int
	HeadSHA        string
	MergeCommitSHA string
	State          string
	OpenedAt       string
	MergedAt       string
}

type payloadProjection struct {
	Repo               string                `json:"repo"`
	PullRequest        int                   `json:"pull_request"`
	HeadSHA            string                `json:"head_sha"`
	WorkflowID         string                `json:"workflow_id"`
	RuntimeOverride    string                `json:"runtime_override"`
	RuntimeOverrideRef string                `json:"runtime_override_ref"`
	Result             *workflow.AgentResult `json:"result"`
}

type projector struct {
	root        db.Job
	jobs        map[string]db.Job
	jobIDs      []string
	results     map[string]*workflow.AgentResult
	receipts    map[string]PRReceipt
	events      map[string][]db.JobEvent
	children    map[string][]db.Job
	builder     *builder
	sessions    map[string]string
	visiting    map[string]bool
	dagOK       bool
	dagGaps     []string
	rootAsOf    string
	implementer map[string]db.Job
}

// Project builds a deterministic Merkle DAG from already-loaded structured
// store records. It performs no I/O and uses only stored timestamps.
func Project(root db.Job, jobs []db.Job, results map[string]*workflow.AgentResult, receipts []PRReceipt, events map[string][]db.JobEvent) Manifest {
	p := &projector{
		root: root, jobs: make(map[string]db.Job), results: results,
		receipts: make(map[string]PRReceipt), events: events,
		children: make(map[string][]db.Job), builder: newBuilder(),
		sessions: make(map[string]string), visiting: make(map[string]bool),
		implementer: make(map[string]db.Job),
	}
	if p.results == nil {
		p.results = map[string]*workflow.AgentResult{}
	}
	if p.events == nil {
		p.events = map[string][]db.JobEvent{}
	}
	for _, receipt := range receipts {
		p.receipts[prKey(receipt.Repo, receipt.Number)] = receipt
	}
	foundRoot := false
	for _, job := range jobs {
		p.jobs[job.ID] = job
		p.jobIDs = append(p.jobIDs, job.ID)
		if job.ID == root.ID {
			foundRoot = true
			p.root = job
		}
		if job.ParentJobID != "" {
			p.children[job.ParentJobID] = append(p.children[job.ParentJobID], job)
		}
		if job.UpdatedAt > p.rootAsOf {
			p.rootAsOf = job.UpdatedAt
		}
	}
	if !foundRoot && root.ID != "" {
		p.jobs[root.ID] = root
		p.jobIDs = append(p.jobIDs, root.ID)
	}
	sort.Strings(p.jobIDs)
	for parent := range p.children {
		sort.Slice(p.children[parent], func(i, j int) bool {
			if p.children[parent][i].DelegationID != p.children[parent][j].DelegationID {
				return p.children[parent][i].DelegationID < p.children[parent][j].DelegationID
			}
			return p.children[parent][i].ID < p.children[parent][j].ID
		})
	}
	p.dagOK, p.dagGaps = p.validateLineage()
	p.indexImplementers()

	rootChildren := make([]string, 0, len(p.jobIDs))
	for _, jobID := range p.jobIDs {
		if id := p.buildSession(jobID); id != "" {
			rootChildren = append(rootChildren, id)
		}
	}
	rootAttrs := map[string]string{
		"root_id":        p.root.ID,
		"as_of":          p.rootAsOf,
		"dag_consistent": strconv.FormatBool(p.dagOK),
	}
	if len(p.dagGaps) > 0 {
		rootAttrs["evidence_gaps"] = strings.Join(p.dagGaps, "; ")
	}
	rootClaims := []Claim{}
	if p.dagOK {
		rootClaims = append(rootClaims, Claim{
			Type: "integrity.delegation_dag", Grade: GradeVerified,
			Source: "proof.projector", EvidenceRef: p.root.ID, AsOf: p.rootAsOf,
		})
	}
	rootID := p.builder.add(Node{
		Kind: KindRoot, Ref: p.root.ID, Attrs: rootAttrs,
		Children: rootChildren, Claims: rootClaims,
	})
	rootNode := p.builder.nodes[rootID]
	return Manifest{ProofID: rootID, Root: rootNode, Nodes: p.builder.nodes}
}

func (p *projector) buildSession(jobID string) string {
	if id := p.sessions[jobID]; id != "" {
		return id
	}
	job, ok := p.jobs[jobID]
	if !ok {
		return ""
	}
	if p.visiting[jobID] {
		p.dagOK = false
		return ""
	}
	p.visiting[jobID] = true
	defer delete(p.visiting, jobID)

	projection, payloadParseable := parsePayloadWithStatus(job.Payload)
	attrs := map[string]string{
		"agent": job.Agent, "job_type": job.Type, "state": job.State,
		"input_tokens":  strconv.Itoa(job.InputTokens),
		"output_tokens": strconv.Itoa(job.OutputTokens),
		"as_of":         job.UpdatedAt,
	}
	if !payloadParseable {
		attrs["payload_unparseable"] = "true"
		attrs["evidence_gap"] = "stored payload is not valid JSON"
	}
	setAttr(attrs, "runtime", firstNonBlank(job.Runtime, projection.RuntimeOverride))
	setAttr(attrs, "model", job.Model)
	setAttr(attrs, "runtime_ref", firstNonBlank(job.RuntimeRef, projection.RuntimeOverrideRef))
	if attrs["runtime_ref"] != "" {
		attrs["runtime_ref_quality"] = "best_effort"
	}
	setAttr(attrs, "created_at", job.CreatedAt)
	if job.ParentJobID != "" {
		attrs["parent_job_id"] = job.ParentJobID
	}

	claims := make([]Claim, 0)
	result := p.results[job.ID]
	if result == nil {
		result = projection.Result
	}
	if result != nil && job.Type != "review" && strings.TrimSpace(result.Decision) != "" {
		claims = append(claims, reportedClaim("result.decision", job, job.UpdatedAt))
	}
	for _, event := range sortedEvents(p.events[job.ID]) {
		claims = append(claims, Claim{
			Type: "job.event." + emptyDash(event.Kind), Grade: GradeObserved,
			Source: "job_event", EvidenceRef: eventRef(event), AsOf: event.CreatedAt,
		})
	}

	children := p.factNodes(job, projection, result)
	children = append(children, p.delegationNodes(job, result)...)
	sessionID := p.builder.add(Node{
		Kind: KindSession, Ref: job.ID, Attrs: attrs,
		Children: children, Claims: claims,
	})
	p.sessions[jobID] = sessionID
	return sessionID
}

func (p *projector) factNodes(job db.Job, projection payloadProjection, result *workflow.AgentResult) []string {
	children := make([]string, 0)
	repo := firstNonBlank(job.Repo, projection.Repo)
	prNumber := job.PullRequest
	if prNumber == 0 {
		prNumber = projection.PullRequest
	}
	receipt := p.receipts[prKey(repo, prNumber)]
	headSHA := firstNonBlank(projection.HeadSHA, receipt.HeadSHA)

	if result != nil && (len(result.ChangesMade) > 0 || job.ResultHash != "" || headSHA != "") {
		attrs := map[string]string{"job_id": job.ID, "as_of": job.UpdatedAt}
		setAttr(attrs, "head_sha", headSHA)
		setAttr(attrs, "result_hash", job.ResultHash)
		claims := make([]Claim, 0, len(result.ChangesMade)+1)
		for i, change := range result.ChangesMade {
			attrs[fmt.Sprintf("change.%d", i)] = change
			claims = append(claims, reportedClaim("change", job, job.UpdatedAt))
		}
		if job.ResultHash != "" {
			matches := resultHashMatches(job.Payload, job.ResultHash)
			attrs["result_hash_valid"] = strconv.FormatBool(matches)
			if matches {
				claims = append(claims, Claim{
					Type: "integrity.result_hash", Grade: GradeVerified,
					Source: "jobs.result_hash", EvidenceRef: hashPrefix + job.ResultHash,
					AsOf: job.UpdatedAt,
				})
			}
		}
		children = append(children, p.builder.add(Node{
			Kind: KindCommit, Ref: firstNonBlank(headSHA, job.ResultHash, job.ID),
			Attrs: attrs, Claims: claims,
		}))
	}

	if result != nil {
		for i, testRun := range result.TestsRun {
			children = append(children, p.builder.add(Node{
				Kind: KindTest, Ref: testRun,
				Attrs: map[string]string{
					"job_id": job.ID, "command": testRun, "index": strconv.Itoa(i),
					"evidence_gap": "CI verification deferred", "as_of": job.UpdatedAt,
				},
				Claims: []Claim{reportedClaim("test.run", job, job.UpdatedAt)},
			}))
		}
	}

	if job.Type == "review" {
		attrs := map[string]string{
			"job_id": job.ID, "agent": job.Agent, "as_of": job.UpdatedAt,
			"reviewed_ref": emptyDash(p.reviewedRef(job, headSHA)),
		}
		claims := []Claim{}
		if result != nil {
			attrs["decision"] = emptyDash(result.Decision)
			attrs["findings_count"] = strconv.Itoa(len(result.Findings))
			if strings.TrimSpace(result.Decision) != "" {
				claims = append(claims, reportedClaim("review.decision", job, job.UpdatedAt))
			}
			implementer, known := p.implementer[job.ID]
			implementerAgent := strings.TrimSpace(implementer.Agent)
			reviewerAgent := strings.TrimSpace(job.Agent)
			comparable := known && implementerAgent != "" && reviewerAgent != ""
			independent := comparable && implementerAgent != reviewerAgent
			if known {
				attrs["implementer_agent"] = implementer.Agent
			}
			if comparable {
				attrs["independent"] = strconv.FormatBool(independent)
			}
			if result.Decision == "approved" {
				claimType := "review.approved"
				if independent {
					claimType = "review.independent_approved"
				} else if comparable {
					claimType = "review.self_approved"
				}
				claims = append(claims, Claim{
					Type: claimType, Grade: GradeObserved, Source: "jobs.review",
					EvidenceRef: job.ID, AsOf: job.UpdatedAt,
				})
			}
		} else {
			attrs["decision"] = "-"
			attrs["findings_count"] = "0"
		}
		children = append(children, p.builder.add(Node{
			Kind: KindReview, Ref: job.ID, Attrs: attrs, Claims: claims,
		}))
	}

	if prNumber > 0 {
		attrs := map[string]string{
			"repo": emptyDash(repo), "number": strconv.Itoa(prNumber),
			"as_of": job.UpdatedAt,
		}
		setAttr(attrs, "head_sha", receipt.HeadSHA)
		setAttr(attrs, "merge_commit_sha", receipt.MergeCommitSHA)
		setAttr(attrs, "state", receipt.State)
		setAttr(attrs, "opened_at", receipt.OpenedAt)
		setAttr(attrs, "merged_at", receipt.MergedAt)
		claims := []Claim{}
		if receipt.OpenedAt != "" {
			claims = append(claims, Claim{
				Type: "pr.opened", Grade: GradeObserved, Source: "workflow_journal",
				EvidenceRef: fmt.Sprintf("%s#%d:opened", repo, prNumber), AsOf: receipt.OpenedAt,
			})
		}
		if receipt.MergedAt != "" {
			claims = append(claims, Claim{
				Type: "pr.merged", Grade: GradeObserved, Source: "workflow_journal",
				EvidenceRef: fmt.Sprintf("%s#%d:merged", repo, prNumber), AsOf: receipt.MergedAt,
			})
		}
		children = append(children, p.builder.add(Node{
			Kind: KindPR, Ref: fmt.Sprintf("%s#%d", emptyDash(repo), prNumber),
			Attrs: attrs, Claims: claims,
		}))
	}
	return children
}

func (p *projector) delegationNodes(parent db.Job, result *workflow.AgentResult) []string {
	specs := map[string]workflow.Delegation{}
	if result != nil {
		for _, delegation := range result.Delegations {
			specs[delegation.ID] = delegation
		}
	}
	type edge struct {
		key   string
		spec  workflow.Delegation
		child *db.Job
	}
	edges := map[string]edge{}
	for id, spec := range specs {
		edges[id] = edge{key: id, spec: spec}
	}
	for i := range p.children[parent.ID] {
		child := p.children[parent.ID][i]
		key := child.DelegationID
		if key == "" {
			key = "job:" + child.ID
		}
		if existing, ok := edges[key]; ok && existing.child != nil {
			key += ":" + child.ID
		}
		e := edges[key]
		e.key, e.child = key, &child
		if e.spec.ID == "" && child.DelegationID != "" {
			e.spec = specs[child.DelegationID]
		}
		edges[key] = e
	}
	keys := make([]string, 0, len(edges))
	for key := range edges {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	built := map[string]string{}
	visiting := map[string]bool{}
	var build func(string) string
	build = func(key string) string {
		if id := built[key]; id != "" {
			return id
		}
		e, ok := edges[key]
		if !ok || visiting[key] {
			return ""
		}
		visiting[key] = true
		defer delete(visiting, key)
		attrs := map[string]string{
			"parent_job_id": parent.ID, "delegation_id": emptyDash(e.spec.ID),
			"synthesis_rule": emptyDash(e.spec.SynthesisRule),
			"quorum":         strconv.Itoa(e.spec.Quorum), "as_of": parent.UpdatedAt,
		}
		if len(e.spec.Deps) > 0 {
			deps := append([]string(nil), e.spec.Deps...)
			sort.Strings(deps)
			attrs["deps"] = strings.Join(deps, ",")
		}
		children := []string{}
		for _, dep := range e.spec.Deps {
			if depID := build(dep); depID != "" {
				children = append(children, depID)
			}
		}
		if e.child != nil {
			attrs["child_job_id"] = e.child.ID
			attrs["delegation_depth"] = strconv.Itoa(e.child.DelegationDepth)
			attrs["delegated_by"] = emptyDash(e.child.DelegatedBy)
			childID := p.buildSession(e.child.ID)
			attrs["resolved"] = strconv.FormatBool(childID != "")
			if childID != "" {
				children = append(children, childID)
			}
		} else {
			attrs["resolved"] = "false"
		}
		id := p.builder.add(Node{
			Kind: KindDelegation, Ref: parent.ID + "/" + key,
			Attrs: attrs, Children: children,
		})
		built[key] = id
		return id
	}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		ids = append(ids, build(key))
	}
	return ids
}

func (p *projector) validateLineage() (bool, []string) {
	gaps := []string{}
	if len(p.jobs) > workflow.MaxDelegationTotalJobs {
		gaps = append(gaps, fmt.Sprintf("job budget exceeded: %d>%d", len(p.jobs), workflow.MaxDelegationTotalJobs))
	}
	for _, id := range p.jobIDs {
		job := p.jobs[id]
		if job.DelegationDepth < 0 || job.DelegationDepth > workflow.MaxDelegationDepth {
			gaps = append(gaps, fmt.Sprintf("job %s depth %d outside 0..%d", id, job.DelegationDepth, workflow.MaxDelegationDepth))
		}
		if job.RootID != "" && job.RootID != p.root.ID {
			gaps = append(gaps, fmt.Sprintf("job %s belongs to root %s", id, job.RootID))
		}
		if id != p.root.ID {
			parent, ok := p.jobs[job.ParentJobID]
			if !ok {
				gaps = append(gaps, fmt.Sprintf("job %s parent %s is absent", id, emptyDash(job.ParentJobID)))
			} else if job.DelegationDepth != parent.DelegationDepth+1 {
				gaps = append(gaps, fmt.Sprintf("job %s depth does not follow parent %s", id, parent.ID))
			}
		}
		result := p.results[id]
		if result == nil {
			projection := parsePayload(job.Payload)
			result = projection.Result
		}
		if result != nil {
			gaps = append(gaps, validateDelegationSpecs(id, result.Delegations, p.children[id])...)
		}
	}
	state := map[string]uint8{}
	var visit func(string)
	visit = func(id string) {
		if state[id] == 1 {
			gaps = append(gaps, "job parent graph contains a cycle at "+id)
			return
		}
		if state[id] == 2 {
			return
		}
		state[id] = 1
		for _, child := range p.children[id] {
			visit(child.ID)
		}
		state[id] = 2
	}
	visit(p.root.ID)
	sort.Strings(gaps)
	return len(gaps) == 0, compactStrings(gaps)
}

func validateDelegationSpecs(parent string, specs []workflow.Delegation, children []db.Job) []string {
	byID := map[string]workflow.Delegation{}
	childIDs := map[string]bool{}
	for _, child := range children {
		if child.DelegationID != "" {
			childIDs[child.DelegationID] = true
		}
	}
	for _, spec := range specs {
		byID[spec.ID] = spec
	}
	gaps := []string{}
	for id, spec := range byID {
		if !childIDs[id] {
			gaps = append(gaps, fmt.Sprintf("job %s delegation %s has no stored child", parent, id))
		}
		for _, dep := range spec.Deps {
			if _, ok := byID[dep]; !ok {
				gaps = append(gaps, fmt.Sprintf("job %s delegation %s dep %s is absent", parent, id, dep))
			}
		}
	}
	state := map[string]uint8{}
	var visit func(string)
	visit = func(id string) {
		if state[id] == 1 {
			gaps = append(gaps, fmt.Sprintf("job %s delegation deps contain a cycle at %s", parent, id))
			return
		}
		if state[id] == 2 {
			return
		}
		state[id] = 1
		for _, dep := range byID[id].Deps {
			if _, ok := byID[dep]; ok {
				visit(dep)
			}
		}
		state[id] = 2
	}
	for id := range byID {
		visit(id)
	}
	return gaps
}

func (p *projector) indexImplementers() {
	for _, id := range p.jobIDs {
		job := p.jobs[id]
		current := job
		seen := map[string]bool{}
		for current.ParentJobID != "" && !seen[current.ParentJobID] {
			seen[current.ParentJobID] = true
			parent, ok := p.jobs[current.ParentJobID]
			if !ok {
				break
			}
			if parent.Type == "implement" {
				p.implementer[id] = parent
				break
			}
			current = parent
		}
		if _, ok := p.implementer[id]; !ok && p.root.Type == "implement" && id != p.root.ID {
			p.implementer[id] = p.root
		}
	}
}

func (p *projector) reviewedRef(job db.Job, headSHA string) string {
	if headSHA != "" {
		return headSHA
	}
	if implementer, ok := p.implementer[job.ID]; ok {
		if implementer.ResultHash != "" {
			return hashPrefix + implementer.ResultHash
		}
		return implementer.ID
	}
	return ""
}

func parsePayload(raw string) payloadProjection {
	projection, _ := parsePayloadWithStatus(raw)
	return projection
}

func parsePayloadWithStatus(raw string) (payloadProjection, bool) {
	var projection payloadProjection
	if err := json.Unmarshal([]byte(raw), &projection); err != nil {
		return payloadProjection{}, false
	}
	return projection, true
}

func resultHashMatches(payload, stored string) bool {
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal([]byte(payload), &envelope) != nil {
		return false
	}
	raw := bytes.TrimSpace(envelope.Result)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) == nil {
		raw = compact.Bytes()
	}
	sum := sha256.Sum256(raw)
	return stored == hex.EncodeToString(sum[:])
}

// StoredResultHashMatches reports whether jobs.result_hash is the SHA-256 hex
// digest of the compacted, structured payload.result value. It deliberately
// does not hash Instructions, raw output, or any other free-form field.
func StoredResultHashMatches(payload, stored string) bool {
	return resultHashMatches(payload, strings.TrimSpace(stored))
}

func reportedClaim(claimType string, job db.Job, asOf string) Claim {
	evidence := job.ID
	if job.ResultHash != "" {
		evidence = hashPrefix + job.ResultHash
	}
	return Claim{
		Type: claimType, Grade: GradeReported, Source: "job.result",
		EvidenceRef: evidence, AsOf: asOf,
	}
}

func eventRef(event db.JobEvent) string {
	raw, _ := json.Marshal(struct {
		JobID, Kind, Message, CreatedAt string
	}{event.JobID, event.Kind, event.Message, event.CreatedAt})
	sum := sha256.Sum256(raw)
	return hashPrefix + hex.EncodeToString(sum[:])
}

func sortedEvents(events []db.JobEvent) []db.JobEvent {
	out := append([]db.JobEvent(nil), events...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Message < out[j].Message
	})
	return out
}

func prKey(repo string, number int) string {
	return strings.TrimSpace(repo) + "#" + strconv.Itoa(number)
}

func setAttr(attrs map[string]string, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		attrs[key] = value
	}
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
