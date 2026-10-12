package contracts_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"guard-daemon/internal/contracts"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
)

const testCompilerVersion = "0.8.36+commit.8a079791.Emscripten.clang"

type attestationFixture struct {
	manifest         []byte
	artifact         []byte
	expectations     contracts.ManifestExpectations
	trusted          contracts.DeploymentManifest
	runtime          []byte
	address          common.Address
	destination      common.Address
	sponsor          common.Address
	deploymentHeader *types.Header
	commonHeader     *types.Header
	transaction      *types.Transaction
	receipt          *types.Receipt
	deploymentData   []byte
	sponsorKey       *ecdsa.PrivateKey
	deploymentNonce  uint64
}

type artifactJSON struct {
	ArtifactVersion     string                         `json:"artifactVersion"`
	ContractName        string                         `json:"contractName"`
	SourceName          string                         `json:"sourceName"`
	CompilerVersion     string                         `json:"compilerVersion"`
	Settings            json.RawMessage                `json:"settings"`
	SourceTreeSHA256    string                         `json:"sourceTreeSha256"`
	CompilerInputSHA256 string                         `json:"compilerInputSha256"`
	ABI                 json.RawMessage                `json:"abi"`
	Bytecode            string                         `json:"bytecode"`
	DeployedBytecode    string                         `json:"deployedBytecode"`
	ImmutableReferences map[string][]artifactReference `json:"immutableReferences"`
}

type artifactReference struct {
	Start  uint64 `json:"start"`
	Length uint64 `json:"length"`
}

type fakeAttestationReader struct {
	chainID           *big.Int
	finalized         *types.Header
	headers           map[string]*types.Header
	code              []byte
	getters           map[string][]byte
	expectedAddress   common.Address
	expectedStateHash common.Hash
	transaction       *types.Transaction
	pending           bool
	receipt           *types.Receipt
	failStage         string
	blockStage        string
	calls             int
	missingDeadline   bool
}

func TestLoadTrustedManifest(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	manifest := fixture.trusted
	if manifest.Address != fixture.address || manifest.Immutables.Destination != fixture.destination || manifest.Immutables.Sponsor != fixture.sponsor {
		t.Fatalf("trusted manifest addresses = %s, %s, %s", manifest.Address, manifest.Immutables.Destination, manifest.Immutables.Sponsor)
	}
	if manifest.Runtime.ByteLength != uint64(len(fixture.runtime)) || manifest.Runtime.Keccak256 != crypto.Keccak256Hash(fixture.runtime).Hex() {
		t.Fatalf("trusted runtime identifier = %d, %s", manifest.Runtime.ByteLength, manifest.Runtime.Keccak256)
	}
}

func TestAttestDeploymentValidQuorum(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	first := fixture.reader(fixture.commonHeader)
	second := fixture.reader(fixture.commonHeader)
	providers := []contracts.ReadProvider{testProvider("archive-a", first), testProvider("archive-b", second)}

	if err := contracts.AttestDeployment(context.Background(), fixture.trusted, providers, time.Second); err != nil {
		t.Fatal(err)
	}
	for _, reader := range []*fakeAttestationReader{first, second} {
		if reader.missingDeadline || reader.calls != 9 {
			t.Fatalf("network calls = %d, missing deadline = %t", reader.calls, reader.missingDeadline)
		}
	}
}

func TestAttestDeploymentAcceptsDifferentFinalizedHeightsWithCommonHash(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	higherFinalized := testHeader(21, 0x21)
	first := fixture.reader(fixture.commonHeader)
	second := fixture.reader(higherFinalized)
	providers := []contracts.ReadProvider{testProvider("archive-a", first), testProvider("archive-b", second)}

	if err := contracts.AttestDeployment(context.Background(), fixture.trusted, providers, time.Second); err != nil {
		t.Fatal(err)
	}
	if first.calls != 10 || second.calls != 10 {
		t.Fatalf("network calls = %d, %d, want 10 per provider", first.calls, second.calls)
	}
}

func TestAttestDeploymentRejectsFinalizedHashChangedDuringCommonRead(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	changedLowest := testHeader(fixture.commonHeader.Number.Int64(), 0xfc)
	higherFinalized := testHeader(21, 0x21)
	if err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
		testProvider("archive-a", fixture.reader(changedLowest)),
		testProvider("archive-b", fixture.reader(higherFinalized)),
	}, time.Second); err == nil {
		t.Fatal("provider that changed its finalized hash accepted")
	}
}

