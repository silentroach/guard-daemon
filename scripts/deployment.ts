import { createHash } from "node:crypto";
import { execFile, spawn } from "node:child_process";
import { lstat, open, readFile, readdir, realpath } from "node:fs/promises";
import { basename, dirname, resolve } from "node:path";

import {
  getAddress,
  getBytes,
  keccak256,
  type JsonFragment,
  type JsonFragmentType,
} from "ethers";

export interface ImmutableReference {
  readonly length: number;
  readonly start: number;
}

export interface CanonicalArtifact {
  readonly abi: readonly JsonFragment[];
  readonly artifactVersion: "1";
  readonly bytecode: string;
  readonly compilerInputSha256: string;
  readonly compilerVersion: string;
  readonly contractName: "RescuerV2";
  readonly deployedBytecode: string;
  readonly immutableReferences: Readonly<
    Record<"destination" | "self" | "sponsor", readonly ImmutableReference[]>
  >;
  readonly settings: Readonly<Record<string, unknown>>;
  readonly sourceName: "RescuerV2.sol";
  readonly sourceTreeSha256: string;
}

export interface LoadedArtifact {
  readonly artifact: CanonicalArtifact;
  readonly sha256: string;
}

export interface ReleaseCandidate {
  readonly artifactSha256: string;
  readonly releaseCommit: string;
  readonly releaseTree: string;
  readonly repositoryRoot: string;
}

export interface ImmutableValues {
  readonly destination: string;
  readonly self: string;
  readonly sponsor: string;
}

export interface SourceProvenance {
  readonly kind: "git-commit" | "source-tree-sha256";
  readonly value: string;
}

export interface DeploymentManifest {
  readonly address: string;
  readonly artifact: { readonly sha256: string };
  readonly chainId: string;
  readonly compiler: {
    readonly settings: Readonly<Record<string, unknown>>;
    readonly version: string;
  };
  readonly constructorArguments: {
    readonly destination: string;
    readonly sponsor: string;
  };
  readonly contractRole: "rescuer";
  readonly deploymentBlockHash: string;
  readonly deploymentBlockNumber: string;
  readonly deploymentTransactionHash: string;
  readonly immutables: ImmutableValues;
  readonly runtime: {
    readonly byteLength: number;
    readonly keccak256: string;
  };
  readonly schemaVersion: "1";
  readonly source: SourceProvenance;
}

const sha256Pattern = /^sha256:[0-9a-f]{64}$/;
const releaseCommitPattern = /^[0-9a-f]{40}$/;
const bytecodePattern = /^0x(?:[0-9a-f]{2})+$/;
const immutableNames = ["destination", "self", "sponsor"] as const;
const maximumCandidateJSONBytes = 1024 * 1024;
const maximumCandidateJSONDepth = 8;

export class DeploymentInputError extends Error {}

const sha256 = (value: Buffer): string =>
  `sha256:${createHash("sha256").update(value).digest("hex")}`;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const isSafeInteger = (value: unknown): value is number =>
  typeof value === "number" && Number.isSafeInteger(value);

const isOptionalString = (value: unknown): value is string | undefined =>
  value === undefined || typeof value === "string";

const isOptionalBoolean = (value: unknown): value is boolean | undefined =>
  value === undefined || typeof value === "boolean";

const isJsonFragmentType = (value: unknown): value is JsonFragmentType => {
  if (!isRecord(value)) {
    return false;
  }
  const components = value["components"];
  return (
    isOptionalString(value["name"]) &&
    isOptionalBoolean(value["indexed"]) &&
    isOptionalString(value["type"]) &&
    isOptionalString(value["internalType"]) &&
    (components === undefined ||
      (Array.isArray(components) && components.every(isJsonFragmentType)))
  );
};

const isJsonFragment = (value: unknown): value is JsonFragment => {
  if (!isRecord(value)) {
    return false;
  }
  const inputs = value["inputs"];
  const outputs = value["outputs"];
  return (
    isOptionalString(value["name"]) &&
    isOptionalString(value["type"]) &&
    isOptionalBoolean(value["anonymous"]) &&
    isOptionalBoolean(value["payable"]) &&
    isOptionalBoolean(value["constant"]) &&
    isOptionalString(value["stateMutability"]) &&
    isOptionalString(value["gas"]) &&
    (inputs === undefined ||
      (Array.isArray(inputs) && inputs.every(isJsonFragmentType))) &&
    (outputs === undefined ||
      (Array.isArray(outputs) && outputs.every(isJsonFragmentType)))
  );
};

