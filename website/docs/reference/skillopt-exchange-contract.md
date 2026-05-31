---
title: SkillOpt Exchange Contract
---

Gitmoot keeps the SkillOpt optimizer outside the main binary. The boundary is a
pair of JSON package formats handled by `gitmoot skillopt export` and
`gitmoot skillopt import`.

## Training Package

```sh
gitmoot skillopt export --run run-2026-05-31 --output training.json
```

The package contains the template snapshot, eval run, review items, artifact
manifests, canonical feedback events when available, and evaluator config.
Artifact entries reference local SHA256 blobs stored under Gitmoot home; blobs
are not copied into the repository by default.

## Candidate Package

```sh
gitmoot skillopt import --file candidate.json
```

The candidate package contains full agent-template Markdown with YAML
frontmatter, matching metadata, an optional eval report, and an optional summary.
Importing stores the candidate as a pending template version and never promotes
it automatically.