func TestAttestDeploymentRejectsRuntimeEvenWithCorrectDestination(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	first := fixture.reader(fixture.commonHeader)
	second := fixture.reader(fixture.commonHeader)
	second.code = append([]byte(nil), second.code...)
	second.code[0] ^= 0x01

	err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
		testProvider("archive-a", first),
		testProvider("archive-b", second),
	}, time.Second)
	if err == nil {
		t.Fatal("incorrect runtime accepted")
	}
}

func TestAttestDeploymentRejectsMissingSponsorGetterWithoutLeakingRPCError(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	legacy := fixture.reader(fixture.commonHeader)
	legacy.failStage = "sponsor"
	err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
		testProvider("archive-a", fixture.reader(fixture.commonHeader)),
		testProvider("archive-b", legacy),
	}, time.Second)
	if err == nil {
		t.Fatal("stale reader method set accepted")
	}
	if strings.Contains(err.Error(), "private.invalid") || strings.Contains(err.Error(), "credential") {
		t.Fatalf("operator error exposed RPC details: %v", err)
	}
}

func TestAttestDeploymentRejectsWrongChainFromOneProvider(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	wrongChain := fixture.reader(fixture.commonHeader)
	wrongChain.chainID = big.NewInt(31338)
	err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
		testProvider("archive-a", fixture.reader(fixture.commonHeader)),
		testProvider("archive-b", wrongChain),
	}, time.Second)
	if err == nil {
		t.Fatal("incorrect network accepted")
	}
}

func TestAttestDeploymentRejectsDivergentFinalizedHash(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	divergent := testHeader(fixture.commonHeader.Number.Int64(), 0xff)
	err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
		testProvider("archive-a", fixture.reader(fixture.commonHeader)),
		testProvider("archive-b", fixture.reader(divergent)),
	}, time.Second)
	if err == nil {
		t.Fatal("divergent finalized hashes accepted")
	}
}

func TestAttestDeploymentRejectsMissingCode(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	missing := fixture.reader(fixture.commonHeader)
	missing.code = nil
	err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
		testProvider("archive-a", fixture.reader(fixture.commonHeader)),
		testProvider("archive-b", missing),
	}, time.Second)
	if err == nil {
		t.Fatal("missing runtime code accepted")
	}
}

func TestAttestDeploymentRejectsWrongDeploymentBlockHash(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	wrongDeployment := fixture.reader(fixture.commonHeader)
	wrongDeployment.headers[fixture.deploymentHeader.Number.String()] = testHeader(10, 0xfe)
	if err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
		testProvider("archive-a", fixture.reader(fixture.commonHeader)),
		testProvider("archive-b", wrongDeployment),
	}, time.Second); err == nil {
		t.Fatal("incorrect deployment block hash accepted")
	}
}

func TestAttestDeploymentRejectsTwoHashesAtSameFinalizedHeight(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	conflictingFinalized := testHeader(fixture.deploymentHeader.Number.Int64(), 0xfd)
	first := fixture.reader(conflictingFinalized)
	second := fixture.reader(conflictingFinalized)
	first.expectedStateHash = conflictingFinalized.Hash()
	second.expectedStateHash = conflictingFinalized.Hash()
	if err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
		testProvider("archive-a", first),
		testProvider("archive-b", second),
	}, time.Second); err == nil {
		t.Fatal("different finalization and deployment hashes at the same height accepted")
	}
}

func TestAttestDeploymentRejectsTamperedTransactionHash(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	tamperedHash := common.HexToHash("0xdead")
	trusted := fixture.trustedWithTransactionHash(t, tamperedHash)
	if err := contracts.AttestDeployment(context.Background(), trusted, []contracts.ReadProvider{
		testProvider("archive-a", fixture.reader(fixture.commonHeader)),
		testProvider("archive-b", fixture.reader(fixture.commonHeader)),
	}, time.Second); err == nil {
		t.Fatal("manifest with a transaction hash unrelated to the returned transaction accepted")
	}
}

func TestAttestDeploymentRejectsInvalidDeploymentTransaction(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*testing.T, *attestationFixture) *types.Transaction{
		"wrong data": func(t *testing.T, fixture *attestationFixture) *types.Transaction {
			data := append([]byte(nil), fixture.deploymentData...)
			data[0] ^= 0x01
			return signedDeploymentTransaction(t, fixture.sponsorKey, big.NewInt(31337), fixture.deploymentNonce, data)
		},
		"wrong sender": func(t *testing.T, fixture *attestationFixture) *types.Transaction {
			otherKey := deterministicSigningKey(t, "other-deployment-sender")
			return signedDeploymentTransaction(t, otherKey, big.NewInt(31337), fixture.deploymentNonce, fixture.deploymentData)
		},
		"wrong transaction chain": func(t *testing.T, fixture *attestationFixture) *types.Transaction {
			return signedDeploymentTransaction(t, fixture.sponsorKey, big.NewInt(31338), fixture.deploymentNonce, fixture.deploymentData)
		},
	}
	for name, transactionFor := range tests {
		name, transactionFor := name, transactionFor
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAttestationFixture(t)
			transaction := transactionFor(t, fixture)
			trusted := fixture.trustedWithTransactionHash(t, transaction.Hash())
			first := fixture.reader(fixture.commonHeader)
			second := fixture.reader(fixture.commonHeader)
			first.transaction = transaction
			second.transaction = transaction
			if err := contracts.AttestDeployment(context.Background(), trusted, []contracts.ReadProvider{
				testProvider("archive-a", first),
				testProvider("archive-b", second),
			}, time.Second); err == nil {
				t.Fatal("invalid deployment transaction accepted")
			}
		})
	}
}

