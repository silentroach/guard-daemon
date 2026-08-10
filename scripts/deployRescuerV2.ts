import { createHash, randomUUID } from "node:crypto";
import type { BigIntStats } from "node:fs";
import {
  link,
  lstat,
  open,
  readFile,
  readdir,
  realpath,
  rm,
  stat,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { basename, dirname, isAbsolute, relative, resolve } from "node:path";
import { pathToFileURL } from "node:url";

import {
  ContractFactory,
  FetchRequest,
  Interface,
  JsonRpcProvider,
  Transaction,
  Wallet,
  getAddress,
  getBytes,
  type TransactionRequest,
} from "ethers";

import {
  DeploymentInputError,
  createDeploymentManifest,
  linkRuntime,
  loadCanonicalArtifact,
  loadReleaseCandidate,
  normalizeRoles,
  sourceProvenance,
} from "./deployment.js";
import { verifyCanonicalArtifact } from "./verifyArtifacts.js";

interface BaseCLIOptions {
  readonly chainId: bigint;
  readonly destination: string;
  readonly sponsor: string;
}

interface PlanCLIOptions extends BaseCLIOptions {
  readonly broadcast: false;
}

interface BroadcastCLIOptions extends BaseCLIOptions {
  readonly broadcast: true;
  readonly confirmations: number;
  readonly gasLimit: bigint;
  readonly manifestPath: string;
  readonly maxFeePerGas: bigint;
  readonly maxPriorityFeePerGas: bigint;
  readonly maxTotalCost: bigint;
  readonly receiptTimeoutMS: number;
  readonly recoveryDirectory: string;
  readonly releaseCandidatePath: string;
  readonly rpcTimeoutMS: number;
  readonly rpcURL: string;
}

type CLIOptions = BroadcastCLIOptions | PlanCLIOptions;

interface TransactionLimits {
  readonly gasLimit: bigint;
  readonly maxFeePerGas: bigint;
  readonly maxPriorityFeePerGas: bigint;
}

interface DeploymentRecoveryRecord {
  readonly caps: {
    readonly gasLimit: string;
    readonly maxFeePerGas: string;
    readonly maxPriorityFeePerGas: string;
    readonly maxTotalCost: string;
  };
  readonly chainId: string;
  readonly kind: "rescuer-v2-deployment";
  readonly releaseCommit: string;
  readonly schemaVersion: "1";
  readonly sponsor: string;
  readonly transaction: {
    readonly hash: string;
    readonly nonce: number;
    readonly rawSignedTransaction: string;
    readonly type: 2;
  };
}

class DeploymentError extends Error {}

const optionNames: ReadonlySet<string> = new Set([
  "--broadcast",
  "--chain-id",
  "--confirmations",
  "--destination",
  "--gas-limit",
  "--manifest",
  "--max-fee-per-gas-wei",
  "--max-priority-fee-per-gas-wei",
  "--max-total-cost-wei",
  "--receipt-timeout-ms",
  "--recovery-directory",
  "--release-candidate",
  "--rpc-timeout-ms",
  "--rpc-url",
  "--sponsor",
]);

const broadcastChainIDs: ReadonlySet<bigint> = new Set([1n, 56n, 137n, 31337n]);
const productionChainIDs: ReadonlySet<bigint> = new Set([1n, 56n, 137n]);
const productionRecoveryDirectory =
  "/var/lib/guard-daemon-operator/deployment-recovery";
const productionManifestDirectory =
  "/var/lib/guard-daemon-operator/deployment-manifests";
const productionCandidateRoot = "/usr/lib/guard-daemon/candidates";
const productionToolingRoot = "/usr/lib/guard-daemon/deployment";
const maximumRecoveryRecordBytes = 1024 * 1024;

const requiredValue = (
  values: ReadonlyMap<string, string>,
  name: string,
): string => {
  const value = values.get(name);
  if (!value) {
    throw new DeploymentInputError(`Required argument ${name} is missing`);
  }
  return value;
};

const parsePositiveInteger = (value: string, name: string): number => {
  if (!/^[1-9][0-9]*$/.test(value)) {
    throw new DeploymentInputError(`${name} must be a positive integer`);
  }
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed)) {
    throw new DeploymentInputError(`${name} is outside the safe range`);
  }
  return parsed;
};

const maximumUint256 = (1n << 256n) - 1n;

const parseDecimalBigInt = (
  value: string,
  name: string,
  allowZero: boolean,
): bigint => {
  const pattern = allowZero ? /^(?:0|[1-9][0-9]*)$/ : /^[1-9][0-9]*$/;
  if (!pattern.test(value)) {
    throw new DeploymentInputError(
      `${name} must be a ${allowZero ? "nonnegative" : "positive"} decimal bigint`,
    );
  }
  const parsed = BigInt(value);
  if (parsed > maximumUint256) {
    throw new DeploymentInputError(`${name} is outside the uint256 range`);
  }
  return parsed;
};

