import { createHash, randomUUID } from "node:crypto";
import {
  chmod,
  mkdir,
  open,
  readFile,
  realpath,
  rename,
  rm,
  stat,
} from "node:fs/promises";
import { basename, dirname, resolve } from "node:path";
import { pathToFileURL } from "node:url";

import {
  ContractFactory,
  FetchRequest,
  Interface,
  JsonRpcProvider,
  Wallet,
  getAddress,
  getBytes,
} from "ethers";

import {
  DeploymentInputError,
  createDeploymentManifest,
  linkRuntime,
  loadCanonicalArtifact,
  normalizeRoles,
  sourceProvenance,
} from "./deployment.js";
import { verifyCanonicalArtifact } from "./verifyArtifacts.js";

const canonicalArtifactPath = resolve("artifacts/contracts/RescuerV2.json");

interface CLIOptions {
  readonly broadcast: boolean;
  readonly chainId: bigint;
  readonly configKey?: string;
  readonly configPath?: string;
  readonly confirmations: number;
  readonly destination: string;
  readonly manifestPath?: string;
  readonly receiptTimeoutMS: number;
  readonly rpcTimeoutMS: number;
  readonly rpcURL?: string;
  readonly sponsor: string;
}

class DeploymentError extends Error {}

const optionNames = new Set([
  "--broadcast",
  "--chain-id",
  "--config",
  "--config-key",
  "--confirmations",
  "--destination",
  "--manifest",
  "--receipt-timeout-ms",
  "--rpc-timeout-ms",
  "--rpc-url",
  "--sponsor",
]);

const requiredValue = (
  values: ReadonlyMap<string, string>,
  name: string,
): string => {
  const value = values.get(name);
  if (!value) {
    throw new DeploymentInputError(`Не задан обязательный аргумент ${name}`);
  }
  return value;
};

const parsePositiveInteger = (value: string, name: string): number => {
  if (!/^[1-9][0-9]*$/.test(value)) {
    throw new DeploymentInputError(`${name} должен быть положительным целым`);
  }
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed)) {
    throw new DeploymentInputError(`${name} выходит за безопасный диапазон`);
  }
  return parsed;
};

export const parseCLIOptions = (args: readonly string[]): CLIOptions => {
  const values = new Map<string, string>();
  let broadcast = false;
  for (let index = 0; index < args.length; index++) {
    const name = args[index];
    if (!name || !optionNames.has(name)) {
      throw new DeploymentInputError(`Неизвестный аргумент ${name ?? ""}`);
    }
    if (name === "--broadcast") {
      if (broadcast) {
        throw new DeploymentInputError("Аргумент --broadcast указан повторно");
      }
      broadcast = true;
      continue;
    }
    if (values.has(name)) {
      throw new DeploymentInputError(`Аргумент ${name} указан повторно`);
    }
    const value = args[index + 1];
    if (!value || value.startsWith("--")) {
      throw new DeploymentInputError(`Для ${name} не задано значение`);
    }
    values.set(name, value);
    index++;
  }

  const chainIDValue = requiredValue(values, "--chain-id");
  if (!/^[1-9][0-9]*$/.test(chainIDValue)) {
    throw new DeploymentInputError(
      "--chain-id должен быть положительным decimal",
    );
  }
  const chainId = BigInt(chainIDValue);
  const configPath = values.get("--config");
  const configKey = values.get("--config-key");
  if ((!configPath && configKey) || (configPath && !configKey)) {
    throw new DeploymentInputError(
      "--config и --config-key указываются вместе",
    );
  }
  if (configKey && !/^[A-Z][A-Z0-9_]*$/.test(configKey)) {
    throw new DeploymentInputError("--config-key имеет недопустимый формат");
  }
  const rpcURL = values.get("--rpc-url");
  const manifestPath = values.get("--manifest");
  if (broadcast && (!rpcURL || !manifestPath)) {
    throw new DeploymentInputError(
      "Для --broadcast обязательны --rpc-url и --manifest",
    );
  }

  const destination = requiredValue(values, "--destination");
  const sponsor = requiredValue(values, "--sponsor");
  normalizeRoles(destination, sponsor, sponsor);
  return {
    broadcast,
    chainId,
    configKey,
    configPath: configPath ? resolve(configPath) : undefined,
    confirmations: parsePositiveInteger(
      values.get("--confirmations") ?? "1",
      "--confirmations",
    ),
    destination: getAddress(destination),
    manifestPath: manifestPath ? resolve(manifestPath) : undefined,
    receiptTimeoutMS: parsePositiveInteger(
      values.get("--receipt-timeout-ms") ?? "120000",
      "--receipt-timeout-ms",
    ),
    rpcTimeoutMS: parsePositiveInteger(
      values.get("--rpc-timeout-ms") ?? "10000",
      "--rpc-timeout-ms",
    ),
    rpcURL,
    sponsor: getAddress(sponsor),
  };
};