func TestAttestDeploymentRejectsPendingDeploymentTransaction(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	pending := fixture.reader(fixture.commonHeader)
	pending.pending = true
	if err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
		testProvider("archive-a", fixture.reader(fixture.commonHeader)),
		testProvider("archive-b", pending),
	}, time.Second); err == nil {
		t.Fatal("pending deployment transaction accepted")
	}
}

func TestAttestDeploymentRejectsFailedOrMismatchedReceipt(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*types.Receipt){
		"failed": func(receipt *types.Receipt) {
			receipt.Status = types.ReceiptStatusFailed
		},
		"wrong transaction hash": func(receipt *types.Receipt) {
			receipt.TxHash = common.HexToHash("0xbad1")
		},
		"wrong contract address": func(receipt *types.Receipt) {
			receipt.ContractAddress = deterministicTestAddress(0xe4)
		},
		"wrong block hash": func(receipt *types.Receipt) {
			receipt.BlockHash = common.HexToHash("0xbad2")
		},
		"old unrelated deployment block": func(receipt *types.Receipt) {
			oldHeader := testHeader(9, 0x09)
			receipt.BlockNumber = new(big.Int).Set(oldHeader.Number)
			receipt.BlockHash = oldHeader.Hash()
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAttestationFixture(t)
			byzantine := fixture.reader(fixture.commonHeader)
			receipt := *fixture.receipt
			receipt.BlockNumber = new(big.Int).Set(fixture.receipt.BlockNumber)
			mutate(&receipt)
			byzantine.receipt = &receipt
			if err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
				testProvider("archive-a", fixture.reader(fixture.commonHeader)),
				testProvider("archive-b", byzantine),
			}, time.Second); err == nil {
				t.Fatal("failed or mismatched deployment receipt accepted")
			}
		})
	}
}

func TestAttestDeploymentRejectsOldManifestBlockUnrelatedToReceipt(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	oldHeader := testHeader(9, 0x09)
	trusted := fixture.trustedWithDeploymentBlock(t, oldHeader)
	first := fixture.reader(fixture.commonHeader)
	second := fixture.reader(fixture.commonHeader)
	first.headers[oldHeader.Number.String()] = oldHeader
	second.headers[oldHeader.Number.String()] = oldHeader
	if err := contracts.AttestDeployment(context.Background(), trusted, []contracts.ReadProvider{
		testProvider("archive-a", first),
		testProvider("archive-b", second),
	}, time.Second); err == nil {
		t.Fatal("old manifest block unrelated to the deployment receipt accepted")
	}
}

func TestAttestDeploymentRejectsWrongGettersAndMinorityDivergence(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*fakeAttestationReader){
		"destination": func(reader *fakeAttestationReader) {
			reader.getters["destination"] = encodedAddress(deterministicTestAddress(0xe1))
		},
		"sponsor": func(reader *fakeAttestationReader) {
			reader.getters["sponsor"] = encodedAddress(deterministicTestAddress(0xe2))
		},
		"self": func(reader *fakeAttestationReader) {
			reader.getters["self"] = encodedAddress(deterministicTestAddress(0xe3))
		},
		"noncanonical": func(reader *fakeAttestationReader) {
			reader.getters["destination"] = append(encodedAddress(reader.getterAddress("destination")), 0)
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAttestationFixture(t)
			byzantine := fixture.reader(fixture.commonHeader)
			mutate(byzantine)
			providers := []contracts.ReadProvider{
				testProvider("archive-a", fixture.reader(fixture.commonHeader)),
				testProvider("archive-b", fixture.reader(fixture.commonHeader)),
				testProvider("archive-c", byzantine),
			}
			if err := contracts.AttestDeployment(context.Background(), fixture.trusted, providers, time.Second); err == nil {
				t.Fatal("two-of-three result accepted despite a divergent provider")
			}
		})
	}
}