export const parseCLIOptions = (
  args: readonly string[],
  environment: NodeJS.ProcessEnv = process.env,
): CLIOptions => {
  const values = new Map<string, string>();
  let broadcast = false;
  for (let index = 0; index < args.length; index++) {
    const name = args[index];
    if (!name || !optionNames.has(name)) {
      throw new DeploymentInputError(`Unknown argument ${name ?? ""}`);
    }
    if (name === "--broadcast") {
      if (broadcast) {
        throw new DeploymentInputError(
          "Argument --broadcast was specified more than once",
        );
      }
      broadcast = true;
      continue;
    }
    if (values.has(name)) {
      throw new DeploymentInputError(
        `Argument ${name} was specified more than once`,
      );
    }
    const value = args[index + 1];
    if (!value || value.startsWith("--")) {
      throw new DeploymentInputError(`No value was specified for ${name}`);
    }
    values.set(name, value);
    index++;
  }

  const chainIDValue = requiredValue(values, "--chain-id");
  if (!/^[1-9][0-9]*$/.test(chainIDValue)) {
    throw new DeploymentInputError(
      "--chain-id must be a positive decimal value",
    );
  }
  const chainId = BigInt(chainIDValue);
  const destination = requiredValue(values, "--destination");
  const sponsor = requiredValue(values, "--sponsor");
  normalizeRoles(destination, sponsor, sponsor);
  const normalizedDestination = getAddress(destination);
  const normalizedSponsor = getAddress(sponsor);
  const recoveryDirectoryValue = values.get("--recovery-directory");
  if (productionChainIDs.has(chainId) && recoveryDirectoryValue) {
    throw new DeploymentInputError(
      "--recovery-directory is allowed only for local chain ID 31337",
    );
  }
  if (!broadcast) {
    return {
      broadcast: false,
      chainId,
      destination: normalizedDestination,
      sponsor: normalizedSponsor,
    };
  }

  const manifestPath = requiredValue(values, "--manifest");
  const releaseCandidatePath = requiredValue(values, "--release-candidate");
  if (!broadcastChainIDs.has(chainId)) {
    throw new DeploymentInputError(
      "Broadcast is allowed only for chain IDs 1, 56, 137, and local chain ID 31337",
    );
  }
  const rpcURLArgument = values.get("--rpc-url");
  let rpcURL: string;
  if (chainId === 31337n) {
    rpcURL = requiredValue(values, "--rpc-url");
  } else {
    if (rpcURLArgument) {
      throw new DeploymentInputError(
        "Production RPC must be specified through DEPLOYMENT_RPC_URL, not --rpc-url",
      );
    }
    const deploymentRPCURL = environment["DEPLOYMENT_RPC_URL"];
    if (!deploymentRPCURL) {
      throw new DeploymentInputError(
        "DEPLOYMENT_RPC_URL is not set for production --broadcast",
      );
    }
    rpcURL = deploymentRPCURL;
  }
  const recoveryDirectory =
    chainId === 31337n
      ? resolve(requiredValue(values, "--recovery-directory"))
      : productionRecoveryDirectory;
  const gasLimit = parseDecimalBigInt(
    requiredValue(values, "--gas-limit"),
    "--gas-limit",
    false,
  );
  const maxFeePerGas = parseDecimalBigInt(
    requiredValue(values, "--max-fee-per-gas-wei"),
    "--max-fee-per-gas-wei",
    false,
  );
  const maxPriorityFeePerGas = parseDecimalBigInt(
    requiredValue(values, "--max-priority-fee-per-gas-wei"),
    "--max-priority-fee-per-gas-wei",
    true,
  );
  const maxTotalCost = parseDecimalBigInt(
    requiredValue(values, "--max-total-cost-wei"),
    "--max-total-cost-wei",
    false,
  );
  if (maxPriorityFeePerGas > maxFeePerGas) {
    throw new DeploymentInputError(
      "--max-priority-fee-per-gas-wei exceeds --max-fee-per-gas-wei",
    );
  }
  if (gasLimit * maxFeePerGas > maxTotalCost) {
    throw new DeploymentInputError(
      "gasLimit * maxFeePerGas limit exceeds --max-total-cost-wei",
    );
  }
  return {
    broadcast: true,
    chainId,
    confirmations: parsePositiveInteger(
      values.get("--confirmations") ?? "1",
      "--confirmations",
    ),
    destination: normalizedDestination,
    gasLimit,
    manifestPath: resolve(manifestPath),
    maxFeePerGas,
    maxPriorityFeePerGas,
    maxTotalCost,
    receiptTimeoutMS: parsePositiveInteger(
      values.get("--receipt-timeout-ms") ?? "120000",
      "--receipt-timeout-ms",
    ),
    recoveryDirectory,
    releaseCandidatePath: resolve(releaseCandidatePath),
    rpcTimeoutMS: parsePositiveInteger(
      values.get("--rpc-timeout-ms") ?? "10000",
      "--rpc-timeout-ms",
    ),
    rpcURL,
    sponsor: normalizedSponsor,
  };
};

