import {mkdir, readFile, writeFile} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

const websiteDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(websiteDir, '..', '..');
const staticDir = path.join(repoRoot, 'website', 'static');

const sources = [
  'README.md',
  'SKILL.md',
  'docs/local-workflow.md',
  'docs/plugins.md',
  'docs/adapters.md',
  'docs/troubleshooting.md',
  'docs/beta-smoke-tests.md',
  'skills/gitmoot/SKILL.md',
  'skills/gitmoot/agent-templates/planner.md',
  'skills/gitmoot/agent-templates/planner-here.md',
  'skills/gitmoot/references/CLI.md',
  'skills/gitmoot/references/WORKFLOWS.md',
  'skills/gitmoot/references/SAFETY.md',
  'skills/gitmoot/references/RESULT_CONTRACT.md',
  'skills/gitmoot/references/GOAL_TEMPLATE.md',
];

const header = `# Gitmoot Full LLM Context

This file is generated from canonical Gitmoot Markdown sources. Prefer
https://gitmoot.io/llms.txt for a concise index and this file when an agent
needs fuller local workflow, CLI, plugin, safety, and result-contract context.
`;

const parts = [header.trimEnd()];

for (const source of sources) {
  const absolute = path.join(repoRoot, source);
  const body = await readFile(absolute, 'utf8');
  parts.push(`\n\n---\n\n# Source: ${source}\n\n${body.trimEnd()}`);
}

await mkdir(staticDir, {recursive: true});
await writeFile(path.join(staticDir, 'llms-full.txt'), `${parts.join('')}\n`);
