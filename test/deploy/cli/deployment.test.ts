import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import {
  link,
  mkdtemp,
  readFile,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";

import {
  parseCLIOptions,
  runDeployment,
} from "../../../scripts/deployRescuerV2.js";
import { verifyCanonicalArtifact } from "../../../scripts/verifyArtifacts.js";

const destination = "0x1000000000000000000000000000000000000001";
const sponsor = "0x2000000000000000000000000000000000000002";

const runCLI = async (
  args: readonly string[],
): Promise<{ readonly code: number | null; readonly stderr: string }> => {
  const child = spawn(
    process.execPath,
    ["--import", "tsx", resolve("scripts/deployRescuerV2.ts"), ...args],
    { env: process.env, stdio: ["ignore", "ignore", "pipe"] },
  );
  const stderr: Buffer[] = [];
  child.stderr.on("data", (chunk: Buffer) => stderr.push(chunk));
  const code = await new Promise<number | null>((resolveExit, reject) => {
    child.once("error", reject);
    child.once("exit", resolveExit);
  });
  return { code, stderr: Buffer.concat(stderr).toString("utf8") };
};

test("режим по умолчанию не читает ключ и не обращается к RPC", async () => {
  const options = parseCLIOptions([
    "--chain-id",
    "31337",
    "--destination",
    destination,
    "--sponsor",
    sponsor,
    "--rpc-url",
    "http://127.0.0.1:1/private-credential",
  ]);

  await runDeployment(options, { DEPLOYER_PRIVATE_KEY: "not-a-private-key" });
});

test("подменённый artifact отклоняется повторной pinned-сборкой", async () => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), "guard-artifact-"));
  const artifactPath = join(temporaryDirectory, "RescuerV2.json");
  const canonical = await readFile(
    resolve("artifacts/contracts/RescuerV2.json"),
    "utf8",
  );
  await writeFile(
    artifactPath,
    canonical.replace(
      '"compilerVersion": "0.8.36',
      '"compilerVersion": "0.8.35',
    ),
  );
  try {
    await assert.rejects(
      verifyCanonicalArtifact(artifactPath),
      /pinned source\/compiler\/settings/,
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("wrong chain блокирует отправку и атомарное обновление", async () => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), "guard-deploy-cli-"));
  const manifestPath = join(temporaryDirectory, "manifest.json");
  const configPath = join(temporaryDirectory, "operator.env");
  const originalManifest = "manifest-before\n";
  const originalConfig = "RESCUER_LOCAL=before\n";
  await Promise.all([
    writeFile(manifestPath, originalManifest),
    writeFile(configPath, originalConfig),
  ]);

  const server = createServer((request, response) => {
    const chunks: Buffer[] = [];
    request.on("data", (chunk: Buffer) => chunks.push(chunk));
    request.on("end", () => {
      const payload = JSON.parse(Buffer.concat(chunks).toString("utf8")) as
        | { readonly id: number; readonly method: string }
        | { readonly id: number; readonly method: string }[];
      const answer = (item: {
        readonly id: number;
        readonly method: string;
      }) => ({
        id: item.id,
        jsonrpc: "2.0",
        result: item.method === "eth_chainId" ? "0x1" : "0x0",
      });
      response.setHeader("content-type", "application/json");
      response.end(
        JSON.stringify(
          Array.isArray(payload)
            ? payload.map(answer)
            : answer(
                payload as { readonly id: number; readonly method: string },
              ),
        ),
      );
    });
  });
  await new Promise<void>((resolveListen) =>
    server.listen(0, "127.0.0.1", resolveListen),
  );
  const address = server.address();
  if (!address || typeof address === "string") {
    assert.fail("Тестовый RPC не получил TCP port");
  }

  try {
    const args = [
      "--chain-id",
      "31337",
      "--destination",
      destination,
      "--sponsor",
      sponsor,
      "--rpc-url",
      `http://127.0.0.1:${address.port}`,
      "--manifest",
      manifestPath,
      "--config",
      configPath,
      "--config-key",
      "RESCUER_LOCAL",
      "--broadcast",
    ];
    const result = await runCLI(args);
    assert.deepStrictEqual(
      {
        config: await readFile(configPath, "utf8"),
        exitCode: result.code,
        manifest: await readFile(manifestPath, "utf8"),
        redactedError:
          result.stderr.includes("chain ID") &&
          !result.stderr.includes(`127.0.0.1:${address.port}`),
      },
      {
        config: originalConfig,
        exitCode: 1,
        manifest: originalManifest,
        redactedError: true,
      },
    );
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolveClose, reject) =>
      server.close((error) => (error ? reject(error) : resolveClose())),
    );
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("CLI требует явный broadcast и полный набор аргументов", () => {
  assert.throws(() => parseCLIOptions([]), /--chain-id/);
  assert.throws(
    () =>
      parseCLIOptions([
        "--chain-id",
        "31337",
        "--destination",
        destination,
        "--sponsor",
        sponsor,
        "--broadcast",
      ]),
    /--rpc-url и --manifest/,
  );
  assert.equal(
    parseCLIOptions([
      "--chain-id",
      "31337",
      "--destination",
      destination,
      "--sponsor",
      sponsor,
    ]).broadcast,
    false,
  );
});

test("конфликт output paths блокируется до RPC и изменения файлов", async () => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), "guard-paths-"));
  const sharedPath = join(temporaryDirectory, "shared");
  const original = "RESCUER_LOCAL=before\n";
  await writeFile(sharedPath, original);
  try {
    const options = parseCLIOptions([
      "--chain-id",
      "31337",
      "--destination",
      destination,
      "--sponsor",
      sponsor,
      "--rpc-url",
      "http://127.0.0.1:1/private-credential",
      "--manifest",
      sharedPath,
      "--config",
      sharedPath,
      "--config-key",
      "RESCUER_LOCAL",
      "--broadcast",
    ]);
    await assert.rejects(runDeployment(options, {}), /разными файлами/);
    assert.equal(await readFile(sharedPath, "utf8"), original);
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("symlink и hardlink aliases artifact/config отклоняются", async () => {
  const artifactPath = resolve("artifacts/contracts/RescuerV2.json");
  const temporaryDirectory = await mkdtemp(
    join(resolve("artifacts/contracts"), ".guard-aliases-"),
  );
  const artifactBefore = await readFile(artifactPath);
  const artifactAlias = join(temporaryDirectory, "artifact-alias.json");
  const configPath = join(temporaryDirectory, "operator.env");
  const configAlias = join(temporaryDirectory, "config-alias.env");
  await Promise.all([
    link(artifactPath, artifactAlias),
    writeFile(configPath, "RESCUER_LOCAL=before\n"),
  ]);
  await symlink(configPath, configAlias);
  try {
    const common = [
      "--chain-id",
      "31337",
      "--destination",
      destination,
      "--sponsor",
      sponsor,
      "--rpc-url",
      "http://127.0.0.1:1/private-credential",
      "--broadcast",
    ];
    await assert.rejects(
      runDeployment(
        parseCLIOptions([...common, "--manifest", artifactAlias]),
        {},
      ),
      /разными файлами/,
    );
    await assert.rejects(
      runDeployment(
        parseCLIOptions([
          ...common,
          "--manifest",
          configAlias,
          "--config",
          configPath,
          "--config-key",
          "RESCUER_LOCAL",
        ]),
        {},
      ),
      /разными файлами/,
    );
    assert.deepStrictEqual(
      {
        artifact: await readFile(artifactPath),
        config: await readFile(configPath, "utf8"),
      },
      { artifact: artifactBefore, config: "RESCUER_LOCAL=before\n" },
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});