interface PathIdentity {
  readonly canonical: string;
  readonly device?: bigint;
  readonly inode?: bigint;
}

interface DirectoryIdentity {
  readonly canonical: string;
  readonly device: bigint;
  readonly gid: bigint;
  readonly inode: bigint;
  readonly mode: number;
  readonly uid: bigint;
}

interface DirectoryRequirements {
  readonly exactMode?: number;
  readonly rejectGroupOrWorldWrite?: boolean;
  readonly rootOwned?: boolean;
}

interface PreparedOutput {
  readonly parent: DirectoryIdentity;
  readonly path: string;
}

const localRecoveryDirectoryRequirements: DirectoryRequirements = {
  exactMode: 0o700,
};
const productionRecoveryDirectoryRequirements: DirectoryRequirements = {
  exactMode: 0o700,
  rootOwned: true,
};
const localOutputDirectoryRequirements: DirectoryRequirements = {};
const productionOutputDirectoryRequirements: DirectoryRequirements = {
  exactMode: 0o700,
  rejectGroupOrWorldWrite: true,
  rootOwned: true,
};
const productionInstalledRootRequirements: DirectoryRequirements = {
  exactMode: 0o755,
  rootOwned: true,
};
const productionInstalledReleaseRequirements: DirectoryRequirements = {
  exactMode: 0o700,
  rootOwned: true,
};

const hasFileSystemCode = (error: unknown, code: string): boolean =>
  error instanceof Error && (error as NodeJS.ErrnoException).code === code;

const canonicalPath = async (path: string): Promise<string> => {
  try {
    return await realpath(path);
  } catch (error) {
    if (!hasFileSystemCode(error, "ENOENT")) {
      throw new DeploymentError("Failed to inspect output path");
    }
  }
  try {
    return resolve(await realpath(dirname(path)), basename(path));
  } catch {
    throw new DeploymentError("Output parent directory must exist");
  }
};

const pathIdentity = async (path: string): Promise<PathIdentity> => {
  const canonical = await canonicalPath(path);
  try {
    const metadata = await stat(path, { bigint: true });
    return {
      canonical,
      device: metadata.dev,
      inode: metadata.ino,
    };
  } catch (error) {
    if (hasFileSystemCode(error, "ENOENT")) {
      return { canonical };
    }
    throw new DeploymentError("Failed to inspect output path");
  }
};

const verifyDistinctPaths = async (paths: readonly string[]): Promise<void> => {
  const identities = await Promise.all(paths.map(pathIdentity));
  for (let left = 0; left < identities.length; left++) {
    for (let right = left + 1; right < identities.length; right++) {
      const a = identities[left]!;
      const b = identities[right]!;
      if (
        a.canonical === b.canonical ||
        (a.device !== undefined &&
          b.device !== undefined &&
          a.device === b.device &&
          a.inode === b.inode)
      ) {
        throw new DeploymentInputError(
          "Artifact, release candidate, recovery record, and manifest must be different files",
        );
      }
    }
  }
};

const requireAbsentOutput = async (
  path: string,
  subject: string,
): Promise<void> => {
  try {
    await lstat(path);
  } catch (error) {
    if (hasFileSystemCode(error, "ENOENT")) {
      return;
    }
    throw new DeploymentError(`Failed to inspect ${subject}`);
  }
  throw new DeploymentInputError(
    `${subject} already exists; automatic overwrite is forbidden`,
  );
};

const validateDirectoryMetadata = (
  metadata: BigIntStats,
  subject: string,
  requirements: DirectoryRequirements,
): void => {
  const mode = Number(metadata.mode & 0o7777n);
  if (metadata.isSymbolicLink() || !metadata.isDirectory()) {
    throw new DeploymentInputError(
      `${subject} must be a regular directory, not a symbolic link`,
    );
  }
  if (requirements.exactMode !== undefined && mode !== requirements.exactMode) {
    throw new DeploymentInputError(
      `${subject} must have mode ${requirements.exactMode.toString(8).padStart(4, "0")}`,
    );
  }
  if (requirements.rootOwned && (metadata.uid !== 0n || metadata.gid !== 0n)) {
    throw new DeploymentInputError(`${subject} must be owned by root:root`);
  }
  if (requirements.rejectGroupOrWorldWrite && (mode & 0o022) !== 0) {
    throw new DeploymentInputError(
      `${subject} must not be writable by group or others`,
    );
  }
};

