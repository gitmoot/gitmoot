// Canary for a freshly built template: start one sandbox, upload a checkout
// archive, run a command in it, kill the sandbox, and print timing and a
// list-price cost estimate as JSON. Command output streams to stderr.
import { readFile } from "node:fs/promises";

import { CommandExitError, Sandbox } from "e2b";

// E2B list price per second (https://e2b.dev/pricing).
const USD_PER_VCPU_SECOND = 0.000014;
const USD_PER_GIB_SECOND = 0.0000045;

const WORKSPACE = "/home/user/workspace";

function requiredEnv(name) {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`${name} is required`);
  }
  return value;
}

const apiKey = requiredEnv("E2B_API_KEY");
const template = requiredEnv("E2B_TEMPLATE_NAME");
// A .tar.gz whose entries sit under one top-level directory (the checkout).
const source = requiredEnv("VERIFY_SOURCE");
const command = requiredEnv("VERIFY_COMMAND");
// E2B caps a sandbox's lifetime at one hour; keep ten minutes for setup.
const timeoutMinutes = Number(process.env.VERIFY_TIMEOUT_MINUTES ?? "50");
if (!Number.isInteger(timeoutMinutes) || timeoutMinutes < 1 || timeoutMinutes > 50) {
  throw new Error("VERIFY_TIMEOUT_MINUTES must be an integer from 1 to 50");
}

const seconds = (from, to) => Number(((to - from) / 1000).toFixed(1));
const stream = (data) => process.stderr.write(data);

const archive = await readFile(source);
const started = Date.now();
const sandbox = await Sandbox.create(template, {
  apiKey,
  timeoutMs: (timeoutMinutes + 10) * 60_000,
});
let result;
try {
  const info = await sandbox.getInfo();
  await sandbox.files.write("/home/user/source.tar.gz", new Blob([archive]));
  await sandbox.commands.run(
    `mkdir -p ${WORKSPACE} && tar -xzf /home/user/source.tar.gz -C ${WORKSPACE} --strip-components=1 && rm /home/user/source.tar.gz`,
    { timeoutMs: 600_000 },
  );
  const ready = Date.now();
  let exitCode;
  try {
    const run = await sandbox.commands.run(command, {
      cwd: WORKSPACE,
      timeoutMs: timeoutMinutes * 60_000,
      onStdout: stream,
      onStderr: stream,
    });
    exitCode = run.exitCode;
  } catch (error) {
    if (!(error instanceof CommandExitError)) {
      throw error;
    }
    exitCode = error.exitCode;
  }
  const finished = Date.now();
  result = {
    template,
    templateId: info.templateId,
    sandboxId: sandbox.sandboxId,
    cpuCount: info.cpuCount,
    memoryMB: info.memoryMB,
    command,
    exitCode,
    setupSeconds: seconds(started, ready),
    commandSeconds: seconds(ready, finished),
  };
} finally {
  await sandbox.kill();
}
result.wallSeconds = seconds(started, Date.now());
const usdPerSecond =
  result.cpuCount * USD_PER_VCPU_SECOND + (result.memoryMB / 1024) * USD_PER_GIB_SECOND;
result.estimatedCostUsd = Number((result.wallSeconds * usdPerSecond).toFixed(4));
process.stdout.write(`${JSON.stringify(result)}\n`);
process.exitCode = result.exitCode === 0 ? 0 : 1;