func TestAttestDeploymentRejectsTimeoutAndCancellation(t *testing.T) {
	t.Parallel()

	t.Run("timeout", func(t *testing.T) {
		fixture := newAttestationFixture(t)
		blocked := fixture.reader(fixture.commonHeader)
		blocked.blockStage = "chainID"
		started := time.Now()
		err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
			testProvider("archive-a", blocked),
			testProvider("archive-b", fixture.reader(fixture.commonHeader)),
		}, 10*time.Millisecond)
		if err == nil {
			t.Fatal("timed-out provider accepted")
		}
		if time.Since(started) > time.Second {
			t.Fatal("provider timeout was not bounded")
		}
	})

	t.Run("cancelled parent", func(t *testing.T) {
		fixture := newAttestationFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := contracts.AttestDeployment(ctx, fixture.trusted, []contracts.ReadProvider{
			testProvider("archive-a", fixture.reader(fixture.commonHeader)),
			testProvider("archive-b", fixture.reader(fixture.commonHeader)),
		}, time.Second)
		if err == nil {
			t.Fatal("canceled attestation accepted")
		}
	})

	t.Run("nonpositive timeout", func(t *testing.T) {
		fixture := newAttestationFixture(t)
		err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
			testProvider("archive-a", fixture.reader(fixture.commonHeader)),
			testProvider("archive-b", fixture.reader(fixture.commonHeader)),
		}, 0)
		if err == nil {
			t.Fatal("non-positive timeout accepted")
		}
	})
}

func TestAttestDeploymentRejectsInvalidProviderSet(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	reader := fixture.reader(fixture.commonHeader)
	secondReader := fixture.reader(fixture.commonHeader)
	one := testProvider("archive-a", reader)
	two := testProvider("archive-b", secondReader)
	duplicateID := two
	duplicateID.ID = one.ID
	blankID := two
	blankID.ID = " "
	blankEndpoint := two
	blankEndpoint.EndpointFingerprint = " "
	blankDomain := two
	blankDomain.TrustDomain = " "
	duplicateEndpoint := two
	duplicateEndpoint.EndpointFingerprint = one.EndpointFingerprint
	duplicateDomain := two
	duplicateDomain.TrustDomain = one.TrustDomain
	sameReader := testProvider("archive-b", reader)
	tests := map[string][]contracts.ReadProvider{
		"one provider":                   {one},
		"duplicate IDs":                  {one, duplicateID},
		"blank ID":                       {one, blankID},
		"blank endpoint fingerprint":     {one, blankEndpoint},
		"blank trust domain":             {one, blankDomain},
		"duplicate endpoint fingerprint": {one, duplicateEndpoint},
		"duplicate trust domain":         {one, duplicateDomain},
		"same reader different metadata": {one, sameReader},
		"nil reader":                     {one, {ID: "archive-b", EndpointFingerprint: "test-endpoint-b", TrustDomain: "test-domain-b"}},
	}
	for name, providers := range tests {
		name, providers := name, providers
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := contracts.AttestDeployment(context.Background(), fixture.trusted, providers, time.Second)
			if err == nil {
				t.Fatal("invalid provider set accepted")
			}
			for _, provider := range providers {
				for _, opaque := range []string{provider.EndpointFingerprint, provider.TrustDomain} {
					if strings.TrimSpace(opaque) != "" && strings.Contains(err.Error(), opaque) {
						t.Fatalf("operator error exposed opaque provider metadata: %v", err)
					}
				}
			}
		})
	}
}

func TestLoadTrustedManifestRejectsManifestTampering(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*testing.T, *attestationFixture) ([]byte, contracts.ManifestExpectations){
		"artifact digest": func(t *testing.T, fixture *attestationFixture) ([]byte, contracts.ManifestExpectations) {
			object := decodeObject(t, fixture.manifest)
			mustObject(t, object["artifact"])["sha256"] = "sha256:" + strings.Repeat("0", 64)
			return encodeJSON(t, object), fixture.expectations
		},
		"runtime hash": func(t *testing.T, fixture *attestationFixture) ([]byte, contracts.ManifestExpectations) {
			object := decodeObject(t, fixture.manifest)
			mustObject(t, object["runtime"])["keccak256"] = "0x" + strings.Repeat("0", 64)
			return encodeJSON(t, object), fixture.expectations
		},
		"immutable self": func(t *testing.T, fixture *attestationFixture) ([]byte, contracts.ManifestExpectations) {
			object := decodeObject(t, fixture.manifest)
			mustObject(t, object["immutables"])["self"] = deterministicTestAddress(0xee).Hex()
			return encodeJSON(t, object), fixture.expectations
		},
		"stale source": func(t *testing.T, fixture *attestationFixture) ([]byte, contracts.ManifestExpectations) {
			object := decodeObject(t, fixture.manifest)
			stale := "sha256:" + strings.Repeat("b", 64)
			mustObject(t, object["source"])["value"] = stale
			expectations := fixture.expectations
			expectations.SourceProvenance.Value = stale
			return encodeJSON(t, object), expectations
		},
	}

	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAttestationFixture(t)
			manifest, expectations := mutate(t, fixture)
			if _, err := contracts.LoadTrustedManifest(bytes.NewReader(manifest), bytes.NewReader(fixture.artifact), expectations); err == nil {
				t.Fatal("modified manifest accepted")
			}
		})
	}
}