const readDirectoryMetadata = async (
  path: string,
  subject: string,
  requirements: DirectoryRequirements,
): Promise<BigIntStats> => {
  let metadata: BigIntStats;
  try {
    metadata = await lstat(path, { bigint: true });
  } catch {
    throw new DeploymentInputError(`${subject} must already exist`);
  }
  validateDirectoryMetadata(metadata, subject, requirements);
  return metadata;
};

const requireDirectory = async (
  path: string,
  subject: string,
  requirements: DirectoryRequirements,
): Promise<DirectoryIdentity> => {
  const initial = await readDirectoryMetadata(path, subject, requirements);
  let canonical: string;
  try {
    canonical = await realpath(path);
  } catch {
    throw new DeploymentInputError(`Failed to canonicalize ${subject}`);
  }
  const [current, canonicalMetadata] = await Promise.all([
    readDirectoryMetadata(path, subject, requirements),
    readDirectoryMetadata(canonical, subject, requirements),
  ]);
  if (
    initial.dev !== current.dev ||
    initial.ino !== current.ino ||
    current.dev !== canonicalMetadata.dev ||
    current.ino !== canonicalMetadata.ino
  ) {
    throw new DeploymentInputError(
      `${subject} changed during identity verification`,
    );
  }
  return {
    canonical,
    device: current.dev,
    gid: current.gid,
    inode: current.ino,
    mode: Number(current.mode & 0o7777n),
    uid: current.uid,
  };
};

const requireStableDirectory = async (
  expected: DirectoryIdentity,
  subject: string,
  requirements: DirectoryRequirements,
): Promise<void> => {
  const current = await requireDirectory(
    expected.canonical,
    subject,
    requirements,
  );
  if (
    current.canonical !== expected.canonical ||
    current.device !== expected.device ||
    current.inode !== expected.inode
  ) {
    throw new DeploymentInputError(
      `${subject} changed after the preliminary check`,
    );
  }
};

const requireFixedDirectory = async (
  path: string,
  subject: string,
  requirements: DirectoryRequirements,
): Promise<void> => {
  const directory = await requireDirectory(path, subject, requirements);
  if (directory.canonical !== path) {
    throw new DeploymentInputError(`${subject} must have a canonical path`);
  }
};

const isWithin = (root: string, path: string): boolean => {
  const relation = relative(root, path);
  return (
    relation === "" || (!relation.startsWith("..") && !isAbsolute(relation))
  );
};

const prepareManifestPath = async (
  path: string,
  production: boolean,
): Promise<PreparedOutput> => {
  await requireAbsentOutput(path, "Manifest");
  const requirements = production
    ? productionOutputDirectoryRequirements
    : localOutputDirectoryRequirements;
  const parent = await requireDirectory(
    dirname(path),
    "Manifest parent",
    requirements,
  );
  const canonical = resolve(parent.canonical, basename(path));
  await requireAbsentOutput(canonical, "Manifest");
  return { parent, path: canonical };
};

const prepareRecoveryDirectory = async (
  options: BroadcastCLIOptions,
): Promise<DirectoryIdentity> => {
  const production = productionChainIDs.has(options.chainId);
  const requirements = production
    ? productionRecoveryDirectoryRequirements
    : localRecoveryDirectoryRequirements;
  const directory = await requireDirectory(
    options.recoveryDirectory,
    "Recovery registry",
    requirements,
  );
  if (production && directory.canonical !== productionRecoveryDirectory) {
    throw new DeploymentInputError(
      `Production recovery registry must be canonical ${productionRecoveryDirectory}`,
    );
  }
  if (options.chainId === 31337n) {
    let temporaryRoot: string;
    try {
      temporaryRoot = await realpath(tmpdir());
    } catch {
      throw new DeploymentInputError(
        "Failed to inspect temporary directory root",
      );
    }
    if (
      directory.canonical === temporaryRoot ||
      !isWithin(temporaryRoot, directory.canonical)
    ) {
      throw new DeploymentInputError(
        "Local recovery registry must be a separate temporary directory",
      );
    }
  }
  return directory;
};

const requireOutputContainment = async (
  manifestPath: string,
  recoveryDirectory: string,
  candidatePath: string,
  repositoryRoot: string,
): Promise<void> => {
  const [candidateDirectory, worktree] = await Promise.all([
    realpath(dirname(candidatePath)),
    realpath(repositoryRoot),
  ]).catch(() => {
    throw new DeploymentInputError(
      "Failed to inspect candidate and Git worktree boundaries",
    );
  });
  const manifestParent = dirname(manifestPath);
  for (const root of [candidateDirectory, worktree]) {
    if (
      isWithin(root, manifestPath) ||
      isWithin(root, manifestParent) ||
      isWithin(root, recoveryDirectory)
    ) {
      throw new DeploymentInputError(
        "Manifest and recovery registry must be outside the candidate and Git worktree",
      );
    }
  }
};

