# Template Capture Workflow

Template capture turns a useful current Codex or Claude Code conversation into
a reusable Gitmoot agent template.

Capture happens in the current chat. The runtime reads the Gitmoot skill and
template-capture instructions, then distills visible conversation context,
inspected files, commands, corrections, and durable workflow rules into a
draft. Gitmoot cannot read hidden model memory or private runtime state.

## Capture From The Current Chat

```text
Use Gitmoot to capture this session as agent template release-planner. Draft only.
```

The chat should draft the template and wait. It should not start a daemon,
queue a `gitmoot agent ask` job, install the template, or replace an existing
template unless you explicitly ask.

## Scaffold, Validate, Install

Start with a blank template structure:

```sh
gitmoot agent template draft release-planner
```

After the draft is filled and reviewed:

```sh
gitmoot agent template validate .gitmoot/templates/release-planner.md
gitmoot agent template add release-planner --file .gitmoot/templates/release-planner.md
gitmoot agent prompt release-planner
```

## What Each Step Does

- `agent template draft` creates the markdown structure.
- "capture here" fills that structure from visible current-chat context.
- `agent template validate` checks title, required sections, regular file
  status, and unresolved placeholders.
- `agent template add` installs a snapshot into local Gitmoot state.
- `agent prompt` reuses the installed prompt in the current chat.
- `agent start --template` creates a runnable background agent instance.

Installed custom templates are snapshots. If you edit the source file later,
run:

```sh
gitmoot agent template diff release-planner
gitmoot agent template update release-planner
```