const requireString = (
  record: Record<string, unknown>,
  name: string,
): string => {
  const value = record[name];
  if (typeof value !== "string" || !value) {
    throw new DeploymentInputError(`Artifact field ${name} is missing`);
  }
  return value;
};

const requireExactKeys = (
  record: Record<string, unknown>,
  expected: readonly string[],
  subject: string,
): void => {
  const actual = Object.keys(record).sort();
  const wanted = [...expected].sort();
  if (
    actual.length !== wanted.length ||
    actual.some((key, i) => key !== wanted[i])
  ) {
    throw new DeploymentInputError(`${subject} contains unexpected fields`);
  }
};

const parseReferences = (
  value: unknown,
): CanonicalArtifact["immutableReferences"] => {
  if (!isRecord(value)) {
    throw new DeploymentInputError(
      "Artifact does not contain immutable references",
    );
  }
  requireExactKeys(value, immutableNames, "Immutable references");
  const result: Record<string, readonly ImmutableReference[]> = {};
  for (const name of immutableNames) {
    const entries = value[name];
    if (!Array.isArray(entries) || entries.length === 0) {
      throw new DeploymentInputError(`Immutable reference ${name} is missing`);
    }
    result[name] = entries.map((entry) => {
      if (!isRecord(entry)) {
        throw new DeploymentInputError(
          `Immutable reference ${name} is corrupted`,
        );
      }
      requireExactKeys(
        entry,
        ["length", "start"],
        `Immutable reference ${name}`,
      );
      const length = entry["length"];
      const start = entry["start"];
      if (!isSafeInteger(start) || !isSafeInteger(length)) {
        throw new DeploymentInputError(
          `Immutable reference ${name} is corrupted`,
        );
      }
      return { length, start };
    });
  }
  return {
    destination: result["destination"] ?? [],
    self: result["self"] ?? [],
    sponsor: result["sponsor"] ?? [],
  };
};

const parseArtifact = (value: unknown): CanonicalArtifact => {
  if (!isRecord(value)) {
    throw new DeploymentInputError("Canonical artifact is not a JSON object");
  }
  requireExactKeys(
    value,
    [
      "abi",
      "artifactVersion",
      "bytecode",
      "compilerInputSha256",
      "compilerVersion",
      "contractName",
      "deployedBytecode",
      "immutableReferences",
      "settings",
      "sourceName",
      "sourceTreeSha256",
    ],
    "Canonical artifact",
  );
  const artifactVersion = requireString(value, "artifactVersion");
  const contractName = requireString(value, "contractName");
  const sourceName = requireString(value, "sourceName");
  const bytecode = requireString(value, "bytecode");
  const deployedBytecode = requireString(value, "deployedBytecode");
  const compilerInputSha256 = requireString(value, "compilerInputSha256");
  const sourceTreeSha256 = requireString(value, "sourceTreeSha256");
  const abi = value["abi"];
  if (
    artifactVersion !== "1" ||
    contractName !== "RescuerV2" ||
    sourceName !== "RescuerV2.sol" ||
    !bytecodePattern.test(bytecode) ||
    !bytecodePattern.test(deployedBytecode) ||
    !sha256Pattern.test(compilerInputSha256) ||
    !sha256Pattern.test(sourceTreeSha256) ||
    !Array.isArray(abi) ||
    !abi.every(isJsonFragment) ||
    !isRecord(value["settings"])
  ) {
    throw new DeploymentInputError(
      "Canonical artifact failed strict validation",
    );
  }
  return {
    abi,
    artifactVersion: "1",
    bytecode,
    compilerInputSha256,
    compilerVersion: requireString(value, "compilerVersion"),
    contractName: "RescuerV2",
    deployedBytecode,
    immutableReferences: parseReferences(value["immutableReferences"]),
    settings: value["settings"],
    sourceName: "RescuerV2.sol",
    sourceTreeSha256,
  };
};

