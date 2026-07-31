import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";

import { getAddress, getBytes, keccak256, type JsonFragment } from "ethers";

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
const bytecodePattern = /^0x(?:[0-9a-f]{2})+$/;
const immutableNames = ["destination", "self", "sponsor"] as const;

export class DeploymentInputError extends Error {}

const sha256 = (value: Buffer): string =>
  `sha256:${createHash("sha256").update(value).digest("hex")}`;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const requireString = (
  record: Record<string, unknown>,
  name: string,
): string => {
  const value = record[name];
  if (typeof value !== "string" || !value) {
    throw new DeploymentInputError(`Поле artifact ${name} отсутствует`);
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
    throw new DeploymentInputError(`${subject} содержит неожиданные поля`);
  }
};

const parseReferences = (
  value: unknown,
): CanonicalArtifact["immutableReferences"] => {
  if (!isRecord(value)) {
    throw new DeploymentInputError("Artifact не содержит immutable references");
  }
  requireExactKeys(value, immutableNames, "Immutable references");
  const result: Record<string, readonly ImmutableReference[]> = {};
  for (const name of immutableNames) {
    const entries = value[name];
    if (!Array.isArray(entries) || entries.length === 0) {
      throw new DeploymentInputError(`Immutable reference ${name} отсутствует`);
    }
    result[name] = entries.map((entry) => {
      if (!isRecord(entry)) {
        throw new DeploymentInputError(
          `Immutable reference ${name} повреждена`,
        );
      }
      requireExactKeys(
        entry,
        ["length", "start"],
        `Immutable reference ${name}`,
      );
      const length = entry["length"];
      const start = entry["start"];
      if (!Number.isSafeInteger(start) || !Number.isSafeInteger(length)) {
        throw new DeploymentInputError(
          `Immutable reference ${name} повреждена`,
        );
      }
      return { length: length as number, start: start as number };
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
    throw new DeploymentInputError(
      "Canonical artifact не является JSON object",
    );
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
  if (
    artifactVersion !== "1" ||
    contractName !== "RescuerV2" ||
    sourceName !== "RescuerV2.sol" ||
    !bytecodePattern.test(bytecode) ||
    !bytecodePattern.test(deployedBytecode) ||
    !sha256Pattern.test(compilerInputSha256) ||
    !sha256Pattern.test(sourceTreeSha256) ||
    !Array.isArray(value["abi"]) ||
    !isRecord(value["settings"])
  ) {
    throw new DeploymentInputError(
      "Canonical artifact не прошёл строгую проверку",
    );
  }
  return {
    abi: value["abi"] as readonly JsonFragment[],
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
    throw new DeploymentInputError(
      "Canonical artifact содержит недопустимый JSON",
    );
  }
  return { artifact: parseArtifact(parsed), sha256: sha256(content) };
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
    throw new DeploymentInputError("Адрес роли имеет недопустимый формат");
  }
  const zero = "0x0000000000000000000000000000000000000000";
  if (Object.values(values).some((value) => value === zero)) {
    throw new DeploymentInputError("Адрес роли не должен быть нулевым");
  }
  if (values.destination === values.sponsor) {
    throw new DeploymentInputError("Destination и sponsor должны различаться");
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
        throw new DeploymentInputError(
          `Immutable reference ${name} небезопасна`,
        );
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
    throw new DeploymentInputError("Immutable references пересекаются");
  }
  return `0x${runtime.toString("hex")}`;
};

export const sourceProvenance = (
  artifact: CanonicalArtifact,
): SourceProvenance => ({
  kind: "source-tree-sha256",
  value: artifact.sourceTreeSha256,
});

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
