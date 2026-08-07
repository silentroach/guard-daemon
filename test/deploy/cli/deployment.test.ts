import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { createServer } from "node:http";
import {
  mkdir,
  mkdtemp,
  readFile,
  realpath,
  rm,
  stat,
  symlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import test from "node:test";

import { Wallet } from "ethers";

import {
  loadCanonicalArtifact,
  loadReleaseCandidate,
  sourceProvenance,
} from "../../../scripts/deployment.js";
import {
  parseCLIOptions,
  runDeployment,
} from "../../../scripts/deployRescuerV2.js";
import { verifyCanonicalArtifact } from "../../../scripts/verifyArtifacts.js";

const repositoryRoot = resolve(".");
const destination = "0x1000000000000000000000000000000000000001";
const sponsor = "0x2000000000000000000000000000000000000002";
const releaseCommit = "0123456789abcdef0123456789abcdef01234567";
const artifactPath = resolve("artifacts/contracts/RescuerV2.json");
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

const sha256 = (value: Buffer): string =>
  createHash("sha256").update(value).digest("hex");

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
  const child = spawn(
    "git",
    [
      "-c",
      "core.attributesFile=/dev/null",
      "-c",
      "tar.umask=0002",
      "archive",
      "--format=tar",
      `--prefix=guard-daemon-${commit}/`,
      commit,
    ],
    { cwd, stdio: ["ignore", "pipe", "ignore"] },
  );
  const chunks: Buffer[] = [];
  child.stdout.on("data", (chunk: Buffer) => chunks.push(chunk));
  const code = await new Promise<number>((resolveExit, rejectExit) => {
    child.once("error", rejectExit);
    child.once("close", (value) => resolveExit(value ?? -1));
  });
  assert.equal(code, 0);
  return Buffer.concat(chunks);
};