export const loadCanonicalArtifact = async (
  path: string,
): Promise<LoadedArtifact> => {
  const content = await readFile(path);
  let parsed: unknown;
  try {
    parsed = JSON.parse(content.toString("utf8"));
  } catch {
    throw new DeploymentInputError("Canonical artifact contains invalid JSON");
  }
  return { artifact: parseArtifact(parsed), sha256: sha256(content) };
};

type CandidateJSONValue = boolean | string | CandidateJSONObject;
type CandidateJSONObject = ReadonlyMap<string, CandidateJSONValue>;

class CandidateJSONParser {
  private index = 0;

  public constructor(private readonly input: string) {}

  public parse(): CandidateJSONObject {
    this.skipWhitespace();
    const result = this.parseObject(1);
    this.skipWhitespace();
    if (this.index !== this.input.length) {
      this.invalid();
    }
    return result;
  }

  private current(): string | undefined {
    return this.input[this.index];
  }

  private expect(expected: string): void {
    if (this.current() !== expected) {
      this.invalid();
    }
    this.index++;
  }

  private invalid(): never {
    throw new DeploymentInputError(
      "Release candidate failed strict validation",
    );
  }

  private parseObject(depth: number): CandidateJSONObject {
    if (depth > maximumCandidateJSONDepth) {
      this.invalid();
    }
    this.expect("{");
    this.skipWhitespace();
    const result = new Map<string, CandidateJSONValue>();
    if (this.current() === "}") {
      this.index++;
      return result;
    }
    while (true) {
      const key = this.parseString();
      if (result.has(key)) {
        this.invalid();
      }
      this.skipWhitespace();
      this.expect(":");
      this.skipWhitespace();
      const value = this.parseValue(depth);
      result.set(key, value);
      this.skipWhitespace();
      if (this.current() === "}") {
        this.index++;
        return result;
      }
      this.expect(",");
      this.skipWhitespace();
    }
  }

  private parseValue(depth: number): CandidateJSONValue {
    if (this.current() === "{") {
      return this.parseObject(depth + 1);
    }
    if (this.current() === '"') {
      return this.parseString();
    }
    if (this.input.startsWith("false", this.index)) {
      this.index += "false".length;
      return false;
    }
    return this.invalid();
  }

  private parseString(): string {
    this.expect('"');
    const start = this.index;
    while (this.index < this.input.length) {
      const value = this.input[this.index];
      if (value === '"') {
        const result = this.input.slice(start, this.index);
        this.index++;
        return result;
      }
      if (!value || value === "\\" || value.charCodeAt(0) < 0x20) {
        this.invalid();
      }
      this.index++;
    }
    return this.invalid();
  }

  private skipWhitespace(): void {
    while (
      this.current() === " " ||
      this.current() === "\n" ||
      this.current() === "\r" ||
      this.current() === "\t"
    ) {
      this.index++;
    }
  }
}

const requireCandidateKeys = (
  value: CandidateJSONObject,
  expected: readonly string[],
): void => {
  const actual = [...value.keys()].sort();
  const wanted = [...expected].sort();
  if (
    actual.length !== wanted.length ||
    actual.some((key, index) => key !== wanted[index])
  ) {
    throw new DeploymentInputError(
      "Release candidate failed strict validation",
    );
  }
};

const requireCandidateString = (
  value: CandidateJSONObject,
  name: string,
): string => {
  const field = value.get(name);
  if (typeof field !== "string") {
    throw new DeploymentInputError(
      "Release candidate failed strict validation",
    );
  }
  return field;
};

const requireCandidateObject = (
  value: CandidateJSONObject,
  name: string,
): CandidateJSONObject => {
  const field = value.get(name);
  if (!(field instanceof Map)) {
    throw new DeploymentInputError(
      "Release candidate failed strict validation",
    );
  }
  return field;
};

const requireCandidateBoolean = (
  value: CandidateJSONObject,
  name: string,
): boolean => {
  const field = value.get(name);
  if (typeof field !== "boolean") {
    throw new DeploymentInputError(
      "Release candidate failed strict validation",
    );
  }
  return field;
};