func TestLoadTrustedManifestRejectsRawArtifactTampering(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	tampered := bytes.Replace(fixture.artifact, []byte(`"bytecode":"0x6000"`), []byte(`"bytecode":"0x6001"`), 1)
	if bytes.Equal(tampered, fixture.artifact) {
		t.Fatal("test artifact was not modified")
	}
	if _, err := contracts.LoadTrustedManifest(bytes.NewReader(fixture.manifest), bytes.NewReader(tampered), fixture.expectations); err == nil {
		t.Fatal("modified artifact bytes accepted")
	}
}

func TestAttestDeploymentRejectsMutationAfterTrustedLoad(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	mutated := fixture.trusted
	mutated.ContractRole = "other"
	if err := contracts.AttestDeployment(context.Background(), mutated, []contracts.ReadProvider{
		testProvider("archive-a", fixture.reader(fixture.commonHeader)),
		testProvider("archive-b", fixture.reader(fixture.commonHeader)),
	}, time.Second); err == nil {
		t.Fatal("manifest modified after trusted load accepted")
	}
}

func TestLoadTrustedManifestRejectsUnknownTrailingAndDuplicateJSON(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	unknownManifest := decodeObject(t, fixture.manifest)
	unknownManifest["unexpected"] = true
	unknownArtifact := decodeObject(t, fixture.artifact)
	unknownArtifact["unexpected"] = true
	wrongCaseManifest := bytes.Replace(fixture.manifest, []byte(`"schemaVersion"`), []byte(`"SchemaVersion"`), 1)
	wrongCaseArtifact := bytes.Replace(fixture.artifact, []byte(`"sourceTreeSha256"`), []byte(`"SourceTreeSha256"`), 1)

	tests := map[string]struct {
		manifest []byte
		artifact []byte
	}{
		"unknown manifest field":  {manifest: encodeJSON(t, unknownManifest), artifact: fixture.artifact},
		"trailing manifest JSON":  {manifest: append(append([]byte(nil), fixture.manifest...), []byte(` {}`)...), artifact: fixture.artifact},
		"duplicate manifest key":  {manifest: append([]byte(`{"schemaVersion":"1",`), fixture.manifest[1:]...), artifact: fixture.artifact},
		"wrong manifest key case": {manifest: wrongCaseManifest, artifact: fixture.artifact},
		"unknown artifact field":  {manifest: fixture.manifest, artifact: encodeJSON(t, unknownArtifact)},
		"trailing artifact JSON":  {manifest: fixture.manifest, artifact: append(append([]byte(nil), fixture.artifact...), []byte(` []`)...)},
		"duplicate artifact key":  {manifest: fixture.manifest, artifact: append([]byte(`{"artifactVersion":"1",`), fixture.artifact[1:]...)},
		"wrong artifact key case": {manifest: fixture.manifest, artifact: wrongCaseArtifact},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := contracts.LoadTrustedManifest(bytes.NewReader(test.manifest), bytes.NewReader(test.artifact), fixture.expectations); err == nil {
				t.Fatal("invalid JSON accepted")
			}
		})
	}
}

func TestLoadTrustedManifestRejectsUnsafeImmutableReferences(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*testing.T, map[string]any){
		"missing name": func(t *testing.T, artifact map[string]any) {
			delete(mustObject(t, artifact["immutableReferences"]), "sponsor")
		},
		"overlap": func(t *testing.T, artifact map[string]any) {
			references := mustObject(t, artifact["immutableReferences"])
			mustObject(t, references["self"].([]any)[0])["start"] = float64(1)
		},
		"wrong length": func(t *testing.T, artifact map[string]any) {
			references := mustObject(t, artifact["immutableReferences"])
			mustObject(t, references["destination"].([]any)[0])["length"] = float64(31)
		},
		"nonzero placeholder": func(t *testing.T, artifact map[string]any) {
			bytecode := artifact["deployedBytecode"].(string)
			decoded, err := hex.DecodeString(bytecode[2:])
			if err != nil {
				t.Fatal(err)
			}
			decoded[1] = 1
			artifact["deployedBytecode"] = "0x" + hex.EncodeToString(decoded)
		},
	}

	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAttestationFixture(t)
			artifactObject := decodeObject(t, fixture.artifact)
			mutate(t, artifactObject)
			artifact := encodeJSON(t, artifactObject)
			artifactDigest := sha256String(artifact)
			manifestObject := decodeObject(t, fixture.manifest)
			mustObject(t, manifestObject["artifact"])["sha256"] = artifactDigest
			expectations := fixture.expectations
			expectations.ArtifactSHA256 = artifactDigest
			if _, err := contracts.LoadTrustedManifest(bytes.NewReader(encodeJSON(t, manifestObject)), bytes.NewReader(artifact), expectations); err == nil {
				t.Fatal("unsafe immutable references accepted")
			}
		})
	}
}