const recoveryRecordName = (
  chainId: bigint,
  sponsor: string,
  releaseCommit: string,
): string =>
  `rescuer-v2-${chainId.toString()}-${sponsor.toLowerCase()}-${releaseCommit}.json`;

const recoveryLockName = (chainId: bigint, sponsor: string): string =>
  `.rescuer-v2-${chainId.toString()}-${sponsor.toLowerCase()}.lock`;

const isRecord = (value: unknown): value is Readonly<Record<string, unknown>> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const scanRecoveryRegistry = async (
  directory: string,
  chainId: bigint,
  sponsor: string,
  ignoredName?: string,
): Promise<void> => {
  let names: readonly string[];
  try {
    names = (await readdir(directory)).sort();
  } catch {
    throw new DeploymentError("Failed to read recovery registry");
  }
  const blockingLock = recoveryLockName(chainId, sponsor);
  const lockPattern = /^\.rescuer-v2-[1-9][0-9]*-0x[0-9a-f]{40}\.lock$/;
  for (const name of names) {
    if (name === ignoredName) {
      continue;
    }
    const path = resolve(directory, name);
    let metadata;
    try {
      metadata = await lstat(path);
    } catch {
      throw new DeploymentInputError(
        "Recovery registry contains an unverifiable entry",
      );
    }
    if (metadata.isSymbolicLink() || !metadata.isFile()) {
      throw new DeploymentInputError(
        "Recovery registry contains a non-regular file",
      );
    }
    if (lockPattern.test(name)) {
      if (name === blockingLock) {
        throw new DeploymentInputError(
          "Incomplete recovery for this chain and sponsor blocks deployment",
        );
      }
      continue;
    }
    if (metadata.size > maximumRecoveryRecordBytes) {
      throw new DeploymentInputError(
        "Recovery registry contains an oversized entry",
      );
    }
    let value: unknown;
    try {
      value = JSON.parse(await readFile(path, "utf8"));
    } catch {
      throw new DeploymentInputError(
        "Recovery registry contains a corrupted entry",
      );
    }
    if (!isRecord(value)) {
      throw new DeploymentInputError(
        "Recovery registry contains a corrupted entry",
      );
    }
    const recordChainId = value["chainId"];
    const recordSponsor = value["sponsor"];
    const releaseCommit = value["releaseCommit"];
    let normalizedSponsor: string;
    try {
      normalizedSponsor =
        typeof recordSponsor === "string" ? getAddress(recordSponsor) : "";
    } catch {
      normalizedSponsor = "";
    }
    if (
      value["kind"] !== "rescuer-v2-deployment" ||
      value["schemaVersion"] !== "1" ||
      typeof recordChainId !== "string" ||
      !/^[1-9][0-9]*$/.test(recordChainId) ||
      typeof recordSponsor !== "string" ||
      normalizedSponsor !== recordSponsor ||
      typeof releaseCommit !== "string" ||
      !/^[0-9a-f]{40}$/.test(releaseCommit) ||
      name !==
        recoveryRecordName(
          BigInt(recordChainId),
          normalizedSponsor,
          releaseCommit,
        )
    ) {
      throw new DeploymentInputError(
        "Recovery registry contains a noncanonical entry",
      );
    }
    if (recordChainId === chainId.toString() && normalizedSponsor === sponsor) {
      throw new DeploymentInputError(
        "Incomplete recovery for this chain and sponsor blocks deployment",
      );
    }
  }
};

const syncDirectory = async (directory: string): Promise<void> => {
  const handle = await open(directory, "r");
  try {
    await handle.sync();
  } finally {
    await handle.close();
  }
};

const acquireRecoveryLock = async (
  directory: DirectoryIdentity,
  chainId: bigint,
  sponsor: string,
  requirements: DirectoryRequirements,
): Promise<string> => {
  const path = resolve(directory.canonical, recoveryLockName(chainId, sponsor));
  let handle: Awaited<ReturnType<typeof open>> | undefined;
  let created = false;
  try {
    await requireStableDirectory(directory, "Recovery registry", requirements);
    handle = await open(path, "wx", 0o600);
    created = true;
    await handle.writeFile("deployment recovery lock\n", "utf8");
    await handle.sync();
    await handle.close();
    handle = undefined;
    await requireStableDirectory(directory, "Recovery registry", requirements);
    await syncDirectory(directory.canonical);
    return path;
  } catch (error) {
    if (hasFileSystemCode(error, "EEXIST")) {
      throw new DeploymentInputError(
        "Incomplete recovery for this chain and sponsor blocks deployment",
      );
    }
    if (created) {
      await requireStableDirectory(directory, "Recovery registry", requirements)
        .then(() => rm(path, { force: true }))
        .catch(() => undefined);
    }
    throw new DeploymentError("Failed to acquire recovery registry lock");
  } finally {
    await handle?.close().catch(() => undefined);
  }
};

