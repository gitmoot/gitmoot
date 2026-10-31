package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/github"
)

type remoteReviewSourcePlan struct {
	gateway *credgw.Gateway
	policy  credgw.ProxyPolicy
	source  execbackend.RemoteReviewSource
}

// Source credentials are resolved on the host for each authenticated broker
// request. Never place the token in a job payload, sandbox file, argv, or log.
func resolveReviewSourceCredential(ctx context.Context, upstream string) (credgw.ResolvedCredential, error) {
	output, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return credgw.ResolvedCredential{}, errors.New("host GitHub source credential unavailable")
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return credgw.ResolvedCredential{}, errors.New("host GitHub source credential is empty")
	}
	return credgw.ResolvedCredential{Upstream: upstream, AuthKind: credgw.ProxyAuthGitRead, Value: token}, nil
}

func (w jobWorker) prepareRemoteReviewSource(ctx context.Context, job db.Job, cfg config.RemoteExecConfig, ttl time.Duration) (*remoteReviewSourcePlan, error) {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return nil, err
	}
	repo, err := github.ParseRepository(payload.Repo)
	if err != nil {
		return nil, err
	}
	if payload.PullRequest <= 0 || strings.TrimSpace(payload.HeadSHA) == "" {
		return nil, errors.New("remote source requires a pull request and exact head")
	}
	if err := dispatchHeadSHAError(payload.HeadSHA); err != nil {
		return nil, err
	}
	runner, err := jobSubprocessRunnerForBackend(execbackend.Remote)
	if err != nil {
		return nil, err
	}
	pull, err := w.remoteReviewAdmissionClient("", runner).GetPullRequest(ctx, repo, int64(payload.PullRequest))
	if err != nil {
		return nil, fmt.Errorf("read remote source subject: %w", err)
	}
	if !payload.ReviewChecksRouted && !strings.EqualFold(strings.TrimSpace(pull.HeadSHA), strings.TrimSpace(payload.HeadSHA)) {
		return nil, errors.New("remote source pull request head moved")
	}
	if strings.TrimSpace(pull.BaseRef) == "" {
		return nil, errors.New("remote source pull request has no target branch")
	}
	if strings.TrimSpace(cfg.CredentialGatewayListen) == "" || strings.TrimSpace(cfg.ProviderCredentialGatewayURL()) == "" {
		return nil, errors.New("E2B reviews require the configured job-scoped source credential gateway")
	}
	paths, err := w.configPaths()
	if err != nil {
		return nil, err
	}
	policy := credgw.ProxyPolicy{
		Upstream: "https://github.com/" + repo.FullName() + ".git", AuthKind: credgw.ProxyAuthGitRead,
		SandboxID: "preflight", Runtime: "shell", ExpiresAt: time.Now().Add(ttl),
		AllowedHosts: []string{"github.com"}, AdvertiseURL: cfg.ProviderCredentialGatewayURL(),
	}
	if _, _, err := credgw.ValidateProxyPolicy(policy); err != nil {
		return nil, err
	}
	gateway, err := credgw.DefaultRegistry.RemoteGateway(paths.Home, credgw.DefaultLogf, credgw.RemoteListenerOptions{
		ListenAddress: cfg.CredentialGatewayListen, AdvertiseURLs: cfg.CredentialGatewayURLs(),
	})
	if err != nil {
		return nil, err
	}
	plan := &remoteReviewSourcePlan{gateway: gateway, policy: policy, source: execbackend.RemoteReviewSource{
		Head: strings.ToLower(strings.TrimSpace(payload.HeadSHA)), BaseRef: pull.BaseRef,
	}}
	if pull.Merged || strings.TrimSpace(pull.MergedAt) != "" {
		plan.source.MergedSHA = strings.ToLower(strings.TrimSpace(pull.MergeSHA))
	}
	if payload.ReviewScope != nil {
		plan.source.DiffBase = strings.ToLower(strings.TrimSpace(payload.ReviewScope.PreviousHeadSHA))
	}
	return plan, nil
}

func (p *remoteReviewSourcePlan) register(jobID, sandboxID string) (*credgw.Lease, *execbackend.RemoteReviewSource, error) {
	policy := p.policy
	policy.SandboxID = sandboxID
	lease, err := p.gateway.RegisterProxy(jobID, policy, func(ctx context.Context) (credgw.ResolvedCredential, error) {
		return resolveReviewSourceCredential(ctx, policy.Upstream)
	})
	if err != nil {
		return nil, nil, err
	}
	material := lease.RemoteMaterial()
	dir := execbackend.ReviewSourceMaterialDir
	// These options are read only by the trusted preparation process. The file
	// is deleted before the reviewer starts; no source credential enters argv.
	lines := []string{
		"[remote \"source\"]", "\turl = " + strconv.Quote(material.URL),
		"[http]", "\tsslCAInfo = " + strconv.Quote(dir+"/ca.pem"),
		"\tsslCert = " + strconv.Quote(dir+"/client.pem"), "\tsslKey = " + strconv.Quote(dir+"/client.key"),
		"\textraHeader = " + strconv.Quote(credgw.CapabilityHeader+": "+material.Capability),
		"\textraHeader = " + strconv.Quote("Authorization: Bearer "+material.Placeholder),
		"\tfollowRedirects = false", "\tversion = HTTP/1.1",
		"[protocol]", "\tallow = never", "[protocol \"https\"]", "\tallow = always",
	}
	source := p.source
	source.Access = execbackend.CredentialMaterial{
		CACertificate: material.CACertificate, ClientCertificate: material.ClientCertificate,
		ClientPrivateKey: material.ClientPrivateKey, ClientConfig: []byte(strings.Join(lines, "\n") + "\n"),
	}
	return lease, &source, nil
}