const candidateArtifactPaths = {
  binary: "guard-daemon-linux-amd64",
  contract: "RescuerV2.json",
  manifestSchema: "rescuer-manifest.schema.json",
  sbom: "guard-daemon.cdx.json",
  sourceArchive: "guard-daemon-source.tar",
} as const;

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

const candidateOutputNames = [...candidateContentNames, "SHA256SUMS"].sort();

const requireRegularCandidateFile = async (path: string): Promise<void> => {
  let metadata;
  try {
    metadata = await lstat(path);
  } catch {
    throw new DeploymentInputError(
      "Release candidate does not contain a required regular file",
    );
  }
  if (metadata.isSymbolicLink() || !metadata.isFile()) {
    throw new DeploymentInputError(
      "Release candidate does not contain a required regular file",
    );
  }
};

const requireCandidateDirectory = async (path: string): Promise<void> => {
  let metadata;
  try {
    metadata = await lstat(path);
  } catch {
    throw new DeploymentInputError(
      "Release candidate must reside in the complete candidate directory",
    );
  }
  if (metadata.isSymbolicLink() || !metadata.isDirectory()) {
    throw new DeploymentInputError(
      "Release candidate must reside in the complete candidate directory",
    );
  }
};

const gitEnvironment = (): NodeJS.ProcessEnv => {
  const environment: NodeJS.ProcessEnv = {};
  for (const [name, value] of Object.entries(process.env)) {
    if (value !== undefined) {
      environment[name] = value;
    }
  }
  for (const name of [
    "GIT_ALTERNATE_OBJECT_DIRECTORIES",
    "GIT_ATTR_SOURCE",
    "GIT_COMMON_DIR",
    "GIT_CONFIG_PARAMETERS",
    "GIT_DIR",
    "GIT_INDEX_FILE",
    "GIT_OBJECT_DIRECTORY",
    "GIT_NAMESPACE",
    "GIT_SHALLOW_FILE",
    "GIT_WORK_TREE",
  ]) {
    delete environment[name];
  }
  environment["GIT_CONFIG_COUNT"] = "0";
  environment["GIT_CONFIG_GLOBAL"] = "/dev/null";
  environment["GIT_CONFIG_NOSYSTEM"] = "1";
  environment["GIT_ATTR_NOSYSTEM"] = "1";
  environment["GIT_NO_LAZY_FETCH"] = "1";
  environment["GIT_NO_REPLACE_OBJECTS"] = "1";
  return environment;
};

const runGit = async (
  workingDirectory: string,
  args: readonly string[],
): Promise<string> =>
  new Promise((resolveCommand, rejectCommand) => {
    execFile(
      "git",
      args,
      {
        cwd: workingDirectory,
        encoding: "utf8",
        env: gitEnvironment(),
        maxBuffer: maximumCandidateJSONBytes,
      },
      (error, stdout) => {
        if (error) {
          rejectCommand(
            new DeploymentInputError(
              "Failed to verify candidate against local Git",
            ),
          );
          return;
        }
        resolveCommand(stdout);
      },
    );
  });

const isMissingPathError = (error: unknown): boolean =>
  error instanceof Error && (error as NodeJS.ErrnoException).code === "ENOENT";

const requireNoRepositoryAttributes = async (
  paths: readonly string[],
): Promise<void> => {
  for (const path of paths) {
    try {
      await lstat(path);
    } catch (error) {
      if (isMissingPathError(error)) {
        continue;
      }
      throw new DeploymentInputError(
        "Failed to inspect the local Git info/attributes file",
      );
    }
    throw new DeploymentInputError(
      `Local Git info/attributes file is forbidden: ${path}`,
    );
  }
};

const requireNoHiddenIndexFlags = async (
  repositoryRoot: string,
): Promise<void> => {
  const output = await runGit(repositoryRoot, ["ls-files", "-v", "-z"]);
  for (const entry of output.split("\0")) {
    if (!entry) {
      continue;
    }
    const tag = entry[0];
    if (!tag || entry[1] !== " ") {
      throw new DeploymentInputError(
        "Failed to inspect local Git index flags unambiguously",
      );
    }
    if (tag === "S" || (tag >= "a" && tag <= "z")) {
      throw new DeploymentInputError(
        "Git index contains a forbidden assume-unchanged or skip-worktree flag",
      );
    }
  }
};