const releaseRecoveryLock = async (
  path: string,
  directory: DirectoryIdentity,
  requirements: DirectoryRequirements,
): Promise<void> => {
  try {
    await requireStableDirectory(directory, "Recovery registry", requirements);
    await rm(path);
    await requireStableDirectory(directory, "Recovery registry", requirements);
    await syncDirectory(directory.canonical);
  } catch {
    throw new DeploymentError("Failed to release recovery registry lock");
  }
};

const withDeadline = async <T>(
  operation: Promise<T>,
  timeoutMS: number,
  message: string,
): Promise<T> => {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([
      operation,
      new Promise<never>((_resolve, reject) => {
        timer = setTimeout(
          () => reject(new DeploymentError(message)),
          timeoutMS,
        );
      }),
    ]);
  } finally {
    if (timer) {
      clearTimeout(timer);
    }
  }
};

const atomicExclusiveWrite = async (
  path: string,
  content: string,
  subject: string,
  parent: DirectoryIdentity,
  requirements: DirectoryRequirements,
): Promise<void> => {
  const directory = parent.canonical;
  if (dirname(path) !== directory) {
    throw new DeploymentInputError(
      `${subject} is not in the verified parent directory`,
    );
  }
  await requireStableDirectory(parent, `${subject} parent`, requirements);
  const temporary = resolve(
    directory,
    `.${basename(path)}.tmp.${process.pid}.${randomUUID()}`,
  );
  let handle: Awaited<ReturnType<typeof open>> | undefined;
  try {
    handle = await open(temporary, "wx", 0o600);
    await handle.writeFile(content, "utf8");
    await handle.chmod(0o600);
    await handle.sync();
    await handle.close();
    handle = undefined;
    await requireStableDirectory(parent, `${subject} parent`, requirements);
    try {
      await link(temporary, path);
    } catch (error) {
      if (hasFileSystemCode(error, "EEXIST")) {
        throw new DeploymentInputError(
          `${subject} already exists; automatic overwrite is forbidden`,
        );
      }
      throw error;
    }
    await requireStableDirectory(parent, `${subject} parent`, requirements);
    await syncDirectory(directory);
  } finally {
    await handle?.close().catch(() => undefined);
    await requireStableDirectory(parent, `${subject} parent`, requirements)
      .then(() => rm(temporary, { force: true }))
      .catch(() => undefined);
  }
};

const decodeGetter = (data: string, name: string): string => {
  if (!/^0x0{24}[0-9a-fA-F]{40}$/.test(data)) {
    throw new DeploymentError(`Getter ${name} returned a noncanonical address`);
  }
  return getAddress(`0x${data.slice(-40)}`);
};

