import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { spawn, type ChildProcess } from "node:child_process";
import { createServer } from "node:net";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

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
    assert.fail("Не удалось выделить локальный TCP port");
  }
  await new Promise<void>((resolveClose, reject) =>
    server.close((error) => (error ? reject(error) : resolveClose())),
  );
  return address.port;
};

const waitForAnvil = async (
  rpcURL: string,
  process: ChildProcess,
): Promise<void> => {
  for (let attempt = 0; attempt < 100; attempt++) {
    if (process.exitCode !== null) {
      throw new Error(
        `Anvil завершился до запуска с кодом ${process.exitCode}`,
      );
    }
    try {
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
      });
      if (response.ok) {
        return;
      }
    } catch {
      await new Promise((resolveWait) => setTimeout(resolveWait, 50));
    }
  }
  throw new Error("Истёк таймаут запуска Anvil");
};

test(
  "локальный deployment проверяет runtime и публикует manifest",
  { timeout: 30_000 },
  async () => {
    const temporaryDirectory = await mkdtemp(
      join(tmpdir(), "guard-deploy-integration-"),
    );
    const port = await freePort();
    const rpcURL = `http://127.0.0.1:${port}`;
    const deployerKey = deterministicPrivateKey("deployer");
    const deployer = new Wallet(deployerKey);
    const destination = new Wallet(deterministicPrivateKey("destination"))
      .address;
    const anvil = spawn(
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
      { stdio: ["ignore", "pipe", "pipe"] },
    );
    let provider: JsonRpcProvider | undefined;
    const manifestPath = join(temporaryDirectory, "manifest.json");
    const configPath = join(temporaryDirectory, "operator.env");
    await writeFile(configPath, "RESCUER_LOCAL=\n");

    try {
      await waitForAnvil(rpcURL, anvil);
      provider = new JsonRpcProvider(rpcURL);
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
          rpcURL,
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
          rpcURL,
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
      await provider?.destroy();
      anvil.kill("SIGTERM");
      await new Promise<void>((resolveExit) => {
        if (anvil.exitCode !== null) {
          resolveExit();
          return;
        }
        anvil.once("exit", () => resolveExit());
      });
      await rm(temporaryDirectory, { force: true, recursive: true });
    }
  },
);