func TestLoadTrustedManifestEnforcesSizeLimits(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	oversizedManifest := strings.Repeat(" ", int(contracts.MaxDeploymentManifestBytes)+1)
	if _, err := contracts.LoadTrustedManifest(strings.NewReader(oversizedManifest), bytes.NewReader(fixture.artifact), fixture.expectations); err == nil {
		t.Fatal("oversized manifest accepted")
	}
	oversizedArtifact := strings.Repeat(" ", int(contracts.MaxCanonicalArtifactBytes)+1)
	if _, err := contracts.LoadTrustedManifest(bytes.NewReader(fixture.manifest), strings.NewReader(oversizedArtifact), fixture.expectations); err == nil {
		t.Fatal("oversized artifact accepted")
	}
}

func TestAttestDeploymentRejectsDeploymentAboveCommonFinalizedBlock(t *testing.T) {
	t.Parallel()

	fixture := newAttestationFixture(t)
	staleFinalized := testHeader(9, 0x09)
	if err := contracts.AttestDeployment(context.Background(), fixture.trusted, []contracts.ReadProvider{
		testProvider("archive-a", fixture.reader(staleFinalized)),
		testProvider("archive-b", fixture.reader(staleFinalized)),
	}, time.Second); err == nil {
		t.Fatal("unfinalized deployment accepted")
	}
}

func newAttestationFixture(t *testing.T) *attestationFixture {
	t.Helper()

	destination := deterministicTestAddress(0xd1)
	sponsorKey := deterministicSigningKey(t, "deployment-sponsor")
	sponsor := crypto.PubkeyToAddress(sponsorKey.PublicKey)
	deploymentNonce := uint64(7)
	creationCode := []byte{0x60, 0x00}
	deploymentData := testDeploymentData(creationCode, destination, sponsor)
	transaction := signedDeploymentTransaction(t, sponsorKey, big.NewInt(31337), deploymentNonce, deploymentData)
	address := crypto.CreateAddress(sponsor, deploymentNonce)
	template := make([]byte, 101)
	template[0] = 0x60
	template[33] = 0x5b
	template[66] = 0x60
	template[67] = 0x01
	references := map[string][]artifactReference{
		"destination": {{Start: 1, Length: 32}},
		"self":        {{Start: 34, Length: 32}},
		"sponsor":     {{Start: 69, Length: 32}},
	}
	runtime := append([]byte(nil), template...)
	writeImmutable(runtime, 1, destination)
	writeImmutable(runtime, 34, address)
	writeImmutable(runtime, 69, sponsor)

	settings := json.RawMessage(`{"evmVersion":"prague","optimizer":{"enabled":true,"runs":200}}`)
	sourceDigest := "sha256:" + strings.Repeat("a", 64)
	artifactValue := artifactJSON{
		ArtifactVersion:     "1",
		ContractName:        "RescuerV2",
		SourceName:          "RescuerV2.sol",
		CompilerVersion:     testCompilerVersion,
		Settings:            settings,
		SourceTreeSHA256:    sourceDigest,
		CompilerInputSHA256: "sha256:" + strings.Repeat("c", 64),
		ABI:                 json.RawMessage(`[]`),
		Bytecode:            "0x" + hex.EncodeToString(creationCode),
		DeployedBytecode:    "0x" + hex.EncodeToString(template),
		ImmutableReferences: references,
	}
	artifact := encodeJSON(t, artifactValue)
	artifactDigest := sha256String(artifact)
	deploymentHeader := testHeader(10, 0x10)
	commonHeader := testHeader(20, 0x20)
	receipt := &types.Receipt{
		Type:            transaction.Type(),
		Status:          types.ReceiptStatusSuccessful,
		TxHash:          transaction.Hash(),
		ContractAddress: address,
		BlockHash:       deploymentHeader.Hash(),
		BlockNumber:     new(big.Int).Set(deploymentHeader.Number),
	}
	manifestValue := contracts.DeploymentManifest{
		SchemaVersion:             "1",
		ContractRole:              "rescuer",
		ChainID:                   "31337",
		Address:                   address,
		DeploymentTransactionHash: transaction.Hash(),
		DeploymentBlockNumber:     "10",
		DeploymentBlockHash:       deploymentHeader.Hash(),
		Compiler: contracts.CompilerDescription{
			Version:  testCompilerVersion,
			Settings: settings,
		},
		Source:   contracts.SourceProvenance{Kind: "source-tree-sha256", Value: sourceDigest},
		Artifact: contracts.ArtifactDescription{SHA256: artifactDigest},
		Runtime: contracts.RuntimeDescription{
			Keccak256:  crypto.Keccak256Hash(runtime).Hex(),
			ByteLength: uint64(len(runtime)),
		},
		ConstructorArguments: contracts.ConstructorArguments{Destination: destination, Sponsor: sponsor},
		Immutables:           contracts.ImmutableValues{Destination: destination, Self: address, Sponsor: sponsor},
	}
	manifest := encodeJSON(t, manifestValue)
	expectations := contracts.ManifestExpectations{
		ChainID:          "31337",
		ContractRole:     "rescuer",
		Destination:      destination,
		Sponsor:          sponsor,
		ArtifactSHA256:   artifactDigest,
		SourceProvenance: contracts.SourceProvenance{Kind: "source-tree-sha256", Value: sourceDigest},
		CompilerVersion:  testCompilerVersion,
	}
	trusted, err := contracts.LoadTrustedManifest(bytes.NewReader(manifest), bytes.NewReader(artifact), expectations)
	if err != nil {
		t.Fatal(err)
	}
	return &attestationFixture{
		manifest:         manifest,
		artifact:         artifact,
		expectations:     expectations,
		trusted:          trusted,
		runtime:          runtime,
		address:          address,
		destination:      destination,
		sponsor:          sponsor,
		deploymentHeader: deploymentHeader,
		commonHeader:     commonHeader,
		transaction:      transaction,
		receipt:          receipt,
		deploymentData:   deploymentData,
		sponsorKey:       sponsorKey,
		deploymentNonce:  deploymentNonce,
	}
}

