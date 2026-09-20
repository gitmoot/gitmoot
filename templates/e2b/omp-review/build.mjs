import { resolve } from "node:path";

import { Template } from "e2b";

function requiredEnv(name) {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`${name} is required`);
  }
  return value;
}

function positiveIntegerEnv(name, fallback) {
  const configured = process.env[name];
  const raw = configured === undefined ? String(fallback) : configured.trim();
  if (!/^[1-9]\d*$/.test(raw)) {
    throw new Error(`${name} must be a positive integer`);
  }
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value > 2_147_483_647) {
    throw new Error(`${name} must fit in a signed 32-bit integer`);
  }
  return value;
}

const apiKey = requiredEnv("E2B_API_KEY");
const name = requiredEnv("E2B_TEMPLATE_NAME");
const cpuCount = positiveIntegerEnv("E2B_CPU_COUNT", 4);
const memoryMB = positiveIntegerEnv("E2B_MEMORY_MB", 4096);
if (memoryMB < 2048) {
  throw new Error("E2B_MEMORY_MB must be at least 2048");
}
if (memoryMB % 2 !== 0) {
  throw new Error("E2B_MEMORY_MB must be even");
}

const dockerfile = resolve(import.meta.dirname, "Dockerfile");
const template = Template().fromDockerfile(dockerfile);
const build = await Template.build(template, name, {
  apiKey,
  cpuCount,
  memoryMB,
  onBuildLogs(entry) {
    process.stderr.write(`${entry.level}: ${entry.message}\n`);
  },
});

process.stdout.write(`${JSON.stringify(build)}\n`);