const withCleanWorktree = async <T>(
  operation: (checkout: string, temporaryDirectory: string) => Promise<T>,
): Promise<T> => {
  const temporaryDirectory = await mkdtemp(
    join(tmpdir(), "guard-deploy-worktree-"),
  );
  const checkout = join(temporaryDirectory, "checkout");
  await runGitText(repositoryRoot, [
    "worktree",
    "add",
    "--detach",
    checkout,
    "HEAD",
  ]);
  try {
    return await operation(checkout, temporaryDirectory);
  } finally {
    await runGitText(repositoryRoot, [
      "worktree",
      "remove",
      "--force",
      checkout,
    ]).catch(() => undefined);
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
};

const writeReleaseCandidate = async (
  directory: string,
  checkout: string,
  revision = "HEAD",
): Promise<{ readonly path: string; readonly releaseCommit: string }> => {
  await mkdir(directory, { recursive: true });
  const commit = await runGitText(checkout, [
    "rev-parse",
    "--verify",
    `${revision}^{commit}`,
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
    return `sha256:${sha256(content)}`;
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
      return `${sha256(content)}  ${name}\n`;
    })
    .join("");
  await writeFile(join(directory, "SHA256SUMS"), checksums);
  return {
    path: join(directory, "release-candidate.json"),
    releaseCommit: commit,
  };
};

const broadcastArguments = (
  releaseCandidatePath: string,
  manifestPath: string,
  recoveryDirectory: string,
  rpcURL = "http://127.0.0.1:1/private-credential",
  sponsorAddress = sponsor,
): readonly string[] => [
  "--chain-id",
  "31337",
  "--destination",
  destination,
  "--sponsor",
  sponsorAddress,
  "--rpc-url",
  rpcURL,
  "--manifest",
  manifestPath,
  "--recovery-directory",
  recoveryDirectory,
  "--release-candidate",
  releaseCandidatePath,
  "--gas-limit",
  "1000000",
  "--max-fee-per-gas-wei",
  "2000000000",
  "--max-priority-fee-per-gas-wei",
  "100000000",
  "--max-total-cost-wei",
  "2000000000000000",
  "--broadcast",
];

const recoveryRecordPath = (
  directory: string,
  commit: string,
  sponsorAddress = sponsor,
): string =>
  join(
    directory,
    `rescuer-v2-31337-${sponsorAddress.toLowerCase()}-${commit}.json`,
  );

const writeRecoveryRecord = async (
  directory: string,
  commit: string,
  sponsorAddress = sponsor,
): Promise<string> => {
  const path = recoveryRecordPath(directory, commit, sponsorAddress);
  await writeFile(
    path,
    `${JSON.stringify({
      chainId: "31337",
      kind: "rescuer-v2-deployment",
      releaseCommit: commit,
      schemaVersion: "1",
      sponsor: sponsorAddress,
    })}\n`,
  );
  return path;
};

const writeMalformedCandidateDirectory = async (
  directory: string,
  candidateContent: string,
): Promise<string> => {
  await mkdir(directory);
  for (const name of candidateContentNames) {
    await writeFile(
      join(directory, name),
      name === "release-candidate.json" ? candidateContent : `${name}\n`,
    );
  }
  await writeFile(join(directory, "SHA256SUMS"), "invalid\n");
  return join(directory, "release-candidate.json");
};

test("режим по умолчанию не читает ключ и не обращается к RPC", async () => {
  const unreadEnvironment = new Proxy<NodeJS.ProcessEnv>(
    {},
    {
      get: () => assert.fail("Режим планирования прочитал environment"),
    },
  );
  const options = parseCLIOptions(
    [
      "--chain-id",
      "31337",
      "--destination",
      destination,
      "--sponsor",
      sponsor,
      "--rpc-url",
      "http://127.0.0.1:1/private-credential",
      "--release-candidate",
      "/path/that/must/not/be-read/release-candidate.json",
    ],
    unreadEnvironment,
  );

  await runDeployment(options, unreadEnvironment);
});

test("подменённый artifact отклоняется повторной pinned-сборкой", async () => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), "guard-artifact-"));
  const modifiedArtifactPath = join(temporaryDirectory, "RescuerV2.json");
  const canonical = await readFile(artifactPath, "utf8");
  await writeFile(
    modifiedArtifactPath,
    canonical.replace(
      '"compilerVersion": "0.8.36',
      '"compilerVersion": "0.8.35',
    ),
  );
  try {
    await assert.rejects(
      verifyCanonicalArtifact(modifiedArtifactPath),
      /pinned source\/compiler\/settings/,
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("source provenance различает source tree и проверенный commit", async () => {
  const loaded = await loadCanonicalArtifact(artifactPath);
  assert.deepStrictEqual(sourceProvenance(loaded.artifact), {
    kind: "source-tree-sha256",
    value: loaded.artifact.sourceTreeSha256,
  });
  assert.deepStrictEqual(sourceProvenance(loaded.artifact, releaseCommit), {
    kind: "git-commit",
    value: releaseCommit,
  });
  assert.throws(
    () => sourceProvenance(loaded.artifact, "ABCDEF"),
    /Release commit/,
  );
});

test("установленный production candidate не читает Git metadata", async () => {
  await withCleanWorktree(async (checkout, temporaryDirectory) => {
    const candidate = await writeReleaseCandidate(
      join(temporaryDirectory, "candidate"),
      checkout,
    );
    const loaded = await loadCanonicalArtifact(
      join(checkout, "artifacts/contracts/RescuerV2.json"),
    );
    const previousDirectory = process.cwd();
    try {
      process.chdir(temporaryDirectory);
      const installed = await loadReleaseCandidate(
        candidate.path,
        loaded,
        false,
      );
      assert.equal(installed.releaseCommit, candidate.releaseCommit);
      assert.equal(
        installed.repositoryRoot,
        await realpath(temporaryDirectory),
      );
    } finally {
      process.chdir(previousDirectory);
    }
  });
});

test("candidate JSON ограничен по размеру и глубине", async () => {
  const temporaryDirectory = await mkdtemp(
    join(tmpdir(), "guard-json-limits-"),
  );
  const loaded = await loadCanonicalArtifact(artifactPath);
  try {
    const oversized = await writeMalformedCandidateDirectory(
      join(temporaryDirectory, "oversized"),
      `{"padding":"${"a".repeat(1024 * 1024)}"}`,
    );
    await assert.rejects(
      loadReleaseCandidate(oversized, loaded),
      /превышает допустимый размер/,
    );

    const deeplyNested = await writeMalformedCandidateDirectory(
      join(temporaryDirectory, "deep"),
      `${'{"nested":'.repeat(9)}false${"}".repeat(9)}`,
    );
    await assert.rejects(
      loadReleaseCandidate(deeplyNested, loaded),
      /строгую проверку/,
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("CLI требует local recovery directory и полный набор broadcast аргументов", () => {
  assert.throws(() => parseCLIOptions([]), /--chain-id/);
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

  const complete = broadcastArguments(
    "candidate/release-candidate.json",
    "manifest.json",
    join(tmpdir(), "recovery"),
  );
  for (const required of [
    "--rpc-url",
    "--manifest",
    "--recovery-directory",
    "--release-candidate",
    "--gas-limit",
    "--max-fee-per-gas-wei",
    "--max-priority-fee-per-gas-wei",
    "--max-total-cost-wei",
  ]) {
    const missing: string[] = [];
    for (let index = 0; index < complete.length; index++) {
      if (complete[index] === required) {
        index++;
        continue;
      }
      const value = complete[index];
      if (value) {
        missing.push(value);
      }
    }
    assert.throws(() => parseCLIOptions(missing), new RegExp(required));
  }
});

test("CLI отделяет production RPC credentials и фиксирует recovery path", () => {
  const local = [
    ...broadcastArguments(
      "candidate/release-candidate.json",
      "manifest.json",
      join(tmpdir(), "recovery"),
    ),
  ];
  const legacy = [...local];
  legacy[legacy.indexOf("--recovery-directory")] = "--recovery-record";
  assert.throws(() => parseCLIOptions(legacy), /Неизвестный аргумент/);

  const production = local.filter(
    (value, index) =>
      value !== "--recovery-directory" &&
      local[index - 1] !== "--recovery-directory" &&
      value !== "--rpc-url" &&
      local[index - 1] !== "--rpc-url",
  );
  production[production.indexOf("--chain-id") + 1] = "1";
  assert.throws(
    () => parseCLIOptions(production, {}),
    /не задан DEPLOYMENT_RPC_URL/,
  );
  const options = parseCLIOptions(production, {
    DEPLOYMENT_RPC_URL: "https://operator:secret@rpc.example.invalid",
  });
  assert.equal(options.broadcast, true);
  if (!options.broadcast) {
    assert.fail("Ожидался режим broadcast");
  }
  assert.equal(
    options.recoveryDirectory,
    "/var/lib/guard-daemon-operator/deployment-recovery",
  );

  production.push("--recovery-directory", join(tmpdir(), "override"));
  assert.throws(
    () =>
      parseCLIOptions(production, {
        DEPLOYMENT_RPC_URL: "https://rpc.example.invalid",
      }),
    /разрешён только для локальной chain ID 31337/,
  );

  const productionWithRPCArgument = [...local];
  productionWithRPCArgument[
    productionWithRPCArgument.indexOf("--chain-id") + 1
  ] = "1";
  productionWithRPCArgument.splice(
    productionWithRPCArgument.indexOf("--recovery-directory"),
    2,
  );
  assert.throws(
    () =>
      parseCLIOptions(productionWithRPCArgument, {
        DEPLOYMENT_RPC_URL: "https://rpc.example.invalid",
      }),
    /только через DEPLOYMENT_RPC_URL, а не --rpc-url/,
  );
});

test("CLI ограничивает fee-model chain IDs до чтения ключа", () => {
  const args = [
    ...broadcastArguments(
      "candidate/release-candidate.json",
      "manifest.json",
      join(tmpdir(), "recovery"),
    ),
  ];
  args[args.indexOf("--chain-id") + 1] = "10";
  assert.throws(() => parseCLIOptions(args), /chain ID 1, 56, 137/);
});

test("CLI строго ограничивает decimal bigint и полную стоимость", () => {
  const replaceValue = (name: string, value: string): readonly string[] => {
    const args = [
      ...broadcastArguments(
        "candidate/release-candidate.json",
        "manifest.json",
        join(tmpdir(), "recovery"),
      ),
    ];
    const index = args.indexOf(name);
    assert.notEqual(index, -1);
    args[index + 1] = value;
    return args;
  };

  for (const [name, value] of [
    ["--gas-limit", "0"],
    ["--max-fee-per-gas-wei", "0"],
    ["--max-priority-fee-per-gas-wei", "-1"],
    ["--max-priority-fee-per-gas-wei", "00"],
    ["--max-total-cost-wei", "0"],
    ["--gas-limit", (1n << 256n).toString()],
  ] as const) {
    assert.throws(
      () => parseCLIOptions(replaceValue(name, value)),
      /decimal bigint|uint256/,
    );
  }
  assert.throws(
    () =>
      parseCLIOptions(
        replaceValue("--max-priority-fee-per-gas-wei", "2000000001"),
      ),
    /превышает --max-fee-per-gas-wei/,
  );
  assert.throws(
    () =>
      parseCLIOptions(replaceValue("--max-total-cost-wei", "1999999999999999")),
    /gasLimit \* maxFeePerGas/,
  );
});

test("legacy config arguments неизвестны", () => {
  for (const name of ["--config", "--config-key"]) {
    assert.throws(
      () =>
        parseCLIOptions([
          "--chain-id",
          "31337",
          "--destination",
          destination,
          "--sponsor",
          sponsor,
          name,
          "legacy",
        ]),
      /Неизвестный аргумент/,
    );
  }
});

test("существующие recovery record и manifest блокируют до RPC", async () => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), "guard-existing-"));
  const candidatePath = join(
    temporaryDirectory,
    "candidate",
    "release-candidate.json",
  );
  const manifestPath = join(temporaryDirectory, "manifest.json");
  const recoveryDirectory = join(temporaryDirectory, "recovery");
  await mkdir(recoveryDirectory, { mode: 0o700 });
  try {
    const existingRecord = await writeRecoveryRecord(
      recoveryDirectory,
      releaseCommit,
    );
    await assert.rejects(
      runDeployment(
        parseCLIOptions(
          broadcastArguments(candidatePath, manifestPath, recoveryDirectory),
        ),
        {},
      ),
      /Незавершённый recovery.*chain и sponsor/,
    );
    await rm(existingRecord);
    await writeFile(manifestPath, "manifest-before\n");
    await assert.rejects(
      runDeployment(
        parseCLIOptions(
          broadcastArguments(candidatePath, manifestPath, recoveryDirectory),
        ),
        {},
      ),
      /Manifest уже существует/,
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("local recovery registry обязан быть preexisting каталогом 0700 без symlink", async () => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), "guard-registry-"));
  const candidatePath = join(temporaryDirectory, "release-candidate.json");
  const manifestPath = join(temporaryDirectory, "manifest.json");
  const permissive = join(temporaryDirectory, "permissive");
  const registryLink = join(temporaryDirectory, "registry-link");
  await mkdir(permissive, { mode: 0o755 });
  await symlink(permissive, registryLink);
  try {
    await assert.rejects(
      runDeployment(
        parseCLIOptions(
          broadcastArguments(candidatePath, manifestPath, permissive),
        ),
        {},
      ),
      /режим 0700/,
    );
    await assert.rejects(
      runDeployment(
        parseCLIOptions(
          broadcastArguments(candidatePath, manifestPath, registryLink),
        ),
        {},
      ),
      /обычным каталогом, а не символической ссылкой/,
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("самосогласованный candidate чужого commit отклоняется без RPC", async () => {
  await withCleanWorktree(async (checkout, temporaryDirectory) => {
    const candidate = await writeReleaseCandidate(
      join(temporaryDirectory, "foreign-candidate"),
      checkout,
      "HEAD^",
    );
    const manifestPath = join(temporaryDirectory, "manifest.json");
    const recoveryDirectory = join(temporaryDirectory, "recovery");
    await mkdir(recoveryDirectory, { mode: 0o700 });
    let requestCount = 0;
    const server = createServer((_request, response) => {
      requestCount++;
      response.statusCode = 500;
      response.end();
    });
    await new Promise<void>((resolveListen) =>
      server.listen(0, "127.0.0.1", resolveListen),
    );
    const address = server.address();
    if (!address || typeof address === "string") {
      assert.fail("Тестовый RPC не получил TCP port");
    }
    const previousDirectory = process.cwd();
    try {
      process.chdir(checkout);
      await assert.rejects(
        runDeployment(
          parseCLIOptions(
            broadcastArguments(
              candidate.path,
              manifestPath,
              recoveryDirectory,
              `http://127.0.0.1:${address.port}`,
            ),
          ),
          {},
        ),
        /HEAD чистого локального checkout/,
      );
      assert.equal(requestCount, 0);
    } finally {
      process.chdir(previousDirectory);
      server.closeAllConnections();
      await new Promise<void>((resolveClose, rejectClose) =>
        server.close((error) => (error ? rejectClose(error) : resolveClose())),
      );
    }
  });
});

test("manifest внутри candidate или Git worktree блокируется до RPC", async () => {
  await withCleanWorktree(async (checkout, temporaryDirectory) => {
    const candidate = await writeReleaseCandidate(
      join(temporaryDirectory, "candidate"),
      checkout,
    );
    const recoveryDirectory = join(temporaryDirectory, "recovery");
    await mkdir(recoveryDirectory, { mode: 0o700 });
    const previousDirectory = process.cwd();
    try {
      process.chdir(checkout);
      for (const manifestPath of [
        join(dirname(candidate.path), "manifest.json"),
        join(checkout, "manifest.json"),
      ]) {
        await assert.rejects(
          runDeployment(
            parseCLIOptions(
              broadcastArguments(
                candidate.path,
                manifestPath,
                recoveryDirectory,
              ),
            ),
            {},
          ),
          /должны находиться вне candidate и Git worktree/,
        );
      }
    } finally {
      process.chdir(previousDirectory);
    }
  });
});

test("recovery registry внутри Git worktree блокируется до RPC", async () => {
  await withCleanWorktree(async (checkout, temporaryDirectory) => {
    const candidate = await writeReleaseCandidate(
      join(temporaryDirectory, "candidate"),
      checkout,
    );
    const recoveryDirectory = join(checkout, "dist", "recovery");
    await mkdir(recoveryDirectory, { mode: 0o700, recursive: true });
    const previousDirectory = process.cwd();
    try {
      process.chdir(checkout);
      await assert.rejects(
        runDeployment(
          parseCLIOptions(
            broadcastArguments(
              candidate.path,
              join(temporaryDirectory, "manifest.json"),
              recoveryDirectory,
            ),
          ),
          {},
        ),
        /должны находиться вне candidate и Git worktree/,
      );
    } finally {
      process.chdir(previousDirectory);
    }
  });
});

test("candidate verifier отклоняет hidden info/attributes и symlink", async () => {
  await withCleanWorktree(async (checkout, temporaryDirectory) => {
    const candidate = await writeReleaseCandidate(
      join(temporaryDirectory, "candidate"),
      checkout,
    );
    const recoveryDirectory = join(temporaryDirectory, "recovery");
    await mkdir(recoveryDirectory, { mode: 0o700 });
    const gitDirectory = await runGitText(checkout, [
      "rev-parse",
      "--absolute-git-dir",
    ]);
    const infoDirectory = join(gitDirectory, "info");
    const attributesPath = join(infoDirectory, "attributes");
    await mkdir(infoDirectory, { recursive: true });
    const previousDirectory = process.cwd();
    try {
      process.chdir(checkout);
      for (const kind of ["file", "symlink"] as const) {
        if (kind === "file") {
          await writeFile(attributesPath, "* export-ignore\n");
        } else {
          await symlink("/dev/null", attributesPath);
        }
        await assert.rejects(
          runDeployment(
            parseCLIOptions(
              broadcastArguments(
                candidate.path,
                join(temporaryDirectory, "manifest.json"),
                recoveryDirectory,
              ),
            ),
            {},
          ),
          /info\/attributes запрещён/,
        );
        await rm(attributesPath);
      }
    } finally {
      process.chdir(previousDirectory);
      await rm(attributesPath, { force: true });
    }
  });
});

test("candidate verifier отклоняет hidden Git index flags до RPC", async () => {
  await withCleanWorktree(async (checkout, temporaryDirectory) => {
    const recoveryDirectory = join(temporaryDirectory, "recovery");
    const manifestPath = join(temporaryDirectory, "manifest.json");
    const trackedPath = join(checkout, "LICENSE");
    const trackedContent = await readFile(trackedPath);
    await mkdir(recoveryDirectory, { mode: 0o700 });
    let requestCount = 0;
    const server = createServer((_request, response) => {
      requestCount++;
      response.statusCode = 500;
      response.end();
    });
    await new Promise<void>((resolveListen) =>
      server.listen(0, "127.0.0.1", resolveListen),
    );
    const address = server.address();
    if (!address || typeof address === "string") {
      assert.fail("Тестовый RPC не получил TCP port");
    }
    const previousDirectory = process.cwd();
    try {
      process.chdir(checkout);
      for (const flag of ["--assume-unchanged", "--skip-worktree"] as const) {
        await runGitText(checkout, ["update-index", flag, "--", "LICENSE"]);
        await writeFile(
          trackedPath,
          Buffer.concat([trackedContent, Buffer.from(`candidate ${flag}\n`)]),
        );
        const candidate = await writeReleaseCandidate(
          join(temporaryDirectory, `candidate-${flag.slice(2)}`),
          checkout,
        );
        await assert.rejects(
          runDeployment(
            parseCLIOptions(
              broadcastArguments(
                candidate.path,
                manifestPath,
                recoveryDirectory,
                `http://127.0.0.1:${address.port}`,
              ),
            ),
            {},
          ),
          /assume-unchanged или skip-worktree/,
        );
        assert.equal(requestCount, 0);
        await runGitText(checkout, [
          "update-index",
          "--no-assume-unchanged",
          "--no-skip-worktree",
          "--",
          "LICENSE",
        ]);
        await writeFile(trackedPath, trackedContent);
      }
    } finally {
      process.chdir(previousDirectory);
      await runGitText(checkout, [
        "update-index",
        "--no-assume-unchanged",
        "--no-skip-worktree",
        "--",
        "LICENSE",
      ]).catch(() => undefined);
      await writeFile(trackedPath, trackedContent).catch(() => undefined);
      server.closeAllConnections();
      await new Promise<void>((resolveClose, rejectClose) =>
        server.close((error) => (error ? rejectClose(error) : resolveClose())),
      );
    }
  });
});

test("recovery record сохраняется до broadcast и остаётся при RPC error", async () => {
  await withCleanWorktree(async (checkout, temporaryDirectory) => {
    const candidate = await writeReleaseCandidate(
      join(temporaryDirectory, "candidate"),
      checkout,
    );
    const manifestPath = join(temporaryDirectory, "manifest.json");
    const recoveryDirectory = join(temporaryDirectory, "recovery");
    await mkdir(recoveryDirectory, { mode: 0o700 });
    const privateKey = `0x${sha256(Buffer.from("deployment-rpc-error"))}`;
    const deployer = new Wallet(privateKey);
    const recoveryPath = recoveryRecordPath(
      recoveryDirectory,
      candidate.releaseCommit,
      deployer.address,
    );
    let observedRecord: Readonly<Record<string, unknown>> | undefined;
    let resolveObserved: () => void = () => undefined;
    let rejectObserved: (error: unknown) => void = () => undefined;
    const observed = new Promise<void>((resolveValue, rejectValue) => {
      resolveObserved = resolveValue;
      rejectObserved = rejectValue;
    });
    const server = createServer((request, response) => {
      const chunks: Buffer[] = [];
      request.on("data", (chunk: Buffer) => chunks.push(chunk));
      request.on("end", () => {
        void (async () => {
          const payload: unknown = JSON.parse(
            Buffer.concat(chunks).toString("utf8"),
          );
          const answer = async (
            item: unknown,
          ): Promise<Readonly<Record<string, unknown>>> => {
            if (!item || typeof item !== "object" || Array.isArray(item)) {
              assert.fail("Тестовый RPC получил недопустимый запрос");
            }
            const id = Reflect.get(item, "id");
            const method = Reflect.get(item, "method");
            if (typeof id !== "number" || typeof method !== "string") {
              assert.fail("Тестовый RPC получил недопустимый запрос");
            }
            if (method === "eth_sendRawTransaction") {
              const recordValue: unknown = JSON.parse(
                await readFile(recoveryPath, "utf8"),
              );
              if (!isRecord(recordValue)) {
                assert.fail("Recovery record не является object");
              }
              observedRecord = recordValue;
              resolveObserved();
              return {
                error: { code: -32000, message: "test rejection" },
                id,
                jsonrpc: "2.0",
              };
            }
            return {
              id,
              jsonrpc: "2.0",
              result: method === "eth_chainId" ? "0x7a69" : "0x0",
            };
          };
          const result = Array.isArray(payload)
            ? await Promise.all(payload.map(answer))
            : await answer(payload);
          response.setHeader("content-type", "application/json");
          response.end(JSON.stringify(result));
        })().catch((error: unknown) => {
          rejectObserved(error);
          response.statusCode = 500;
          response.end();
        });
      });
    });
    await new Promise<void>((resolveListen) =>
      server.listen(0, "127.0.0.1", resolveListen),
    );
    const address = server.address();
    if (!address || typeof address === "string") {
      assert.fail("Тестовый RPC не получил TCP port");
    }
    const previousDirectory = process.cwd();
    try {
      process.chdir(checkout);
      await assert.rejects(
        runDeployment(
          parseCLIOptions(
            broadcastArguments(
              candidate.path,
              manifestPath,
              recoveryDirectory,
              `http://127.0.0.1:${address.port}`,
              deployer.address,
            ),
          ),
          { DEPLOYER_PRIVATE_KEY: privateKey },
        ),
      );
      await observed;
      const recoveryMode = (await stat(recoveryPath)).mode & 0o777;
      const transaction = Reflect.get(observedRecord ?? {}, "transaction");
      assert.deepStrictEqual(
        {
          chainId: Reflect.get(observedRecord ?? {}, "chainId"),
          manifestAbsent: await stat(manifestPath)
            .then(() => false)
            .catch(() => true),
          mode: recoveryMode,
          rawSignedTransaction:
            transaction && typeof transaction === "object"
              ? typeof Reflect.get(transaction, "rawSignedTransaction")
              : "missing",
          releaseCommit: Reflect.get(observedRecord ?? {}, "releaseCommit"),
          sponsor: Reflect.get(observedRecord ?? {}, "sponsor"),
        },
        {
          chainId: "31337",
          manifestAbsent: true,
          mode: 0o600,
          rawSignedTransaction: "string",
          releaseCommit: candidate.releaseCommit,
          sponsor: deployer.address,
        },
      );
    } finally {
      process.chdir(previousDirectory);
      server.closeAllConnections();
      await new Promise<void>((resolveClose, rejectClose) =>
        server.close((error) => (error ? rejectClose(error) : resolveClose())),
      );
    }
  });
});
