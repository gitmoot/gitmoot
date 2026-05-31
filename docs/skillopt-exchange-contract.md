# Gitmoot-SkillOpt Exchange Contract

Gitmoot keeps the SkillOpt optimizer outside the main binary. The boundary is a
pair of JSON package formats handled by `gitmoot skillopt export` and
`gitmoot skillopt import`.

## Training Package

Export a local eval run:

```sh
gitmoot skillopt export --run run-2026-05-31 --output training.json
```

The exported package has:

- `kind: gitmoot-skillopt-training-package`
- `contract_version: 1`
- `template`: the logical template id, current or pinned version id, content
  hash, metadata, source, and exact template content
- `eval_run`: run id, target repo, state, metadata, and template version
- `items`: review/eval items with artifact references for source, baseline,
  candidate, preview, and diff artifacts
- `artifacts`: local artifact manifests with content hashes, media type, size,
  and driver
- `feedback_events`: canonical human feedback events when available
- `evaluator_config`: the run metadata used by the external optimizer

Artifact package entries reference local SHA256 blobs stored under Gitmoot home.
The export does not copy blobs into the repository by default.

## Candidate Package

Import a candidate produced by an external optimizer:

```sh
gitmoot skillopt import --file candidate.json
```

The imported package must have:

- `kind: gitmoot-skillopt-candidate-package`
- `contract_version: 1`
- `template_id`: an installed Gitmoot agent template id
- `base_version_id`: optional pinned version used by the optimizer
- `candidate.content`: full agent-template Markdown with YAML frontmatter
- `candidate.metadata`: metadata that exactly matches the candidate
  frontmatter
- `eval_report`: optional optimizer report
- `summary`: optional diff, score, and preference summary

Importing never promotes a candidate. Gitmoot stores it as a pending template
version so later review and promotion commands can decide whether it becomes
current.