export const runDeployment = async (
  options: CLIOptions,
  environment: NodeJS.ProcessEnv = process.env,
): Promise<void> => {
  const artifactPath = resolve("artifacts/contracts/RescuerV2.json");
  await verifyCanonicalArtifact(artifactPath);
  const loaded = await loadCanonicalArtifact(artifactPath);
  const factory = new ContractFactory(
    loaded.artifact.abi,
    loaded.artifact.bytecode,
  );
  if (!options.broadcast) {
    const planned = await factory.getDeployTransaction(
      options.destination,
      options.sponsor,
    );
    if (!planned.data) {
      throw new DeploymentError("Artifact did not produce deployment data");
    }
    const planDigest = createHash("sha256")
      .update(getBytes(planned.data))
      .digest("hex");
    sourceProvenance(loaded.artifact);
    console.log("Local RescuerV2 plan verified; broadcast is disabled.");
    console.log(`SHA-256 deployment data: ${planDigest}`);
    return;
  }

  const production = productionChainIDs.has(options.chainId);
  const manifestOutput = await prepareManifestPath(
    options.manifestPath,
    production,
  );
  if (
    production &&
    manifestOutput.parent.canonical !== productionManifestDirectory
  ) {
    throw new DeploymentInputError(
      `Production manifest must be created directly in ${productionManifestDirectory}`,
    );
  }
  const manifestPath = manifestOutput.path;
  const recoveryDirectory = await prepareRecoveryDirectory(options);
  const recoveryRequirements = production
    ? productionRecoveryDirectoryRequirements
    : localRecoveryDirectoryRequirements;
  const manifestRequirements = production
    ? productionOutputDirectoryRequirements
    : localOutputDirectoryRequirements;
  await scanRecoveryRegistry(
    recoveryDirectory.canonical,
    options.chainId,
    options.sponsor,
  );
  await verifyDistinctPaths([
    artifactPath,
    manifestPath,
    options.releaseCandidatePath,
  ]);
  const candidate = await loadReleaseCandidate(
    options.releaseCandidatePath,
    loaded,
    !production,
  );
  if (production) {
    const expectedCandidateDirectory = resolve(
      productionCandidateRoot,
      candidate.releaseCommit,
    );
    const expectedCandidatePath = resolve(
      expectedCandidateDirectory,
      "release-candidate.json",
    );
    const expectedRepositoryRoot = resolve(
      productionToolingRoot,
      candidate.releaseCommit,
    );
    if (
      options.releaseCandidatePath !== expectedCandidatePath ||
      candidate.repositoryRoot !== expectedRepositoryRoot
    ) {
      throw new DeploymentInputError(
        "Production deployment requires authenticated root-owned candidate and tooling",
      );
    }
    await Promise.all([
      requireFixedDirectory(
        productionCandidateRoot,
        "Production candidate root",
        productionInstalledRootRequirements,
      ),
      requireFixedDirectory(
        expectedCandidateDirectory,
        "Production candidate",
        productionInstalledReleaseRequirements,
      ),
      requireFixedDirectory(
        productionToolingRoot,
        "Production tooling root",
        productionInstalledRootRequirements,
      ),
      requireFixedDirectory(
        expectedRepositoryRoot,
        "Production tooling",
        productionInstalledReleaseRequirements,
      ),
    ]);
  }
  await requireOutputContainment(
    manifestPath,
    recoveryDirectory.canonical,
    options.releaseCandidatePath,
    candidate.repositoryRoot,
  );
  const recoveryRecordPath = resolve(
    recoveryDirectory.canonical,
    recoveryRecordName(
      options.chainId,
      options.sponsor,
      candidate.releaseCommit,
    ),
  );
  await requireAbsentOutput(recoveryRecordPath, "Recovery record");
  await verifyDistinctPaths([
    artifactPath,
    manifestPath,
    recoveryRecordPath,
    options.releaseCandidatePath,
  ]);
  const provenance = sourceProvenance(loaded.artifact, candidate.releaseCommit);
  const transactionLimits: TransactionLimits = {
    gasLimit: options.gasLimit,
    maxFeePerGas: options.maxFeePerGas,
    maxPriorityFeePerGas: options.maxPriorityFeePerGas,
  };
  const planned = await factory.getDeployTransaction(
    options.destination,
    options.sponsor,
  );
  if (!planned.data) {
    throw new DeploymentError("Artifact did not produce deployment data");
  }

  const recoveryLockPath = await acquireRecoveryLock(
    recoveryDirectory,
    options.chainId,
    options.sponsor,
    recoveryRequirements,
  );
  try {
    await scanRecoveryRegistry(
      recoveryDirectory.canonical,
      options.chainId,
      options.sponsor,
      basename(recoveryLockPath),
    );
    await requireAbsentOutput(recoveryRecordPath, "Recovery record");
    const request = new FetchRequest(options.rpcURL);
    request.timeout = options.rpcTimeoutMS;
    const provider = new JsonRpcProvider(request);
    try {
      const network = await provider.getNetwork();
      if (network.chainId !== options.chainId) {
        throw new DeploymentError(
          "RPC returned a chain ID different from the expected value",
        );
      }

      const privateKey = environment["DEPLOYER_PRIVATE_KEY"];
      if (!privateKey) {
        throw new DeploymentError(
          "DEPLOYER_PRIVATE_KEY is not set for explicit --broadcast",
        );
      }
      let wallet: Wallet;
      try {
        wallet = new Wallet(privateKey);
      } catch {
        throw new DeploymentError("DEPLOYER_PRIVATE_KEY has an invalid format");
      }
      if (wallet.address !== options.sponsor) {
        throw new DeploymentError(
          "Deployment signer address does not match sponsor",
        );
      }
      const nonce = await provider.getTransactionCount(
        wallet.address,
        "pending",
      );
      const transactionRequest: TransactionRequest = {
        chainId: options.chainId,
        data: planned.data,
        gasLimit: transactionLimits.gasLimit,
        maxFeePerGas: transactionLimits.maxFeePerGas,
        maxPriorityFeePerGas: transactionLimits.maxPriorityFeePerGas,
        nonce,
        type: 2,
        value: 0n,
      };
      const rawSignedTransaction =
        await wallet.signTransaction(transactionRequest);
      const signedTransaction = Transaction.from(rawSignedTransaction);
      if (
        !signedTransaction.hash ||
        signedTransaction.type !== 2 ||
        signedTransaction.to !== null ||
        signedTransaction.chainId !== options.chainId ||
        signedTransaction.nonce !== nonce ||
        signedTransaction.data !== planned.data ||
        signedTransaction.value !== 0n ||
        signedTransaction.gasLimit !== options.gasLimit ||
        signedTransaction.maxFeePerGas !== options.maxFeePerGas ||
        signedTransaction.maxPriorityFeePerGas !== options.maxPriorityFeePerGas
      ) {
        throw new DeploymentError(
          "Signed transaction does not match the exact deployment plan",
        );
      }
      const recoveryRecord: DeploymentRecoveryRecord = {
        caps: {
          gasLimit: options.gasLimit.toString(),
          maxFeePerGas: options.maxFeePerGas.toString(),
          maxPriorityFeePerGas: options.maxPriorityFeePerGas.toString(),
          maxTotalCost: options.maxTotalCost.toString(),
        },
        chainId: options.chainId.toString(),
        kind: "rescuer-v2-deployment",
        releaseCommit: candidate.releaseCommit,
        schemaVersion: "1",
        sponsor: options.sponsor,
        transaction: {
          hash: signedTransaction.hash,
          nonce,
          rawSignedTransaction,
          type: 2,
        },
      };
      await atomicExclusiveWrite(
        recoveryRecordPath,
        `${JSON.stringify(recoveryRecord, null, 2)}\n`,
        "Recovery record",
        recoveryDirectory,
        recoveryRequirements,
      );
      const transaction =
        await provider.broadcastTransaction(rawSignedTransaction);
      if (transaction.hash !== signedTransaction.hash) {
        throw new DeploymentError(
          "RPC returned a hash that does not match the recovery record",
        );
      }
      const receipt = await withDeadline(
        transaction.wait(options.confirmations),
        options.receiptTimeoutMS,
        "Timed out waiting for the deployment receipt",
      );
      if (!receipt || receipt.status !== 1 || !receipt.contractAddress) {
        throw new DeploymentError(
          "Deployment did not receive a successful receipt",
        );
      }

      const address = getAddress(receipt.contractAddress);
      const block = await provider.getBlock(receipt.blockHash);
      if (!block || block.hash !== receipt.blockHash) {
        throw new DeploymentError(
          "RPC did not confirm the deployment block hash",
        );
      }
      const values = normalizeRoles(
        options.destination,
        address,
        options.sponsor,
      );
      const expectedRuntime = linkRuntime(loaded.artifact, values);
      const actualRuntime = await provider.getCode(address, receipt.blockHash);
      if (actualRuntime.toLowerCase() !== expectedRuntime.toLowerCase()) {
        throw new DeploymentError(
          "Deployed runtime does not match the canonical artifact",
        );
      }

      const contractInterface = new Interface(loaded.artifact.abi);
      for (const [name, expected] of [
        ["destination", values.destination],
        ["sponsor", values.sponsor],
        ["self", values.self],
      ] as const) {
        const result = await provider.call({
          blockTag: receipt.blockHash,
          data: contractInterface.encodeFunctionData(name),
          to: address,
        });
        if (decodeGetter(result, name) !== expected) {
          throw new DeploymentError(`Getter ${name} does not match the plan`);
        }
      }

      const manifest = createDeploymentManifest({
        address,
        artifact: loaded,
        blockHash: receipt.blockHash,
        blockNumber: BigInt(receipt.blockNumber),
        chainId: options.chainId,
        destination: options.destination,
        source: provenance,
        sponsor: options.sponsor,
        transactionHash: transaction.hash,
      });
      await atomicExclusiveWrite(
        manifestPath,
        `${JSON.stringify(manifest, null, 2)}\n`,
        "Manifest",
        manifestOutput.parent,
        manifestRequirements,
      );
      console.log("Deployment, runtime, and immutable parameters verified.");
      console.log(
        "Manifest published atomically; live defaults were not changed.",
      );
    } finally {
      await provider.destroy();
    }
  } finally {
    await releaseRecoveryLock(
      recoveryLockPath,
      recoveryDirectory,
      recoveryRequirements,
    );
  }
};

const main = async (): Promise<void> => {
  try {
    await runDeployment(parseCLIOptions(process.argv.slice(2), process.env));
  } catch (error) {
    if (
      error instanceof DeploymentInputError ||
      error instanceof DeploymentError
    ) {
      console.error(error.message);
    } else {
      console.error("Critical deployment error; result was not published");
    }
    process.exitCode = 1;
  }
};

const scriptPath = process.argv[1];
if (scriptPath && import.meta.url === pathToFileURL(resolve(scriptPath)).href) {
  await main();
}