interface PathIdentity {
  readonly canonical: string;
  readonly device?: bigint;
  readonly inode?: bigint;
}

const canonicalPath = async (path: string): Promise<string> => {
  let current = resolve(path);
  const suffix: string[] = [];
  while (true) {
    try {
      return resolve(await realpath(current), ...suffix);
    } catch (error) {
      if (
        !error ||
        typeof error !== "object" ||
        !("code" in error) ||
        error.code !== "ENOENT"
      ) {
        throw new DeploymentError("Не удалось проверить output path");
      }
      const parent = dirname(current);
      if (parent === current) {
        throw new DeploymentError("Не удалось проверить output path");
      }
      suffix.unshift(basename(current));
      current = parent;
    }
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
    if (
      error &&
      typeof error === "object" &&
      "code" in error &&
      error.code === "ENOENT"
    ) {
      return { canonical };
    }
    throw new DeploymentError("Не удалось проверить output path");
  }
};

const verifyDistinctPaths = async (options: CLIOptions): Promise<void> => {
  if (!options.broadcast || !options.manifestPath) {
    return;
  }
  const paths = [canonicalArtifactPath, options.manifestPath];
  if (options.configPath) {
    paths.push(options.configPath);
  }
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
          "Artifact, manifest и operator config должны быть разными файлами",
        );
      }
    }
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

const atomicWrite = async (path: string, content: string): Promise<void> => {
  const directory = dirname(path);
  await mkdir(directory, { recursive: true });
  const temporary = resolve(
    directory,
    `.${basename(path)}.tmp.${process.pid}.${randomUUID()}`,
  );
  let handle: Awaited<ReturnType<typeof open>> | undefined;
  try {
    handle = await open(temporary, "wx", 0o600);
    await handle.writeFile(content, "utf8");
    await handle.sync();
    await handle.close();
    handle = undefined;
    await rename(temporary, path);
    await chmod(path, 0o600);
  } finally {
    await handle?.close().catch(() => undefined);
    await rm(temporary, { force: true }).catch(() => undefined);
  }
};

const updateConfig = async (
  path: string,
  key: string,
  address: string,
): Promise<void> => {
  let content: string;
  try {
    content = await readFile(path, "utf8");
  } catch {
    throw new DeploymentError("Operator config не удалось безопасно прочитать");
  }
  const lines = content.split(/\r?\n/);
  const matching = lines
    .map((line, index) => ({ index, line }))
    .filter(({ line }) => line.startsWith(`${key}=`));
  if (matching.length > 1) {
    throw new DeploymentError("Operator config содержит повторяющийся ключ");
  }
  if (matching.length === 1) {
    lines[matching[0]!.index] = `${key}=${address}`;
  } else {
    if (lines.at(-1) !== "") {
      lines.push("");
    }
    lines.push(`${key}=${address}`, "");
  }
  await atomicWrite(path, lines.join("\n"));
};

const decodeGetter = (data: string, name: string): string => {
  if (!/^0x0{24}[0-9a-fA-F]{40}$/.test(data)) {
    throw new DeploymentError(`Getter ${name} вернул неканонический адрес`);
  }
  return getAddress(`0x${data.slice(-40)}`);
};

