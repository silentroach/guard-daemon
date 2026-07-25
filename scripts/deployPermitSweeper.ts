/**
 * scripts/deployPermitSweeper.ts
 * 
 * Компилирует и деплоит PermitSweeper на все сети одновременно.
 * 
 * Требования:
 *   npm install ethers solc dotenv
 * 
 * Usage:
 *   npx tsx scripts/deployPermitSweeper.ts
 * 
 * Результат: адреса контрактов на каждой сети
 */

import { ethers } from "ethers";
import solc from "solc";
import { readFileSync, writeFileSync } from "fs";
import { resolve } from "path";
import * as dotenv from "dotenv";

dotenv.config();

// ─────────────────────────────────────────────────────────────────────────────
// Network configuration
// ─────────────────────────────────────────────────────────────────────────────

interface NetworkConfig {
  name: string;
  rpc: string;
  chainId: number;
  envKey: string; // key в .env куда сохранять адрес
}

const NETWORKS: NetworkConfig[] = [
  {
    name: "Base",
    rpc: "https://mainnet.base.org",
    chainId: 8453,
    envKey: "PERMIT_SWEEPER_BASE",
  },
  {
    name: "Ethereum",
    rpc: "https://ethereum.publicnode.com",
    chainId: 1,
    envKey: "PERMIT_SWEEPER_ETHEREUM",
  },
  {
    name: "Arbitrum",
    rpc: "https://arb1.arbitrum.io/rpc",
    chainId: 42161,
    envKey: "PERMIT_SWEEPER_ARBITRUM",
  },
  {
    name: "Optimism",
    rpc: "https://mainnet.optimism.io",
    chainId: 10,
    envKey: "PERMIT_SWEEPER_OPTIMISM",
  },
  {
    name: "Polygon",
    rpc: "https://polygon.publicnode.com",
    chainId: 137,
    envKey: "PERMIT_SWEEPER_POLYGON",
  },
  {
    name: "Ink",
    rpc: "https://rpc-gel.inkonchain.com",
    chainId: 57073,
    envKey: "PERMIT_SWEEPER_INK",
  },
];

// ─────────────────────────────────────────────────────────────────────────────
// Compile PermitSweeper
// ─────────────────────────────────────────────────────────────────────────────

function compileContract(): { bytecode: string; abi: any[] } {
  console.log("📦 Compiling PermitSweeper.sol...\n");

  const source = readFileSync(resolve("./contracts/PermitSweeper.sol"), "utf-8");

  const input = {
    language: "Solidity",
    sources: {
      "PermitSweeper.sol": { content: source },
    },
    settings: {
      optimizer: { enabled: true, runs: 200 },
      outputSelection: {
        "PermitSweeper.sol": {
          PermitSweeper: ["evm.bytecode.object", "abi"],
        },
      },
    },
  };

  const output = JSON.parse(solc.compile(JSON.stringify(input)));

  // Check for errors
  if (output.errors?.length) {
    for (const err of output.errors) {
      if (err.severity === "error") {
        console.error("❌ Compilation error:");
        console.error(err.formattedMessage);
        process.exit(1);
      }
    }
  }

  const contract = output.contracts?.["PermitSweeper.sol"]?.["PermitSweeper"];
  if (!contract) {
    console.error("❌ Contract not found in compilation output");
    process.exit(1);
  }

  const bytecode = contract.evm.bytecode.object;
  const abi = contract.abi;

  console.log(`✅ Compiled: ${bytecode.length / 2} bytes`);
  console.log(`✅ ABI: ${abi.length} items\n`);

  return { bytecode: `0x${bytecode}`, abi };
}

// ─────────────────────────────────────────────────────────────────────────────
// Deploy on one network
// ─────────────────────────────────────────────────────────────────────────────