func (fixture *attestationFixture) reader(finalized *types.Header) *fakeAttestationReader {
	return &fakeAttestationReader{
		chainID:   big.NewInt(31337),
		finalized: finalized,
		headers: map[string]*types.Header{
			fixture.deploymentHeader.Number.String(): fixture.deploymentHeader,
			fixture.commonHeader.Number.String():     fixture.commonHeader,
		},
		code:        append([]byte(nil), fixture.runtime...),
		transaction: fixture.transaction,
		receipt:     fixture.receipt,
		getters: map[string][]byte{
			"destination": encodedAddress(fixture.destination),
			"sponsor":     encodedAddress(fixture.sponsor),
			"self":        encodedAddress(fixture.address),
		},
		expectedAddress:   fixture.address,
		expectedStateHash: fixture.commonHeader.Hash(),
	}
}

func (fixture *attestationFixture) trustedWithTransactionHash(t *testing.T, hash common.Hash) contracts.DeploymentManifest {
	t.Helper()
	manifest := decodeObject(t, fixture.manifest)
	manifest["deploymentTransactionHash"] = hash.Hex()
	trusted, err := contracts.LoadTrustedManifest(bytes.NewReader(encodeJSON(t, manifest)), bytes.NewReader(fixture.artifact), fixture.expectations)
	if err != nil {
		t.Fatal(err)
	}
	return trusted
}

func (fixture *attestationFixture) trustedWithDeploymentBlock(t *testing.T, header *types.Header) contracts.DeploymentManifest {
	t.Helper()
	manifest := decodeObject(t, fixture.manifest)
	manifest["deploymentBlockNumber"] = header.Number.String()
	manifest["deploymentBlockHash"] = header.Hash().Hex()
	trusted, err := contracts.LoadTrustedManifest(bytes.NewReader(encodeJSON(t, manifest)), bytes.NewReader(fixture.artifact), fixture.expectations)
	if err != nil {
		t.Fatal(err)
	}
	return trusted
}

func (reader *fakeAttestationReader) ChainID(ctx context.Context) (*big.Int, error) {
	if err := reader.before(ctx, "chainID"); err != nil {
		return nil, err
	}
	return new(big.Int).Set(reader.chainID), nil
}

func (reader *fakeAttestationReader) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	if err := reader.before(ctx, "header"); err != nil {
		return nil, err
	}
	if number != nil && number.Cmp(big.NewInt(int64(gethrpc.FinalizedBlockNumber))) == 0 {
		return reader.finalized, nil
	}
	header := reader.headers[number.String()]
	if header == nil {
		return nil, errors.New("RPC endpoint https://private.invalid unavailable")
	}
	return header, nil
}

