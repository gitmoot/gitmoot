# Template Capture

Use template capture when the user wants to turn the current Codex or Claude
Code session into a reusable Gitmoot agent template.

Template capture is current-chat distillation. Gitmoot cannot read hidden model
memory, runtime internals, or private state outside the visible conversation and
files you inspect. Capture only durable, user-approved behavior that can be
reused safely by future agents.

## Trigger Phrases

Use this workflow for requests like:

- "capture this session as a Gitmoot agent template"
- "turn this workflow into a Gitmoot template"
- "draft a reusable agent template from this chat"
- "make this current agent behavior reusable"

Do not route template capture through `gitmoot agent ask`, PR comments, or the
daemon unless the user explicitly asks for a background job. Capture from
"here" means the current chat writes a draft.

## Capture Rules

- Draft first. Do not install, overwrite, or update a permanent template unless
  the user explicitly approves that step.
- Extract durable workflow rules, role, trigger conditions, inputs, commands,
  output contract, safety rules, examples, and non-goals.
- Exclude one-off mistakes, temporary debugging, private secrets, unrelated repo
  details, hidden model memory, and unverified assumptions.
- Prefer project-agnostic language unless the template is intentionally scoped
  to one project or repo.
- Preserve important user preferences that were corrected repeatedly, but write
  them as actionable rules instead of emotional transcript summaries.
- Mark uncertainty as a question or assumption rather than turning it into a
  permanent instruction.
- Ask before installing or replacing an existing template.

## Draft Structure

Use this structure for captured templates:

```markdown
# <Template Name>

## Role

Describe what this agent is responsible for.

## When To Use

List the request types, project contexts, or trigger phrases that should use
this template.

## Workflow

Give the repeatable step-by-step operating procedure.

## Inputs And Context

List the files, commands, tools, docs, conversation context, or repo state the
agent should inspect.

## Commands And Tools

List important CLI commands, runtime commands, web/source lookup requirements,
or tool usage constraints.

## Output Contract

Describe exactly what the agent should return or create.

## Safety Rules

List boundaries, approval gates, non-destructive behavior, secret handling, and
when to stop or ask.

## Examples

Provide a few concise example requests and the expected style of response.

## Non-Goals

List work this template should not do.
```

## Suggested Current-Chat Flow

1. Confirm the requested template id and intended scope if unclear.
2. Inspect relevant visible conversation context and repo files.
3. Draft the markdown template in chat, or write it to the path the user
   requested.
4. Tell the user to check the draft against the structure above before
   installing it:

   ```sh
   gitmoot agent template add <template-id> --file .gitmoot/templates/<template-id>.md
   ```

5. If the user approves installation, check that required sections are filled
   in, then install with `gitmoot agent template add`.

## Result Expectations

The final capture response should say what was drafted, where it is saved if a
file was written, whether validation ran, and whether the template was
installed. If installation was not explicitly requested, say that the draft is
not installed yet.
