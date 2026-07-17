package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/proof"
)

var pipelineReceiptTemplate = template.Must(template.New("receipt").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Gitmoot pipeline receipt</title><style>body{font:16px/1.5 system-ui,sans-serif;max-width:72rem;margin:3rem auto;padding:0 1.25rem;color:#171717}dl{display:grid;grid-template-columns:max-content 1fr;gap:.4rem 1rem}dt{font-weight:700}pre{padding:1rem;overflow:auto;background:#f5f5f5;border:1px solid #ddd}code{overflow-wrap:anywhere}</style></head>
<body><main><h1>Pipeline receipt</h1><dl><dt>Pipeline</dt><dd>{{.Pipeline}}</dd><dt>Run</dt><dd><code>{{.RunID}}</code></dd><dt>Completed</dt><dd>{{.Completed}}</dd><dt>Proof</dt><dd><code>{{.ProofID}}</code> · stored pipeline outcome verified</dd><dt>Archive</dt><dd><code>{{.ArtifactSHA}}</code> · <a href="{{.BundleURL}}">download bundle</a></dd></dl><h2>Evidence tree</h2><pre>{{.Tree}}</pre></main></body></html>`))

type publicPipelineReceipt struct {
	RunID       string
	Pipeline    string
	Completed   string
	ProofID     string
	ArtifactSHA string
	BundleURL   string
	Tree        string
	Artifact    string
}

func (d *webDataSource) handlePipelineReceipt(w http.ResponseWriter, r *http.Request) {
	receipt, err := d.loadPublicPipelineReceipt(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	setPipelineReceiptHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pipelineReceiptTemplate.Execute(w, receipt); err != nil {
		return
	}
}

func (d *webDataSource) handlePipelineReceiptBundle(w http.ResponseWriter, r *http.Request) {
	receipt, err := d.loadPublicPipelineReceipt(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	setPipelineReceiptHeaders(w)
	servePipelineServiceBundle(w, r, receipt.Artifact, receipt.RunID)
}

func (d *webDataSource) loadPublicPipelineReceipt(ctx context.Context, runID string) (publicPipelineReceipt, error) {
	if !pipelineServiceRunIDPattern.MatchString(runID) {
		return publicPipelineReceipt{}, errors.New("invalid service run id")
	}
	paths, err := pathsFromFlag(d.home)
	if err != nil {
		return publicPipelineReceipt{}, err
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return publicPipelineReceipt{}, err
	}
	defer store.Close()
	serviceRun, ok, err := store.GetServiceRun(ctx, runID)
	if err != nil || !ok || serviceRun.ArtifactRelpath == "" || serviceRun.ArtifactSHA256 == "" || serviceRun.ProofID == "" || serviceRun.ProofVerifiedAt.IsZero() {
		return publicPipelineReceipt{}, errors.New("service receipt is incomplete")
	}
	run, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil || !ok || run.Trigger != "service" || run.State != pipeline.RunSucceeded || run.FinishedAt.IsZero() || run.Pipeline != serviceRun.PipelineName {
		return publicPipelineReceipt{}, errors.New("service run is not a completed success")
	}
	artifact, err := containedServiceArtifactPath(paths, serviceRun.ArtifactRelpath)
	if err != nil {
		return publicPipelineReceipt{}, err
	}
	digest, err := hashFileSHA256(artifact)
	if err != nil || digest != serviceRun.ArtifactSHA256 {
		return publicPipelineReceipt{}, errors.New("service archive digest mismatch")
	}
	manifest, err := readProofManifestFromArchive(artifact)
	if err != nil || manifest.ProofID != serviceRun.ProofID {
		return publicPipelineReceipt{}, errors.New("service archive proof mismatch")
	}
	if err := proof.VerifyManifest(manifest); err != nil {
		return publicPipelineReceipt{}, err
	}
	var tree bytes.Buffer
	if err := proof.RenderTree(&tree, manifest); err != nil {
		return publicPipelineReceipt{}, err
	}
	return publicPipelineReceipt{
		RunID: run.ID, Pipeline: run.Pipeline, Completed: run.FinishedAt.UTC().Format("2006-01-02T15:04:05Z"),
		ProofID: manifest.ProofID, ArtifactSHA: serviceRun.ArtifactSHA256,
		BundleURL: pipelineServiceReceiptURL(run.ID) + "/bundle", Tree: tree.String(), Artifact: artifact,
	}, nil
}

func readProofManifestFromArchive(path string) (proof.Manifest, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return proof.Manifest{}, err
	}
	defer archive.Close()
	for _, file := range archive.File {
		if file.Name != "proof.json" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return proof.Manifest{}, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(reader, 16<<20))
		closeErr := reader.Close()
		if readErr != nil {
			return proof.Manifest{}, readErr
		}
		if closeErr != nil {
			return proof.Manifest{}, closeErr
		}
		var manifest proof.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return proof.Manifest{}, err
		}
		return manifest, nil
	}
	return proof.Manifest{}, errors.New("proof.json is absent")
}

func setPipelineReceiptHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}