const repositoryAttributePaths = async (
  repositoryRoot: string,
): Promise<readonly string[]> => {
  const [gitDirectory, commonDirectory] = await Promise.all([
    runGit(repositoryRoot, ["rev-parse", "--absolute-git-dir"]),
    runGit(repositoryRoot, [
      "rev-parse",
      "--path-format=absolute",
      "--git-common-dir",
    ]),
  ]);
  return [
    ...new Set(
      [gitDirectory, commonDirectory].map((directory) =>
        resolve(directory.trim(), "info/attributes"),
      ),
    ),
  ];
};

const verifyCanonicalSourceArchive = async (
  repositoryRoot: string,
  archivePath: string,
  releaseCommit: string,
  attributePaths: readonly string[],
): Promise<void> => {
  await requireNoRepositoryAttributes(attributePaths);
  const archive = await open(archivePath, "r");
  const child = spawn(
    "git",
    [
      "-c",
      "core.attributesFile=/dev/null",
      "-c",
      "tar.umask=0002",
      "archive",
      "--format=tar",
      `--prefix=guard-daemon-${releaseCommit}/`,
      releaseCommit,
    ],
    {
      cwd: repositoryRoot,
      env: gitEnvironment(),
      stdio: ["ignore", "pipe", "ignore"],
    },
  );
  const output = child.stdout;
  if (!output) {
    await archive.close();
    child.kill();
    throw new DeploymentInputError(
      "Failed to build the canonical source archive",
    );
  }
  const completion = new Promise<number>((resolveExit, rejectExit) => {
    child.once("error", rejectExit);
    child.once("close", (code) => resolveExit(code ?? -1));
  });
  let offset = 0;
  let matches = true;
  try {
    for await (const value of output) {
      const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value);
      const expected = Buffer.alloc(chunk.length);
      const { bytesRead } = await archive.read(
        expected,
        0,
        chunk.length,
        offset,
      );
      if (bytesRead !== chunk.length || !expected.equals(chunk)) {
        matches = false;
      }
      offset += chunk.length;
    }
    const [code, metadata] = await Promise.all([completion, archive.stat()]);
    if (code !== 0 || metadata.size !== offset || !matches) {
      throw new DeploymentInputError(
        "Source archive does not match the canonical git archive",
      );
    }
  } catch (error) {
    child.kill();
    if (error instanceof DeploymentInputError) {
      throw error;
    }
    throw new DeploymentInputError(
      "Failed to verify the canonical source archive",
    );
  } finally {
    await archive.close();
    await requireNoRepositoryAttributes(attributePaths);
  }
};

const verifyCandidateGitIdentity = async (
  directory: string,
  releaseCommit: string,
  releaseTree: string,
): Promise<string> => {
  const repositoryRoot = (
    await runGit(process.cwd(), ["rev-parse", "--show-toplevel"])
  ).trim();
  const attributePaths = await repositoryAttributePaths(repositoryRoot);
  await Promise.all([
    requireNoRepositoryAttributes(attributePaths),
    requireNoHiddenIndexFlags(repositoryRoot),
  ]);
  const resolvedCommit = (
    await runGit(repositoryRoot, [
      "rev-parse",
      "--verify",
      `${releaseCommit}^{commit}`,
    ])
  ).trim();
  const headCommit = (
    await runGit(repositoryRoot, ["rev-parse", "--verify", "HEAD^{commit}"])
  ).trim();
  const actualTree = (
    await runGit(repositoryRoot, ["show", "-s", "--format=%T", releaseCommit])
  ).trim();
  const checkoutStatus = await runGit(repositoryRoot, [
    "status",
    "--porcelain=v1",
    "--untracked-files=all",
  ]);
  if (
    resolvedCommit !== releaseCommit ||
    headCommit !== releaseCommit ||
    actualTree !== releaseTree ||
    checkoutStatus.length !== 0
  ) {
    throw new DeploymentInputError(
      "Release candidate does not match HEAD of a clean local checkout",
    );
  }
  await verifyCanonicalSourceArchive(
    repositoryRoot,
    resolve(directory, "guard-daemon-source.tar"),
    releaseCommit,
    attributePaths,
  );
  await Promise.all([
    requireNoRepositoryAttributes(attributePaths),
    requireNoHiddenIndexFlags(repositoryRoot),
  ]);
  return repositoryRoot;
};