async function deployOnNetwork(
  network: NetworkConfig,
  bytecode: string,
  abi: any[]
): Promise<string | null> {
  try {
    console.log(`\n🚀 Deploying on ${network.name} (Chain ${network.chainId})...`);

    // Get sponsor private key
    const sponsorKey = process.env.SPONSOR_PRIVATE_KEY;
    if (!sponsorKey) {
      console.error("❌ SPONSOR_PRIVATE_KEY not set in .env");
      return null;
    }

    // Connect to network
    const provider = new ethers.JsonRpcProvider(network.rpc);
    const signer = new ethers.Wallet(sponsorKey, provider);

    console.log(`   Signer: ${signer.address}`);

    // Check balance
    const balance = await provider.getBalance(signer.address);
    const balanceEth = ethers.formatEther(balance);
    console.log(`   Balance: ${balanceEth} ETH`);

    if (parseFloat(balanceEth) < 0.0001) {
      console.warn(`   ⚠️  Low balance! Need at least 0.001 ETH for gas`);
      return null;
    }

    // Estimate gas
    const tx = {
      data: bytecode,
    };

    let gasEstimate;
    try {
      gasEstimate = await provider.estimateGas(tx);
    } catch (e) {
      // Fallback estimate
      gasEstimate = BigInt(500000);
      console.log(`   ⚠️  Using fallback gas estimate: ${gasEstimate}`);
    }

    // Get gas price
    const feeData = await provider.getFeeData();
    if (!feeData.gasPrice) {
      console.error(`   ❌ Could not get gas price`);
      return null;
    }

    const gasPrice = feeData.gasPrice;
    const estimatedCost = (gasEstimate * gasPrice) / BigInt(10 ** 18);
    console.log(`   Gas estimate: ${gasEstimate.toString()}`);
    console.log(`   Gas price: ${ethers.formatUnits(gasPrice, "gwei")} gwei`);
    console.log(`   Estimated cost: ${ethers.formatEther(estimatedCost)} ETH`);

    // Deploy
    const factory = new ethers.ContractFactory(abi, bytecode, signer);
    const contract = await factory.deploy();

    const deployTx = contract.deploymentTransaction();
    if (!deployTx) {
      console.error(`   ❌ No deployment tx`);
      return null;
    }

    console.log(`   📝 Tx: ${deployTx.hash}`);
    console.log(`   ⏳ Waiting for confirmation...`);

    const receipt = await deployTx.wait(2);
    if (!receipt || !receipt.contractAddress) {
      console.error(`   ❌ Deployment failed or no address`);
      return null;
    }

    const address = receipt.contractAddress;
    console.log(`   ✅ Deployed at: ${address}`);

    // Verify it's actually deployed
    const code = await provider.getCode(address);
    if (code === "0x") {
      console.error(`   ❌ Code not found at address (deployment may have failed)`);
      return null;
    }

    return address;
  } catch (error: any) {
    console.error(`   ❌ Error: ${error.message}`);
    return null;
  }
}

// ─────────────────────────────────────────────────────────────────────────────
// Save addresses to .env
// ─────────────────────────────────────────────────────────────────────────────

function saveAddressesToEnv(addresses: Map<string, string>) {
  let envContent = readFileSync(".env", "utf-8");

  for (const [key, address] of addresses) {
    const pattern = new RegExp(`^${key}=.*$`, "m");
    const line = `${key}=${address}`;

    if (pattern.test(envContent)) {
      envContent = envContent.replace(pattern, line);
    } else {
      envContent += `\n${line}`;
    }
  }

  writeFileSync(".env", envContent);
  console.log("\n✅ Saved to .env");
}

// ─────────────────────────────────────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────────────────────────────────────

async function main() {
  console.log("═══════════════════════════════════════════════════════");
  console.log("  PermitSweeper Multi-Network Deployer");
  console.log("═══════════════════════════════════════════════════════");

  // Compile
  const { bytecode, abi } = compileContract();

  // Deploy on all networks
  const addresses = new Map<string, string>();
  const results: { network: string; address: string | null }[] = [];

  for (const network of NETWORKS) {
    const address = await deployOnNetwork(network, bytecode, abi);
    results.push({ network: network.name, address });

    if (address) {
      addresses.set(network.envKey, address);
    }
  }

  // Summary
  console.log("\n" + "═".repeat(60));
  console.log("📊 DEPLOYMENT SUMMARY");
  console.log("═".repeat(60));

  for (const result of results) {
    const status = result.address ? "✅" : "❌";
    console.log(`${status} ${result.network.padEnd(12)} ${result.address || "(failed)"}`);
  }

  if (addresses.size > 0) {
    saveAddressesToEnv(addresses);
    console.log("\n✅ Ready to use in Go sweeper daemon!");
    console.log("   Run: .\\ sweeper.exe");
  } else {
    console.log("\n❌ No successful deployments");
  }

  console.log("═".repeat(60) + "\n");
}

main().catch((err) => {
  console.error("Fatal error:", err);
  process.exit(1);
});
