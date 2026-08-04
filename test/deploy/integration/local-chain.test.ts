import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { createServer } from "node:net";
import {
  mkdir,
  mkdtemp,
  readFile,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";
import { setTimeout as delay } from "node:timers/promises";

import { JsonRpcProvider, Transaction, Wallet } from "ethers";

import {
  parseCLIOptions,
  runDeployment,
} from "../../../scripts/deployRescuerV2.js";

const deterministicPrivateKey = (label: string): string =>
  `0x${createHash("sha256").update(`guard-daemon-test:${label}`).digest("hex")}`;

const repositoryRoot = resolve(".");
const candidateContentNames = [
  "LICENSE",
  "RescuerV2.json",
  "guard-daemon-linux-amd64",
  "guard-daemon-source.tar",
  "guard-daemon.cdx.json",
  "guard-daemon.intoto.jsonl",
  "release-candidate.json",
  "rescuer-manifest.schema.json",
] as const;

const isRecord = (value: unknown): value is Readonly<Record<string, unknown>> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const requiredRecord = (
  value: Readonly<Record<string, unknown>>,
  name: string,
): Readonly<Record<string, unknown>> => {
  const field = value[name];
  if (!isRecord(field)) {
    assert.fail(`Manifest не содержит object ${name}`);
  }
  return field;
};

const requiredString = (
  value: Readonly<Record<string, unknown>>,
  name: string,
): string => {
  const field = value[name];
  if (typeof field !== "string") {
    assert.fail(`Manifest не содержит string ${name}`);
  }
  return field;
};

const runGitText = async (
  cwd: string,
  args: readonly string[],
): Promise<string> =>
  new Promise((resolveCommand, rejectCommand) => {
    execFile(
      "git",
      [...args],
      { cwd, encoding: "utf8", maxBuffer: 1024 * 1024 },
      (error, stdout) => {
        if (error) {
          rejectCommand(error);
          return;
        }
        resolveCommand(stdout.trim());
      },
    );
  });

const gitArchive = async (cwd: string, commit: string): Promise<Buffer> => {
  const process = spawn(
    "git",
    [
      "-c",
      "core.attributesFile=/dev/null",
      "archive",
      "--format=tar",
      `--prefix=guard-daemon-${commit}/`,
      commit,
    ],
    { cwd, stdio: ["ignore", "pipe", "ignore"] },
  );
  const chunks: Buffer[] = [];
  process.stdout.on("data", (chunk: Buffer) => chunks.push(chunk));
  const code = await new Promise<number>((resolveExit, rejectExit) => {
    process.once("error", rejectExit);
    process.once("close", (value) => resolveExit(value ?? -1));
  });
  assert.equal(code, 0);
  return Buffer.concat(chunks);
};

const writeReleaseCandidate = async (
  directory: string,
  checkout: string,
): Promise<{ readonly path: string; readonly releaseCommit: string }> => {
  await mkdir(directory, { recursive: true });
  const commit = await runGitText(checkout, [
    "rev-parse",
    "--verify",
    "HEAD^{commit}",
  ]);
  const tree = await runGitText(checkout, [
    "show",
    "-s",
    "--format=%T",
    commit,
  ]);
  const artifact = await readFile(
    join(checkout, "artifacts/contracts/RescuerV2.json"),
  );
  const outputs = new Map<string, Buffer>([
    ["LICENSE", await readFile(join(checkout, "LICENSE"))],
    ["RescuerV2.json", artifact],
    ["guard-daemon-linux-amd64", Buffer.from("test binary\n")],
    ["guard-daemon-source.tar", await gitArchive(checkout, commit)],
    ["guard-daemon.cdx.json", Buffer.from("{}\n")],
    ["guard-daemon.intoto.jsonl", Buffer.from("{}\n")],
    [
      "rescuer-manifest.schema.json",
      await readFile(
        join(checkout, "deployments/schema/rescuer-manifest.schema.json"),
      ),
    ],
  ]);
  for (const [name, content] of outputs) {
    await writeFile(join(directory, name), content);
  }
  const digest = (name: string): string => {
    const content = outputs.get(name);
    assert.ok(content);
    return `sha256:${createHash("sha256").update(content).digest("hex")}`;
  };
  const candidate = {
    artifacts: {
      binary: {
        path: "guard-daemon-linux-amd64",
        sha256: digest("guard-daemon-linux-amd64"),
      },
      contract: {
        path: "RescuerV2.json",
        sha256: digest("RescuerV2.json"),
      },
      manifestSchema: {
        path: "rescuer-manifest.schema.json",
        sha256: digest("rescuer-manifest.schema.json"),
      },
      sbom: {
        path: "guard-daemon.cdx.json",
        sha256: digest("guard-daemon.cdx.json"),
      },
      sourceArchive: {
        path: "guard-daemon-source.tar",
        sha256: digest("guard-daemon-source.tar"),
      },
    },
    releaseCommit: commit,
    releaseTree: tree,
    schemaVersion: "1",
    target: { cgoEnabled: false, goarch: "amd64", goos: "linux" },
  } as const;
  const candidateContent = Buffer.from(
    `${JSON.stringify(candidate, null, 2)}\n`,
  );
  outputs.set("release-candidate.json", candidateContent);
  await writeFile(join(directory, "release-candidate.json"), candidateContent);
  const checksums = candidateContentNames
    .map((name) => {
      const content = outputs.get(name);
      assert.ok(content);
      return `${createHash("sha256").update(content).digest("hex")}  ${name}\n`;
    })
    .join("");
  await writeFile(join(directory, "SHA256SUMS"), checksums);
  return {
    path: join(directory, "release-candidate.json"),
    releaseCommit: commit,
  };
};

const freePort = async (): Promise<number> => {
  const server = createServer();
  await new Promise<void>((resolveListen) =>
    server.listen(0, "127.0.0.1", resolveListen),
  );
  const address = server.address();
  if (!address || typeof address === "string") {
    assert.fail("Не удалось выделить локальный TCP-порт");
  }
  await new Promise<void>((resolveClose, reject) =>
    server.close((error) => (error ? reject(error) : resolveClose())),
  );
  return address.port;
};

const waitForSpawn = async (process: ChildProcess): Promise<void> =>
  new Promise((resolveSpawn, rejectSpawn) => {
    const onError = (): void => {
      process.off("spawn", onSpawn);
      rejectSpawn(
        new Error(
          "Не удалось запустить тестовый Anvil; проверьте установку Foundry",
        ),
      );
    };
    const onSpawn = (): void => {
      process.off("error", onError);
      resolveSpawn();
    };
    process.once("error", onError);
    process.once("spawn", onSpawn);
  });

const waitForAnvil = async (
  rpcURL: string,
  process: ChildProcess,
): Promise<void> => {
  for (let attempt = 0; attempt < 100; attempt++) {
    if (process.exitCode !== null || process.signalCode !== null) {
      throw new Error(
        `Anvil завершился до готовности: код ${process.exitCode}, сигнал ${process.signalCode}`,
      );
    }
    const response = await fetch(rpcURL, {
      body: JSON.stringify({
        id: 1,
        jsonrpc: "2.0",
        method: "eth_chainId",
        params: [],
      }),
      headers: { "content-type": "application/json" },
      method: "POST",
      signal: AbortSignal.timeout(200),
    }).catch(() => undefined);
    if (response?.ok) return;
    await delay(50);
  }
  throw new Error("Истёк таймаут запуска Anvil");
};

const stopAnvil = async (process: ChildProcess): Promise<void> => {
  if (process.exitCode !== null || process.signalCode !== null) return;

  const closed = new Promise<void>((resolveClose) =>
    process.once("close", () => resolveClose()),
  );
  process.kill("SIGTERM");
  const stopped = await Promise.race([
    closed.then(() => true),
    delay(2_000).then(() => false),
  ]);
  if (stopped) return;

  if (process.exitCode === null && process.signalCode === null) {
    process.kill("SIGKILL");
  }
  const killed = await Promise.race([
    closed.then(() => true),
    delay(2_000).then(() => false),
  ]);
  if (!killed) throw new Error("Не удалось остановить тестовый Anvil");
};

type LocalAnvil = {
  readonly process: ChildProcess;
  readonly rpcURL: string;
};

const startAnvil = async (): Promise<LocalAnvil> => {
  for (let attempt = 0; attempt < 3; attempt++) {
    const port = await freePort();
    const process = spawn(
      "anvil",
      [
        "--quiet",
        "--host",
        "127.0.0.1",
        "--port",
        String(port),
        "--chain-id",
        "31337",
      ],
      { stdio: "ignore" },
    );
    const spawned = waitForSpawn(process);
    try {
      await spawned;
    } catch (error) {
      await stopAnvil(process);
      throw error;
    }
    const rpcURL = `http://127.0.0.1:${port}`;
    try {
      await waitForAnvil(rpcURL, process);
      return { process, rpcURL };
    } catch {
      await stopAnvil(process);
    }
  }
  throw new Error(
    "Не удалось запустить тестовый Anvil на свободном локальном порту",
  );
};

test(
  "локальное развёртывание сохраняет recovery и эксклюзивно публикует manifest",
  { timeout: 60_000 },
  async () => {
    const temporaryDirectory = await mkdtemp(
      join(tmpdir(), "guard-deploy-integration-"),
    );
    const deployerKey = deterministicPrivateKey("deployer");
    const deployer = new Wallet(deployerKey);
    const destination = new Wallet(deterministicPrivateKey("destination"))
      .address;
    let anvil: LocalAnvil | undefined;
    let provider: JsonRpcProvider | undefined;
    let worktreeAdded = false;
    const checkout = join(temporaryDirectory, "checkout");
    const manifestPath = join(temporaryDirectory, "manifest.json");
    const recoveryDirectory = join(temporaryDirectory, "recovery");
    const configPath = join(temporaryDirectory, "operator.env");
    const previousDirectory = process.cwd();

    try {
      await runGitText(repositoryRoot, [
        "worktree",
        "add",
        "--detach",
        checkout,
        "HEAD",
      ]);
      worktreeAdded = true;
      const candidate = await writeReleaseCandidate(
        join(temporaryDirectory, "candidate"),
        checkout,
      );
      await mkdir(recoveryDirectory, { mode: 0o700 });
      const recoveryPath = join(
        recoveryDirectory,
        `rescuer-v2-31337-${deployer.address.toLowerCase()}-${candidate.releaseCommit}.json`,
      );
      anvil = await startAnvil();
      await writeFile(configPath, "RESCUER_LOCAL=unchanged\n");
      provider = new JsonRpcProvider(anvil.rpcURL);
      await provider.send("anvil_setBalance", [
        deployer.address,
        "0x56bc75e2d63100000",
      ]);
      const beforePlan = await provider.getBlockNumber();
      process.chdir(checkout);
      await runDeployment(
        parseCLIOptions([
          "--chain-id",
          "31337",
          "--destination",
          destination,
          "--sponsor",
          deployer.address,
          "--rpc-url",
          anvil.rpcURL,
        ]),
        { DEPLOYER_PRIVATE_KEY: "invalid-and-unused" },
      );
      const afterPlan = await provider.getBlockNumber();
      assert.equal(afterPlan, beforePlan);

      await runDeployment(
        parseCLIOptions([
          "--chain-id",
          "31337",
          "--destination",
          destination,
          "--sponsor",
          deployer.address,
          "--rpc-url",
          anvil.rpcURL,
          "--manifest",
          manifestPath,
          "--recovery-directory",
          recoveryDirectory,
          "--release-candidate",
          candidate.path,
          "--gas-limit",
          "1000000",
          "--max-fee-per-gas-wei",
          "5000000000",
          "--max-priority-fee-per-gas-wei",
          "100000000",
          "--max-total-cost-wei",
          "5000000000000000",
          "--broadcast",
        ]),
        { DEPLOYER_PRIVATE_KEY: deployerKey },
      );
      process.chdir(previousDirectory);

      const manifestText = await readFile(manifestPath, "utf8");
      const manifestValue: unknown = JSON.parse(manifestText);
      if (!isRecord(manifestValue)) {
        assert.fail("Manifest не является object");
      }
      const immutables = requiredRecord(manifestValue, "immutables");
      const runtime = requiredRecord(manifestValue, "runtime");
      const source = requiredRecord(manifestValue, "source");
      const address = requiredString(manifestValue, "address");
      const transactionHash = requiredString(
        manifestValue,
        "deploymentTransactionHash",
      );
      const recoveryText = await readFile(recoveryPath, "utf8");
      const recoveryValue: unknown = JSON.parse(recoveryText);
      if (!isRecord(recoveryValue)) {
        assert.fail("Recovery record не является object");
      }
      const recoveryTransaction = requiredRecord(recoveryValue, "transaction");
      const rawSignedTransaction = requiredString(
        recoveryTransaction,
        "rawSignedTransaction",
      );
      const parsedRecoveryTransaction = Transaction.from(rawSignedTransaction);
      const config = await readFile(configPath, "utf8");
      const code = await provider.getCode(address);
      const transaction = await provider.getTransaction(transactionHash);
      if (!transaction) {
        assert.fail("Anvil не вернул deployment transaction");
      }
      assert.deepStrictEqual(
        {
          chain: requiredString(manifestValue, "chainId"),
          config,
          destination: requiredString(immutables, "destination"),
          gasLimit: transaction.gasLimit,
          hasRuntime:
            code !== "0x" &&
            requiredString(runtime, "keccak256").startsWith("0x"),
          maxFeePerGas: transaction.maxFeePerGas,
          maxPriorityFeePerGas: transaction.maxPriorityFeePerGas,
          noPrivateKey:
            !manifestText.includes(deployerKey.slice(2)) &&
            !recoveryText.includes(deployerKey.slice(2)),
          recoveryChain: requiredString(recoveryValue, "chainId"),
          recoveryHash: requiredString(recoveryTransaction, "hash"),
          recoveryMode: (await stat(recoveryPath)).mode & 0o777,
          recoveryNonce: Reflect.get(recoveryTransaction, "nonce"),
          recoverySponsor: requiredString(recoveryValue, "sponsor"),
          signedHash: parsedRecoveryTransaction.hash,
          self: requiredString(immutables, "self"),
          sourceKind: requiredString(source, "kind"),
          sourceValue: requiredString(source, "value"),
          sponsor: requiredString(immutables, "sponsor"),
        },
        {
          chain: "31337",
          config: "RESCUER_LOCAL=unchanged\n",
          destination,
          gasLimit: 1_000_000n,
          hasRuntime: true,
          maxFeePerGas: 5_000_000_000n,
          maxPriorityFeePerGas: 100_000_000n,
          noPrivateKey: true,
          recoveryChain: "31337",
          recoveryHash: transactionHash,
          recoveryMode: 0o600,
          recoveryNonce: transaction.nonce,
          recoverySponsor: deployer.address,
          signedHash: transactionHash,
          self: address,
          sourceKind: "git-commit",
          sourceValue: candidate.releaseCommit,
          sponsor: deployer.address,
        },
      );
    } finally {
      process.chdir(previousDirectory);
      provider?.destroy();
      if (anvil) await stopAnvil(anvil.process);
      if (worktreeAdded) {
        await runGitText(repositoryRoot, [
          "worktree",
          "remove",
          "--force",
          checkout,
        ]).catch(() => undefined);
      }
      await rm(temporaryDirectory, { force: true, recursive: true });
    }
  },
);