func (reader *fakeAttestationReader) TransactionByHash(ctx context.Context, _ common.Hash) (*types.Transaction, bool, error) {
	if err := reader.before(ctx, "transaction"); err != nil {
		return nil, false, err
	}
	return reader.transaction, reader.pending, nil
}

func (reader *fakeAttestationReader) TransactionReceipt(ctx context.Context, _ common.Hash) (*types.Receipt, error) {
	if err := reader.before(ctx, "receipt"); err != nil {
		return nil, err
	}
	return reader.receipt, nil
}

func (reader *fakeAttestationReader) CodeAtHash(ctx context.Context, address common.Address, hash common.Hash) ([]byte, error) {
	if err := reader.before(ctx, "code"); err != nil {
		return nil, err
	}
	if address != reader.expectedAddress || hash != reader.expectedStateHash {
		return nil, errors.New("wrong historical state")
	}
	return append([]byte(nil), reader.code...), nil
}

func (reader *fakeAttestationReader) CallContractAtHash(ctx context.Context, call ethereum.CallMsg, hash common.Hash) ([]byte, error) {
	name := getterName(call.Data)
	if err := reader.before(ctx, name); err != nil {
		return nil, err
	}
	if call.To == nil || *call.To != reader.expectedAddress || hash != reader.expectedStateHash {
		return nil, errors.New("wrong historical call")
	}
	result, ok := reader.getters[name]
	if !ok {
		return nil, errors.New("RPC endpoint https://private.invalid?credential=test unavailable")
	}
	return append([]byte(nil), result...), nil
}

func (reader *fakeAttestationReader) before(ctx context.Context, stage string) error {
	reader.calls++
	if _, ok := ctx.Deadline(); !ok {
		reader.missingDeadline = true
	}
	if reader.blockStage == stage {
		<-ctx.Done()
		return ctx.Err()
	}
	if reader.failStage == stage {
		return errors.New("RPC endpoint https://private.invalid?credential=test unavailable")
	}
	return ctx.Err()
}

func (reader *fakeAttestationReader) getterAddress(name string) common.Address {
	result := reader.getters[name]
	return common.BytesToAddress(result[12:])
}

func getterName(selector []byte) string {
	for _, signature := range []string{"destination", "sponsor", "self"} {
		if bytes.Equal(selector, crypto.Keccak256([]byte(signature + "()"))[:4]) {
			return signature
		}
	}
	return "unknown"
}

func encodedAddress(address common.Address) []byte {
	encoded := make([]byte, 32)
	copy(encoded[12:], address[:])
	return encoded
}

func writeImmutable(runtime []byte, start int, address common.Address) {
	copy(runtime[start+12:start+32], address[:])
}

func deterministicSigningKey(t *testing.T, label string) *ecdsa.PrivateKey {
	t.Helper()
	digest := sha256.Sum256([]byte("serdemon attestation test key: " + label))
	key, err := crypto.ToECDSA(digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func signedDeploymentTransaction(t *testing.T, key *ecdsa.PrivateKey, chainID *big.Int, nonce uint64, data []byte) *types.Transaction {
	t.Helper()
	unsigned := types.NewTx(&types.DynamicFeeTx{
		ChainID:   new(big.Int).Set(chainID),
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(2),
		Gas:       500_000,
		Data:      append([]byte(nil), data...),
	})
	signed, err := types.SignTx(unsigned, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func testDeploymentData(creationCode []byte, destination, sponsor common.Address) []byte {
	data := make([]byte, len(creationCode)+64)
	copy(data, creationCode)
	copy(data[len(creationCode)+12:len(creationCode)+32], destination[:])
	copy(data[len(creationCode)+44:len(creationCode)+64], sponsor[:])
	return data
}

func testProvider(id string, reader contracts.AttestationReader) contracts.ReadProvider {
	return contracts.ReadProvider{
		ID:                  id,
		EndpointFingerprint: "test-endpoint-fingerprint-" + id,
		TrustDomain:         "test-trust-domain-" + id,
		Reader:              reader,
	}
}

func testHeader(number int64, tag byte) *types.Header {
	return &types.Header{
		ParentHash: common.BytesToHash([]byte{tag}),
		Number:     big.NewInt(number),
		GasLimit:   30_000_000,
		Time:       uint64(tag),
		Extra:      []byte{tag},
	}
}

func sha256String(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func encodeJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func decodeObject(t *testing.T, value []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(value, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func mustObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value %T is not a JSON object", value)
	}
	return object
}
