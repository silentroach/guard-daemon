import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { spawn, type ChildProcess } from "node:child_process";
import { createServer } from "node:net";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { setTimeout as delay } from "node:timers/promises";

import { JsonRpcProvider, Wallet } from "ethers";

import {
  parseCLIOptions,
  runDeployment,
} from "../../../scripts/deployRescuerV2.js";

const deterministicPrivateKey = (label: string): string =>
  `0x${createHash("sha256").update(`guard-daemon-test:${label}`).digest("hex")}`;

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
  "локальное развёртывание проверяет runtime и публикует манифест",
  { timeout: 30_000 },
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
    const manifestPath = join(temporaryDirectory, "manifest.json");
    const configPath = join(temporaryDirectory, "operator.env");

    try {
      anvil = await startAnvil();
      await writeFile(configPath, "RESCUER_LOCAL=\n");
      provider = new JsonRpcProvider(anvil.rpcURL);
      await provider.send("anvil_setBalance", [
        deployer.address,
        "0x56bc75e2d63100000",
      ]);
      const beforePlan = await provider.getBlockNumber();
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
          "--config",
          configPath,
          "--config-key",
          "RESCUER_LOCAL",
          "--broadcast",
        ]),
        { DEPLOYER_PRIVATE_KEY: deployerKey },
      );

      const manifestText = await readFile(manifestPath, "utf8");
      const manifest = JSON.parse(manifestText) as {
        readonly address: string;
        readonly chainId: string;
        readonly immutables: {
          readonly destination: string;
          readonly self: string;
          readonly sponsor: string;
        };
        readonly runtime: { readonly keccak256: string };
      };
      const config = await readFile(configPath, "utf8");
      const code = await provider.getCode(manifest.address);
      assert.deepStrictEqual(
        {
          chain: manifest.chainId,
          configPublished: config === `RESCUER_LOCAL=${manifest.address}\n`,
          destination: manifest.immutables.destination,
          hasRuntime:
            code !== "0x" && manifest.runtime.keccak256.startsWith("0x"),
          noPrivateKey: !manifestText.includes(deployerKey.slice(2)),
          self: manifest.immutables.self,
          sponsor: manifest.immutables.sponsor,
        },
        {
          chain: "31337",
          configPublished: true,
          destination,
          hasRuntime: true,
          noPrivateKey: true,
          self: manifest.address,
          sponsor: deployer.address,
        },
      );
    } finally {
      provider?.destroy();
      if (anvil) await stopAnvil(anvil.process);
      await rm(temporaryDirectory, { force: true, recursive: true });
    }
  },
);