export const loadReleaseCandidate = async (
  path: string,
  artifact: LoadedArtifact,
  verifyGitIdentity = true,
): Promise<ReleaseCandidate> => {
  const candidatePath = resolve(path);
  if (basename(candidatePath) !== "release-candidate.json") {
    throw new DeploymentInputError(
      "--release-candidate must point to release-candidate.json",
    );
  }
  const directory = dirname(candidatePath);
  await requireCandidateDirectory(directory);
  let names: readonly string[];
  try {
    names = (await readdir(directory)).sort();
  } catch {
    throw new DeploymentInputError(
      "Complete release candidate directory could not be read safely",
    );
  }
  if (
    names.length !== candidateOutputNames.length ||
    names.some((name, index) => name !== candidateOutputNames[index])
  ) {
    throw new DeploymentInputError(
      "Release candidate directory has invalid contents",
    );
  }
  await Promise.all(
    candidateOutputNames.map((name) =>
      requireRegularCandidateFile(resolve(directory, name)),
    ),
  );
  const contentBytes = await readFile(candidatePath);
  if (contentBytes.length > maximumCandidateJSONBytes) {
    throw new DeploymentInputError(
      "Release candidate exceeds the allowed JSON size",
    );
  }
  let content: string;
  try {
    content = new TextDecoder("utf-8", { fatal: true }).decode(contentBytes);
  } catch {
    throw new DeploymentInputError(
      "Release candidate contains invalid encoding",
    );
  }
  const candidate = new CandidateJSONParser(content).parse();
  requireCandidateKeys(candidate, [
    "artifacts",
    "releaseCommit",
    "releaseTree",
    "schemaVersion",
    "target",
  ]);
  const artifacts = requireCandidateObject(candidate, "artifacts");
  requireCandidateKeys(artifacts, Object.keys(candidateArtifactPaths));
  let contractSha256 = "";
  for (const [name, expectedPath] of Object.entries(candidateArtifactPaths)) {
    const record = requireCandidateObject(artifacts, name);
    requireCandidateKeys(record, ["path", "sha256"]);
    const recordPath = requireCandidateString(record, "path");
    const recordSha256 = requireCandidateString(record, "sha256");
    const outputPath = resolve(directory, expectedPath);
    const actualSha256 = sha256(await readFile(outputPath));
    if (
      recordPath !== expectedPath ||
      !sha256Pattern.test(recordSha256) ||
      recordSha256 !== actualSha256
    ) {
      throw new DeploymentInputError(
        "Release candidate failed strict validation",
      );
    }
    if (name === "contract") {
      contractSha256 = recordSha256;
    }
  }
  const releaseCommit = requireCandidateString(candidate, "releaseCommit");
  const releaseTree = requireCandidateString(candidate, "releaseTree");
  const target = requireCandidateObject(candidate, "target");
  requireCandidateKeys(target, ["cgoEnabled", "goarch", "goos"]);
  if (
    requireCandidateString(candidate, "schemaVersion") !== "1" ||
    !releaseCommitPattern.test(releaseCommit) ||
    !releaseCommitPattern.test(releaseTree) ||
    requireCandidateBoolean(target, "cgoEnabled") ||
    requireCandidateString(target, "goarch") !== "amd64" ||
    requireCandidateString(target, "goos") !== "linux" ||
    contractSha256 !== artifact.sha256
  ) {
    throw new DeploymentInputError(
      "Release candidate failed strict validation",
    );
  }
  const expectedChecksums = (
    await Promise.all(
      candidateContentNames.map(async (name) => {
        const digest = sha256(await readFile(resolve(directory, name)));
        return `${digest.slice("sha256:".length)}  ${name}\n`;
      }),
    )
  ).join("");
  const checksums = await readFile(resolve(directory, "SHA256SUMS"), "utf8");
  if (checksums !== expectedChecksums) {
    throw new DeploymentInputError(
      "Candidate SHA256SUMS is invalid or unsorted",
    );
  }
  const repositoryRoot = verifyGitIdentity
    ? await verifyCandidateGitIdentity(directory, releaseCommit, releaseTree)
    : await realpath(process.cwd()).catch(() => {
        throw new DeploymentInputError(
          "Failed to verify installed production tooling",
        );
      });
  return {
    artifactSha256: contractSha256,
    releaseCommit,
    releaseTree,
    repositoryRoot,
  };
};

