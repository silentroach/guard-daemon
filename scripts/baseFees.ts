import { Interface, type JsonRpcProvider } from "ethers";

const oracle = "0x420000000000000000000000000000000000000F";
const codec = new Interface([
  "function getL1FeeUpperBound(uint256) view returns (uint256)",
  "function getOperatorFee(uint256) view returns (uint256)",
]);

interface BaseFeeProvider {
  readonly getBlock: (
    tag: "latest",
  ) => Promise<{ readonly hash: string | null } | null>;
  readonly call: JsonRpcProvider["call"];
}

// A block-pinned estimate; it is not a protocol-enforced total fee cap.
export const estimateBaseDeploymentFees = async (
  provider: BaseFeeProvider,
  creationBytes: number,
  gasLimit: bigint,
): Promise<bigint> => {
  if (
    !Number.isSafeInteger(creationBytes) ||
    creationBytes <= 0 ||
    creationBytes > 128 * 1024 ||
    gasLimit <= 0n
  ) {
    throw new Error("Invalid Base deployment fee request");
  }
  const block = await provider.getBlock("latest");
  const blockHash = block?.hash;
  if (!blockHash) throw new Error("Base fee estimate requires a block hash");
  const read = async (method: string, value: bigint): Promise<bigint> => {
    const result = await provider.call({
      to: oracle,
      data: codec.encodeFunctionData(method, [value]),
      blockTag: blockHash,
      gasLimit: 200_000n,
    });
    if (!/^0x[0-9a-fA-F]{64}$/.test(result))
      throw new Error("Malformed Base fee response");
    return BigInt(result);
  };
  const l1 = await read("getL1FeeUpperBound", BigInt(creationBytes + 512));
  const operator = await read("getOperatorFee", gasLimit);
  const total = l1 + operator;
  if (total >= 1n << 256n) throw new Error("Base fee estimate exceeds uint256");
  return total;
};