export const runDeployment = async (
  options: CLIOptions,
  environment: NodeJS.ProcessEnv = process.env,
): Promise<void> => {
  await verifyDistinctPaths(options);
  await verifyCanonicalArtifact(canonicalArtifactPath);
  const loaded = await loadCanonicalArtifact(canonicalArtifactPath);
  const factory = new ContractFactory(
    loaded.artifact.abi,
    loaded.artifact.bytecode,
  );
  const planned = await factory.getDeployTransaction(
    options.destination,
    options.sponsor,
  );
  if (!planned.data) {
    throw new DeploymentError("Artifact не сформировал deployment data");
  }
  const planDigest = createHash("sha256")
    .update(getBytes(planned.data))
    .digest("hex");
  sourceProvenance(loaded.artifact);

  if (!options.broadcast) {
    console.log("Локальный план RescuerV2 проверен; отправка отключена.");
    console.log(`SHA-256 deployment data: ${planDigest}`);
    return;
  }

  const request = new FetchRequest(options.rpcURL!);
  request.timeout = options.rpcTimeoutMS;
  const provider = new JsonRpcProvider(request);
  try {
    const network = await provider.getNetwork();
    if (network.chainId !== options.chainId) {
      throw new DeploymentError("RPC вернул chain ID, отличный от ожидаемого");
    }

    const privateKey = environment["DEPLOYER_PRIVATE_KEY"];
    if (!privateKey) {
      throw new DeploymentError(
        "Для явного --broadcast не задан DEPLOYER_PRIVATE_KEY",
      );
    }
    let wallet: Wallet;
    try {
      wallet = new Wallet(privateKey, provider);
    } catch {
      throw new DeploymentError(
        "DEPLOYER_PRIVATE_KEY имеет недопустимый формат",
      );
    }
    if (wallet.address !== options.sponsor) {
      throw new DeploymentError(
        "Адрес deployment signer не совпадает со sponsor",
      );
    }

    const sendingFactory = new ContractFactory(
      loaded.artifact.abi,
      loaded.artifact.bytecode,
      wallet,
    );
    const contract = await sendingFactory.deploy(
      options.destination,
      options.sponsor,
    );
    const transaction = contract.deploymentTransaction();
    if (!transaction || transaction.data !== planned.data) {
      throw new DeploymentError(
        "Отправленная транзакция не совпадает с проверенным планом",
      );
    }
    const receipt = await withDeadline(
      transaction.wait(options.confirmations),
      options.receiptTimeoutMS,
      "Истёк таймаут receipt deployment",
    );
    if (!receipt || receipt.status !== 1 || !receipt.contractAddress) {
      throw new DeploymentError("Deployment не получил успешный receipt");
    }

    const address = getAddress(receipt.contractAddress);
    const block = await provider.getBlock(receipt.blockHash);
    if (!block || block.hash !== receipt.blockHash) {
      throw new DeploymentError("RPC не подтвердил deployment block hash");
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
        "Deployed runtime не совпадает с canonical artifact",
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
        throw new DeploymentError(`Getter ${name} не совпадает с планом`);
      }
    }

    const manifest = createDeploymentManifest({
      address,
      artifact: loaded,
      blockHash: receipt.blockHash,
      blockNumber: BigInt(receipt.blockNumber),
      chainId: options.chainId,
      destination: options.destination,
      source: sourceProvenance(loaded.artifact),
      sponsor: options.sponsor,
      transactionHash: transaction.hash,
    });
    await atomicWrite(
      options.manifestPath!,
      `${JSON.stringify(manifest, null, 2)}\n`,
    );
    if (options.configPath && options.configKey) {
      await updateConfig(options.configPath, options.configKey, address);
    }
    console.log("Deployment, runtime и immutable параметры проверены.");
    console.log("Manifest опубликован атомарно; live defaults не изменялись.");
  } finally {
    await provider.destroy();
  }
};

const main = async (): Promise<void> => {
  try {
    await runDeployment(parseCLIOptions(process.argv.slice(2)));
  } catch (error) {
    if (
      error instanceof DeploymentInputError ||
      error instanceof DeploymentError
    ) {
      console.error(error.message);
    } else {
      console.error("Критическая ошибка deployment; результат не опубликован");
    }
    process.exitCode = 1;
  }
};

const scriptPath = process.argv[1];
if (scriptPath && import.meta.url === pathToFileURL(resolve(scriptPath)).href) {
  await main();
}