export const normalizeRoles = (
  destination: string,
  self: string,
  sponsor: string,
): ImmutableValues => {
  let values: ImmutableValues;
  try {
    values = {
      destination: getAddress(destination),
      self: getAddress(self),
      sponsor: getAddress(sponsor),
    };
  } catch {
    throw new DeploymentInputError("Role address has an invalid format");
  }
  const zero = "0x0000000000000000000000000000000000000000";
  if (Object.values(values).some((value) => value === zero)) {
    throw new DeploymentInputError("Role address must not be zero");
  }
  if (values.destination === values.sponsor) {
    throw new DeploymentInputError("Destination and sponsor must differ");
  }
  return values;
};

export const linkRuntime = (
  artifact: CanonicalArtifact,
  values: ImmutableValues,
): string => {
  const runtime = Buffer.from(getBytes(artifact.deployedBytecode));
  const ranges: { readonly end: number; readonly start: number }[] = [];
  for (const name of immutableNames) {
    const address = Buffer.from(getBytes(values[name]));
    for (const reference of artifact.immutableReferences[name]) {
      const end = reference.start + reference.length;
      if (
        reference.length !== 32 ||
        reference.start < 0 ||
        end > runtime.length ||
        runtime.subarray(reference.start, end).some((byte) => byte !== 0)
      ) {
        throw new DeploymentInputError(`Immutable reference ${name} is unsafe`);
      }
      ranges.push({ end, start: reference.start });
      address.copy(runtime, reference.start + 12);
    }
  }
  ranges.sort((left, right) => left.start - right.start);
  if (
    ranges.some(
      (range, index) => index > 0 && range.start < ranges[index - 1]!.end,
    )
  ) {
    throw new DeploymentInputError("Immutable references overlap");
  }
  return `0x${runtime.toString("hex")}`;
};

export const sourceProvenance = (
  artifact: CanonicalArtifact,
  releaseCommit?: string,
): SourceProvenance => {
  if (!releaseCommit) {
    return {
      kind: "source-tree-sha256",
      value: artifact.sourceTreeSha256,
    };
  }
  if (!releaseCommitPattern.test(releaseCommit)) {
    throw new DeploymentInputError("Release commit has an invalid format");
  }
  return { kind: "git-commit", value: releaseCommit };
};

export const createDeploymentManifest = (input: {
  readonly address: string;
  readonly artifact: LoadedArtifact;
  readonly blockHash: string;
  readonly blockNumber: bigint;
  readonly chainId: bigint;
  readonly destination: string;
  readonly source: SourceProvenance;
  readonly sponsor: string;
  readonly transactionHash: string;
}): DeploymentManifest => {
  const values = normalizeRoles(
    input.destination,
    input.address,
    input.sponsor,
  );
  const runtime = linkRuntime(input.artifact.artifact, values);
  return {
    address: values.self,
    artifact: { sha256: input.artifact.sha256 },
    chainId: input.chainId.toString(),
    compiler: {
      settings: input.artifact.artifact.settings,
      version: input.artifact.artifact.compilerVersion,
    },
    constructorArguments: {
      destination: values.destination,
      sponsor: values.sponsor,
    },
    contractRole: "rescuer",
    deploymentBlockHash: input.blockHash.toLowerCase(),
    deploymentBlockNumber: input.blockNumber.toString(),
    deploymentTransactionHash: input.transactionHash.toLowerCase(),
    immutables: values,
    runtime: {
      byteLength: getBytes(runtime).length,
      keccak256: keccak256(runtime),
    },
    schemaVersion: "1",
    source: input.source,
  };
};
