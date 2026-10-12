import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

import solc from "solc";

interface CompilerDiagnostic {
  readonly formattedMessage: string;
  readonly severity: string;
}

interface CompilerContract {
  readonly abi: readonly unknown[];
  readonly evm: {
    readonly bytecode: { readonly object: string };
    readonly deployedBytecode: {
      readonly immutableReferences: Readonly<
        Record<string, readonly ImmutableReference[]>
      >;
      readonly object: string;
    };
  };
}

interface CompilerSource {
  readonly ast?: ASTNode;
}

interface CompilerOutput {
  readonly contracts?: Readonly<
    Record<string, Readonly<Record<string, CompilerContract>>>
  >;
  readonly errors?: readonly CompilerDiagnostic[];
  readonly sources?: Readonly<Record<string, CompilerSource>>;
}

interface ASTNode {
  readonly id?: number;
  readonly mutability?: string;
  readonly name?: string;
  readonly nodeType?: string;
  readonly nodes?: readonly ASTNode[];
  readonly stateVariable?: boolean;
}

interface ImmutableReference {
  readonly length: number;
  readonly start: number;
}

const compilerVersion = "0.8.36+commit.8a079791.Emscripten.clang";
const contracts = [
  { contractName: "RescuerV2", sourceName: "RescuerV2.sol" },
] as const;
const requiredImmutables = ["destination", "self", "sponsor"] as const;

const compilerSettings = {
  evmVersion: "prague",
  metadata: {
    appendCBOR: false,
    bytecodeHash: "none",
    useLiteralContent: true,
  },
  optimizer: {
    enabled: true,
    runs: 200,
  },
  outputSelection: {
    "*": {
      "": ["ast"],
      "*": [
        "abi",
        "evm.bytecode.object",
        "evm.deployedBytecode.immutableReferences",
        "evm.deployedBytecode.object",
      ],
    },
  },
} as const;

const sha256 = (value: string | Buffer): string =>
  `sha256:${createHash("sha256").update(value).digest("hex")}`;

const immutableNamesByID = (
  source?: CompilerSource,
): ReadonlyMap<string, string> => {
  const result = new Map<string, string>();
  const visit = (node?: ASTNode): void => {
    if (!node) {
      return;
    }
    if (
      node.nodeType === "VariableDeclaration" &&
      node.stateVariable === true &&
      node.mutability === "immutable" &&
      node.id !== undefined &&
      node.name
    ) {
      result.set(String(node.id), node.name);
    }
    for (const child of node.nodes ?? []) {
      visit(child);
    }
  };
  visit(source?.ast);
  return result;
};

const namedImmutableReferences = (
  source: CompilerSource | undefined,
  references: Readonly<Record<string, readonly ImmutableReference[]>>,
): Readonly<Record<string, readonly ImmutableReference[]>> => {
  const names = immutableNamesByID(source);
  const named: Record<string, readonly ImmutableReference[]> = {};
  for (const [id, entries] of Object.entries(references)) {
    const name = names.get(id);
    if (!name || !requiredImmutables.some((required) => required === name)) {
      throw new Error(`Unknown immutable reference with AST ID ${id}`);
    }
    named[name] = entries;
  }
  for (const name of requiredImmutables) {
    if (!named[name]?.length) {
      throw new Error(`Compiler did not return immutable reference ${name}`);
    }
  }
  return named;
};

export const compileContracts = (
  outputDirectory: string,
  sourceDirectory = resolve("contracts"),
): readonly string[] => {
  const actualCompilerVersion = solc.version();
  if (actualCompilerVersion !== compilerVersion) {
    throw new Error(
      `Expected solc ${compilerVersion}, received ${actualCompilerVersion}`,
    );
  }

  const sources = Object.fromEntries(
    contracts.map(({ sourceName }) => [
      sourceName,
      { content: readFileSync(resolve(sourceDirectory, sourceName), "utf8") },
    ]),
  );
  const input = {
    language: "Solidity",
    settings: compilerSettings,
    sources,
  };
  const compilerInput = JSON.stringify(input);
  const output = JSON.parse(solc.compile(compilerInput)) as CompilerOutput;
  const errors = output.errors?.filter(({ severity }) => severity === "error");
  if (errors?.length) {
    throw new Error(
      errors.map(({ formattedMessage }) => formattedMessage).join("\n"),
    );
  }

  rmSync(outputDirectory, { force: true, recursive: true });
  mkdirSync(outputDirectory, { recursive: true });
  return contracts.map(({ contractName, sourceName }) => {
    const contract = output.contracts?.[sourceName]?.[contractName];
    if (!contract) {
      throw new Error(`Compiler did not return ${sourceName}:${contractName}`);
    }

    const sourceTree = Object.entries(sources)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(
        ([name, source]) =>
          `${name}\0${source.content.length}\0${source.content}`,
      )
      .join("\0");
    const artifact = {
      artifactVersion: "1",
      abi: contract.abi,
      bytecode: `0x${contract.evm.bytecode.object}`,
      compilerInputSha256: sha256(compilerInput),
      compilerVersion: actualCompilerVersion,
      contractName,
      deployedBytecode: `0x${contract.evm.deployedBytecode.object}`,
      immutableReferences: namedImmutableReferences(
        output.sources?.[sourceName],
        contract.evm.deployedBytecode.immutableReferences,
      ),
      settings: compilerSettings,
      sourceName,
      sourceTreeSha256: sha256(sourceTree),
    };
    const artifactPath = resolve(outputDirectory, `${contractName}.json`);
    writeFileSync(artifactPath, `${JSON.stringify(artifact, null, 2)}\n`, {
      encoding: "utf8",
      mode: 0o600,
    });
    return artifactPath;
  });
};

const scriptPath = process.argv[1];
if (scriptPath && import.meta.url === pathToFileURL(resolve(scriptPath)).href) {
  const outputDirectory = resolve("build/contracts");
  const artifactPaths = compileContracts(outputDirectory);
  console.log(`Contracts compiled: ${artifactPaths.length}`);
  console.log(`Artifacts: ${outputDirectory}`);
}
