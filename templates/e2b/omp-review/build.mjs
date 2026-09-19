import { resolve } from "node:path";

import { Template } from "e2b";

const apiKey = process.env.E2B_API_KEY?.trim();
if (!apiKey) {
  throw new Error("E2B_API_KEY is required");
}

const name = process.env.E2B_TEMPLATE_NAME?.trim() || "gitmoot-omp-go126-v2";
const dockerfile = resolve(import.meta.dirname, "Dockerfile");
const template = Template().fromDockerfile(dockerfile);
const build = await Template.build(template, name, {
  apiKey,
  cpuCount: 4,
  memoryMB: 4096,
  onBuildLogs(entry) {
    process.stderr.write(`${entry.level}: ${entry.message}\n`);
  },
});

process.stdout.write(`${JSON.stringify(build)}\n`);
