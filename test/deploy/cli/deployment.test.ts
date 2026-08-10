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

test("default mode does not read the key or contact RPC", async () => {
  const unreadEnvironment = new Proxy<NodeJS.ProcessEnv>(
    {},
    {
      get: () => assert.fail("Planning mode read the environment"),
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

test("a tampered artifact is rejected by recompilation with pinned settings", async () => {
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
      /pinned source, compiler, and settings/,
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("source metadata distinguishes the source tree from the verified commit", async () => {
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

test("an installed production candidate does not read Git metadata", async () => {
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

test("candidate JSON is bounded by size and depth", async () => {
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
      /exceeds the allowed JSON size/,
    );

    const deeplyNested = await writeMalformedCandidateDirectory(
      join(temporaryDirectory, "deep"),
      `${'{"nested":'.repeat(9)}false${"}".repeat(9)}`,
    );
    await assert.rejects(
      loadReleaseCandidate(deeplyNested, loaded),
      /failed strict validation/,
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("CLI requires a local recovery directory and complete broadcast arguments", () => {
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

test("CLI isolates production RPC credentials and pins the recovery path", () => {
  const local = [
    ...broadcastArguments(
      "candidate/release-candidate.json",
      "manifest.json",
      join(tmpdir(), "recovery"),
    ),
  ];
  const legacy = [...local];
  legacy[legacy.indexOf("--recovery-directory")] = "--recovery-record";
  assert.throws(() => parseCLIOptions(legacy), /Unknown argument/);

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
    /DEPLOYMENT_RPC_URL is not set/,
  );
  const options = parseCLIOptions(production, {
    DEPLOYMENT_RPC_URL: "https://operator:secret@rpc.example.invalid",
  });
  assert.equal(options.broadcast, true);
  if (!options.broadcast) {
    assert.fail("Expected broadcast mode");
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
    /allowed only for local chain ID 31337/,
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
    /through DEPLOYMENT_RPC_URL, not --rpc-url/,
  );
});

test("CLI restricts chain IDs by fee model before reading the key", () => {
  const args = [
    ...broadcastArguments(
      "candidate/release-candidate.json",
      "manifest.json",
      join(tmpdir(), "recovery"),
    ),
  ];
  args[args.indexOf("--chain-id") + 1] = "10";
  assert.throws(() => parseCLIOptions(args), /chain IDs 1, 56, 137/);
});

test("CLI strictly bounds decimal bigints and total cost", () => {
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
    /exceeds --max-fee-per-gas-wei/,
  );
  assert.throws(
    () =>
      parseCLIOptions(replaceValue("--max-total-cost-wei", "1999999999999999")),
    /gasLimit \* maxFeePerGas/,
  );
});

test("legacy configuration arguments are unknown", () => {
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
      /Unknown argument/,
    );
  }
});

test("an existing recovery record and manifest block execution before RPC", async () => {
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
      /Incomplete recovery.*chain and sponsor/,
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
      /Manifest already exists/,
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("local recovery registry must be a pre-created 0700 directory without symbolic links", async () => {
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
      /mode 0700/,
    );
    await assert.rejects(
      runDeployment(
        parseCLIOptions(
          broadcastArguments(candidatePath, manifestPath, registryLink),
        ),
        {},
      ),
      /regular directory, not a symbolic link/,
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("a self-consistent candidate from another commit is rejected without RPC", async () => {
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
      assert.fail("Test RPC did not receive a TCP port");
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
        /HEAD of a clean local checkout/,
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

test("a manifest inside the candidate directory or Git worktree is blocked before RPC", async () => {
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
          /must be outside the candidate and Git worktree/,
        );
      }
    } finally {
      process.chdir(previousDirectory);
    }
  });
});

test("a recovery registry inside the Git worktree is blocked before RPC", async () => {
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
        /must be outside the candidate and Git worktree/,
      );
    } finally {
      process.chdir(previousDirectory);
    }
  });
});

test("candidate verification rejects a hidden info/attributes file and symbolic link", async () => {
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
          /info\/attributes file is forbidden/,
        );
        await rm(attributesPath);
      }
    } finally {
      process.chdir(previousDirectory);
      await rm(attributesPath, { force: true });
    }
  });
});

test("candidate verification rejects hidden Git index flags before RPC", async () => {
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
      assert.fail("Test RPC did not receive a TCP port");
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
          /assume-unchanged or skip-worktree/,
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

test("the recovery record is persisted before broadcast and retained after an RPC error", async () => {
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
              assert.fail("Test RPC received an invalid request");
            }
            const id = Reflect.get(item, "id");
            const method = Reflect.get(item, "method");
            if (typeof id !== "number" || typeof method !== "string") {
              assert.fail("Test RPC received an invalid request");
            }
            if (method === "eth_sendRawTransaction") {
              const recordValue: unknown = JSON.parse(
                await readFile(recoveryPath, "utf8"),
              );
              if (!isRecord(recordValue)) {
                assert.fail("Recovery record is not an object");
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
      assert.fail("Test RPC did not receive a TCP port");
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
