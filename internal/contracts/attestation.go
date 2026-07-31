package contracts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
)

const (
	MaxDeploymentManifestBytes int64 = 1 << 20
	MaxCanonicalArtifactBytes  int64 = 16 << 20
)

const (
	manifestSchemaVersion = "1"
	artifactSchemaVersion = "1"
	rescuerContractRole   = "rescuer"
	rescuerContractName   = "RescuerV2"
	rescuerSourceName     = "RescuerV2.sol"
)

type AttestationReader interface {
	ChainID(context.Context) (*big.Int, error)
	HeaderByNumber(context.Context, *big.Int) (*types.Header, error)
	TransactionByHash(context.Context, common.Hash) (*types.Transaction, bool, error)
	TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error)
	CodeAtHash(context.Context, common.Address, common.Hash) ([]byte, error)
	CallContractAtHash(context.Context, ethereum.CallMsg, common.Hash) ([]byte, error)
}

var _ AttestationReader = (*ethclient.Client)(nil)

type ReadProvider struct {
	ID                  string
	EndpointFingerprint string
	TrustDomain         string
	Reader              AttestationReader
}

type SourceProvenance struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type CompilerDescription struct {
	Version  string          `json:"version"`
	Settings json.RawMessage `json:"settings"`
}

type ArtifactDescription struct {
	SHA256 string `json:"sha256"`
}

type RuntimeDescription struct {
	Keccak256  string `json:"keccak256"`
	ByteLength uint64 `json:"byteLength"`
}

type ConstructorArguments struct {
	Destination common.Address `json:"destination"`
	Sponsor     common.Address `json:"sponsor"`
}

type ImmutableValues struct {
	Destination common.Address `json:"destination"`
	Self        common.Address `json:"self"`
	Sponsor     common.Address `json:"sponsor"`
}

type DeploymentManifest struct {
	SchemaVersion             string               `json:"schemaVersion"`
	ContractRole              string               `json:"contractRole"`
	ChainID                   string               `json:"chainId"`
	Address                   common.Address       `json:"address"`
	DeploymentTransactionHash common.Hash          `json:"deploymentTransactionHash"`
	DeploymentBlockNumber     string               `json:"deploymentBlockNumber"`
	DeploymentBlockHash       common.Hash          `json:"deploymentBlockHash"`
	Compiler                  CompilerDescription  `json:"compiler"`
	Source                    SourceProvenance     `json:"source"`
	Artifact                  ArtifactDescription  `json:"artifact"`
	Runtime                   RuntimeDescription   `json:"runtime"`
	ConstructorArguments      ConstructorArguments `json:"constructorArguments"`
	Immutables                ImmutableValues      `json:"immutables"`

	linkedRuntime  []byte
	deploymentData []byte
	trustedSeal    [sha256.Size]byte
}

type ManifestExpectations struct {
	ChainID          string
	ContractRole     string
	Destination      common.Address
	Sponsor          common.Address
	ArtifactSHA256   string
	SourceProvenance SourceProvenance
	CompilerVersion  string
}

type canonicalArtifact struct {
	ArtifactVersion     string                          `json:"artifactVersion"`
	ContractName        string                          `json:"contractName"`
	SourceName          string                          `json:"sourceName"`
	CompilerVersion     string                          `json:"compilerVersion"`
	Settings            json.RawMessage                 `json:"settings"`
	SourceTreeSHA256    string                          `json:"sourceTreeSha256"`
	CompilerInputSHA256 string                          `json:"compilerInputSha256"`
	ABI                 json.RawMessage                 `json:"abi"`
	Bytecode            string                          `json:"bytecode"`
	DeployedBytecode    string                          `json:"deployedBytecode"`
	ImmutableReferences map[string][]immutableReference `json:"immutableReferences"`
}

type immutableReference struct {
	Start  uint64 `json:"start"`
	Length uint64 `json:"length"`
}

type immutableRange struct {
	start uint64
	end   uint64
	value common.Address
}

type transactionLookup struct {
	transaction *types.Transaction
	pending     bool
}

type attestationReaderIdentity struct {
	typeOf  reflect.Type
	pointer uintptr
}

func LoadTrustedManifest(manifestReader, artifactReader io.Reader, expectations ManifestExpectations) (DeploymentManifest, error) {
	var empty DeploymentManifest
	if manifestReader == nil || artifactReader == nil {
		return empty, operatorError("не передан manifest или artifact")
	}
	if err := validateExpectations(expectations); err != nil {
		return empty, err
	}

	manifestBytes, err := readLimited(manifestReader, MaxDeploymentManifestBytes)
	if err != nil {
		return empty, operatorError("не удалось безопасно прочитать deployment manifest")
	}
	artifactBytes, err := readLimited(artifactReader, MaxCanonicalArtifactBytes)
	if err != nil {
		return empty, operatorError("не удалось безопасно прочитать canonical artifact")
	}

	var manifest DeploymentManifest
	if err := decodeStrictJSON(manifestBytes, &manifest); err != nil || !validManifestJSONShape(manifestBytes) {
		return empty, operatorError("deployment manifest содержит недопустимый JSON")
	}
	var artifact canonicalArtifact
	if err := decodeStrictJSON(artifactBytes, &artifact); err != nil || !validArtifactJSONShape(artifactBytes) {
		return empty, operatorError("canonical artifact содержит недопустимый JSON")
	}

	artifactDigest := sha256.Sum256(artifactBytes)
	artifactSHA256 := "sha256:" + hex.EncodeToString(artifactDigest[:])
	if manifest.Artifact.SHA256 != artifactSHA256 || expectations.ArtifactSHA256 != artifactSHA256 {
		return empty, operatorError("SHA-256 artifact не совпадает с доверенным значением")
	}
	if err := validateManifestMetadata(manifest, artifact, expectations); err != nil {
		return empty, err
	}

	template, err := decodeCanonicalBytecode(artifact.DeployedBytecode)
	if err != nil || len(template) == 0 {
		return empty, operatorError("deployed bytecode в artifact имеет недопустимый формат")
	}
	creationCode, err := decodeCanonicalBytecode(artifact.Bytecode)
	if err != nil || len(creationCode) == 0 {
		return empty, operatorError("creation bytecode в artifact имеет недопустимый формат")
	}

	linkedRuntime, err := linkImmutableReferences(template, artifact.ImmutableReferences, manifest.Immutables)
	if err != nil {
		return empty, err
	}
	if uint64(len(linkedRuntime)) != manifest.Runtime.ByteLength || manifest.Runtime.ByteLength == 0 {
		return empty, operatorError("длина linked runtime не совпадает с manifest")
	}
	runtimeHash := crypto.Keccak256Hash(linkedRuntime).Hex()
	if manifest.Runtime.Keccak256 != runtimeHash {
		return empty, operatorError("Keccak-256 linked runtime не совпадает с manifest")
	}

	manifest.linkedRuntime = append([]byte(nil), linkedRuntime...)
	manifest.deploymentData = buildDeploymentData(creationCode, expectations.Destination, expectations.Sponsor)
	seal, err := deploymentManifestSeal(manifest)
	if err != nil {
		return empty, operatorError("не удалось зафиксировать доверенный manifest")
	}
	manifest.trustedSeal = seal
	return manifest, nil
}

func AttestDeployment(ctx context.Context, manifest DeploymentManifest, providers []ReadProvider, readTimeout time.Duration) error {
	if ctx == nil {
		return attestationError("не передан context")
	}
	if readTimeout <= 0 {
		return attestationError("таймаут чтения должен быть положительным")
	}
	if err := validateProviders(providers); err != nil {
		return err
	}
	if err := validateLoadedManifest(manifest); err != nil {
		return err
	}

	expectedChainID, _ := parseDecimal(manifest.ChainID, false)
	for _, provider := range providers {
		chainID, ok := timedRead(ctx, readTimeout, provider.Reader.ChainID)
		if !ok || chainID == nil || chainID.Cmp(expectedChainID) != 0 {
			return attestationError("поставщик чтения не подтвердил ожидаемый chain ID")
		}
	}

	finalizedHeaders := make([]*types.Header, len(providers))
	finalizedNumber := big.NewInt(int64(gethrpc.FinalizedBlockNumber))
	for index, provider := range providers {
		header, ok := timedRead(ctx, readTimeout, func(callContext context.Context) (*types.Header, error) {
			return provider.Reader.HeaderByNumber(callContext, new(big.Int).Set(finalizedNumber))
		})
		if !ok || !validHeader(header) {
			return attestationError("не удалось получить корректный finalized header")
		}
		finalizedHeaders[index] = header
	}

	commonNumber := new(big.Int).Set(finalizedHeaders[0].Number)
	sameHeight := true
	for _, header := range finalizedHeaders[1:] {
		if header.Number.Cmp(commonNumber) != 0 {
			sameHeight = false
			if header.Number.Cmp(commonNumber) < 0 {
				commonNumber.Set(header.Number)
			}
		}
	}

	commonHeaders := finalizedHeaders
	if !sameHeight {
		commonHeaders = make([]*types.Header, len(providers))
		for index, provider := range providers {
			header, ok := timedRead(ctx, readTimeout, func(callContext context.Context) (*types.Header, error) {
				return provider.Reader.HeaderByNumber(callContext, new(big.Int).Set(commonNumber))
			})
			if !ok || !validHeader(header) || header.Number.Cmp(commonNumber) != 0 {
				return attestationError("поставщик чтения не подтвердил общий finalized block")
			}
			if finalizedHeaders[index].Number.Cmp(commonNumber) == 0 && finalizedHeaders[index].Hash() != header.Hash() {
				return attestationError("поставщик чтения изменил finalized block hash")
			}
			commonHeaders[index] = header
		}
	}

	commonHash := commonHeaders[0].Hash()
	for _, header := range commonHeaders {
		if header.Number.Cmp(commonNumber) != 0 || header.Hash() != commonHash {
			return attestationError("поставщики чтения расходятся по finalized block hash")
		}
	}

	deploymentNumber, _ := parseDecimal(manifest.DeploymentBlockNumber, true)
	if deploymentNumber.Cmp(commonNumber) > 0 {
		return attestationError("deployment block ещё не finalized")
	}
	if deploymentNumber.Cmp(commonNumber) == 0 && manifest.DeploymentBlockHash != commonHash {
		return attestationError("deployment block hash не совпадает с общим finalized block")
	}
	for _, provider := range providers {
		header, ok := timedRead(ctx, readTimeout, func(callContext context.Context) (*types.Header, error) {
			return provider.Reader.HeaderByNumber(callContext, new(big.Int).Set(deploymentNumber))
		})
		if !ok || !validHeader(header) || header.Number.Cmp(deploymentNumber) != 0 || header.Hash() != manifest.DeploymentBlockHash {
			return attestationError("поставщик чтения не подтвердил deployment block hash")
		}
	}
	for _, provider := range providers {
		lookup, ok := timedRead(ctx, readTimeout, func(callContext context.Context) (transactionLookup, error) {
			transaction, pending, err := provider.Reader.TransactionByHash(callContext, manifest.DeploymentTransactionHash)
			return transactionLookup{transaction: transaction, pending: pending}, err
		})
		if !ok || lookup.pending || lookup.transaction == nil {
			return attestationError("поставщик чтения не подтвердил mined deployment transaction")
		}
		transaction := lookup.transaction
		if transaction.Hash() != manifest.DeploymentTransactionHash || transaction.ChainId().Cmp(expectedChainID) != 0 || transaction.To() != nil {
			return attestationError("deployment transaction не совпадает с trusted manifest")
		}
		if !bytes.Equal(transaction.Data(), manifest.deploymentData) {
			return attestationError("deployment transaction содержит неожиданный creation input")
		}
		sender, err := types.Sender(types.LatestSignerForChainID(expectedChainID), transaction)
		if err != nil || sender != manifest.Immutables.Sponsor {
			return attestationError("deployment transaction не подписана ожидаемым sponsor")
		}
		if crypto.CreateAddress(sender, transaction.Nonce()) != manifest.Address {
			return attestationError("CREATE address не совпадает с deployment manifest")
		}

		receipt, receiptOK := timedRead(ctx, readTimeout, func(callContext context.Context) (*types.Receipt, error) {
			return provider.Reader.TransactionReceipt(callContext, manifest.DeploymentTransactionHash)
		})
		if !receiptOK || receipt == nil || receipt.Status != types.ReceiptStatusSuccessful {
			return attestationError("поставщик чтения не подтвердил успешный deployment receipt")
		}
		if receipt.TxHash != manifest.DeploymentTransactionHash || receipt.ContractAddress != manifest.Address || receipt.BlockNumber == nil || receipt.BlockNumber.Cmp(deploymentNumber) != 0 || receipt.BlockHash != manifest.DeploymentBlockHash {
			return attestationError("deployment receipt не совпадает с trusted manifest")
		}
	}

	getters := []struct {
		name     string
		selector []byte
		expected common.Address
	}{
		{name: "destination", selector: methodSelector("destination()"), expected: manifest.Immutables.Destination},
		{name: "sponsor", selector: methodSelector("sponsor()"), expected: manifest.Immutables.Sponsor},
		{name: "self", selector: methodSelector("self()"), expected: manifest.Immutables.Self},
	}

	for _, provider := range providers {
		code, ok := timedRead(ctx, readTimeout, func(callContext context.Context) ([]byte, error) {
			return provider.Reader.CodeAtHash(callContext, manifest.Address, commonHash)
		})
		if !ok || !bytes.Equal(code, manifest.linkedRuntime) || crypto.Keccak256Hash(code).Hex() != manifest.Runtime.Keccak256 {
			return attestationError("runtime code не совпадает с доверенным artifact")
		}

		for _, getter := range getters {
			call := ethereum.CallMsg{To: &manifest.Address, Data: append([]byte(nil), getter.selector...)}
			result, callOK := timedRead(ctx, readTimeout, func(callContext context.Context) ([]byte, error) {
				return provider.Reader.CallContractAtHash(callContext, call, commonHash)
			})
			address, decodeOK := decodeCanonicalAddress(result)
			if !callOK || !decodeOK || address != getter.expected {
				return attestationError("getter " + getter.name + " не подтвердил ожидаемый immutable address")
			}
		}
	}

	return nil
}

func validateExpectations(expectations ManifestExpectations) error {
	if _, ok := parseDecimal(expectations.ChainID, false); !ok {
		return operatorError("ожидаемый chain ID имеет недопустимый формат")
	}
	if expectations.ContractRole != rescuerContractRole {
		return operatorError("ожидаемая роль контракта не поддерживается")
	}
	if expectations.Destination == (common.Address{}) || expectations.Sponsor == (common.Address{}) {
		return operatorError("ожидаемые destination и sponsor должны быть ненулевыми")
	}
	if expectations.Destination == expectations.Sponsor {
		return operatorError("ожидаемые destination и sponsor должны различаться")
	}
	if !validSHA256(expectations.ArtifactSHA256) {
		return operatorError("ожидаемый SHA-256 artifact имеет недопустимый формат")
	}
	if !validSourceProvenance(expectations.SourceProvenance) {
		return operatorError("ожидаемое происхождение source имеет недопустимый формат")
	}
	if expectations.CompilerVersion == "" {
		return operatorError("не зафиксирована ожидаемая версия compiler")
	}
	return nil
}

func validateManifestMetadata(manifest DeploymentManifest, artifact canonicalArtifact, expectations ManifestExpectations) error {
	if manifest.SchemaVersion != manifestSchemaVersion {
		return operatorError("версия схемы deployment manifest не поддерживается")
	}
	if manifest.ContractRole != rescuerContractRole || manifest.ContractRole != expectations.ContractRole {
		return operatorError("роль контракта не совпадает с доверенным значением")
	}
	if _, ok := parseDecimal(manifest.ChainID, false); !ok || manifest.ChainID != expectations.ChainID {
		return operatorError("chain ID в manifest не совпадает с доверенным значением")
	}
	if _, ok := parseDecimal(manifest.DeploymentBlockNumber, true); !ok {
		return operatorError("номер deployment block имеет недопустимый формат")
	}
	if manifest.Address == (common.Address{}) || manifest.DeploymentTransactionHash == (common.Hash{}) || manifest.DeploymentBlockHash == (common.Hash{}) {
		return operatorError("deployment manifest содержит нулевой адрес или hash")
	}
	if manifest.Compiler.Version != expectations.CompilerVersion || artifact.CompilerVersion != expectations.CompilerVersion {
		return operatorError("версия compiler не совпадает с доверенным значением")
	}
	if !validJSONObject(manifest.Compiler.Settings) || !validJSONObject(artifact.Settings) || !equalJSON(manifest.Compiler.Settings, artifact.Settings) {
		return operatorError("настройки compiler в manifest и artifact не совпадают")
	}
	if manifest.Source != expectations.SourceProvenance || !validSourceProvenance(manifest.Source) {
		return operatorError("происхождение source не совпадает с доверенным значением")
	}
	if artifact.ArtifactVersion != artifactSchemaVersion || artifact.ContractName != rescuerContractName || artifact.SourceName != rescuerSourceName {
		return operatorError("metadata canonical artifact не соответствует RescuerV2")
	}
	if !validSHA256(artifact.SourceTreeSHA256) || !validSHA256(artifact.CompilerInputSHA256) {
		return operatorError("digest source или compiler input в artifact имеет недопустимый формат")
	}
	if manifest.Source.Kind == "source-tree-sha256" && manifest.Source.Value != artifact.SourceTreeSHA256 {
		return operatorError("source tree в artifact не совпадает с manifest")
	}
	if !validJSONArray(artifact.ABI) {
		return operatorError("ABI в canonical artifact имеет недопустимый формат")
	}
	if manifest.ConstructorArguments.Destination != manifest.Immutables.Destination || manifest.ConstructorArguments.Sponsor != manifest.Immutables.Sponsor {
		return operatorError("constructor arguments не совпадают с immutable values")
	}
	if manifest.Immutables.Destination != expectations.Destination || manifest.Immutables.Sponsor != expectations.Sponsor {
		return operatorError("immutable values не совпадают с доверенными адресами")
	}
	if manifest.Immutables.Self != manifest.Address {
		return operatorError("immutable self не совпадает с deployment address")
	}
	if manifest.Immutables.Destination == (common.Address{}) || manifest.Immutables.Sponsor == (common.Address{}) || manifest.Immutables.Self == (common.Address{}) {
		return operatorError("immutable addresses должны быть ненулевыми")
	}
	if manifest.Immutables.Destination == manifest.Immutables.Sponsor {
		return operatorError("immutable destination и sponsor должны различаться")
	}
	if !validKeccak256(manifest.Runtime.Keccak256) || manifest.Runtime.ByteLength == 0 {
		return operatorError("runtime identity имеет недопустимый формат")
	}
	return nil
}

func linkImmutableReferences(template []byte, references map[string][]immutableReference, values ImmutableValues) ([]byte, error) {
	expected := map[string]common.Address{
		"destination": values.Destination,
		"self":        values.Self,
		"sponsor":     values.Sponsor,
	}
	if len(references) != len(expected) {
		return nil, operatorError("immutable references должны содержать только destination, self и sponsor")
	}

	ranges := make([]immutableRange, 0)
	for name, value := range expected {
		entries, ok := references[name]
		if !ok || len(entries) == 0 {
			return nil, operatorError("artifact не содержит обязательную immutable reference")
		}
		for _, entry := range entries {
			if entry.Length != 32 || entry.Start > uint64(len(template)) || entry.Length > uint64(len(template))-entry.Start {
				return nil, operatorError("immutable reference выходит за границы runtime template")
			}
			end := entry.Start + entry.Length
			if !allZero(template[entry.Start:end]) {
				return nil, operatorError("immutable placeholder в runtime template должен быть нулевым")
			}
			ranges = append(ranges, immutableRange{start: entry.Start, end: end, value: value})
		}
	}
	for name := range references {
		if _, ok := expected[name]; !ok {
			return nil, operatorError("artifact содержит неизвестную immutable reference")
		}
	}

	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	for index := 1; index < len(ranges); index++ {
		if ranges[index].start < ranges[index-1].end {
			return nil, operatorError("immutable references пересекаются")
		}
	}

	linked := append([]byte(nil), template...)
	for _, immutable := range ranges {
		copy(linked[immutable.start+12:immutable.end], immutable.value[:])
	}
	return linked, nil
}

func validateProviders(providers []ReadProvider) error {
	if len(providers) < 2 {
		return attestationError("требуются минимум два независимых поставщика чтения")
	}
	ids := make(map[string]struct{}, len(providers))
	endpointFingerprints := make(map[string]struct{}, len(providers))
	trustDomains := make(map[string]struct{}, len(providers))
	readerIdentities := make(map[attestationReaderIdentity]struct{}, len(providers))
	for _, provider := range providers {
		id := strings.TrimSpace(provider.ID)
		endpointFingerprint := strings.TrimSpace(provider.EndpointFingerprint)
		trustDomain := strings.TrimSpace(provider.TrustDomain)
		if id == "" {
			return attestationError("идентификатор поставщика чтения не должен быть пустым")
		}
		if endpointFingerprint == "" || trustDomain == "" {
			return attestationError("не заданы обязательные атрибуты независимости поставщика чтения")
		}
		if _, exists := ids[id]; exists {
			return attestationError("идентификаторы поставщиков чтения должны быть уникальными")
		}
		ids[id] = struct{}{}
		if _, exists := endpointFingerprints[endpointFingerprint]; exists {
			return attestationError("поставщики чтения не имеют уникальных endpoint identity")
		}
		endpointFingerprints[endpointFingerprint] = struct{}{}
		if _, exists := trustDomains[trustDomain]; exists {
			return attestationError("поставщики чтения не принадлежат независимым trust domain")
		}
		trustDomains[trustDomain] = struct{}{}

		identity, ok := readerIdentity(provider.Reader)
		if !ok {
			return attestationError("identity поставщика чтения нельзя надёжно сравнить")
		}
		if _, exists := readerIdentities[identity]; exists {
			return attestationError("повторно использован один экземпляр поставщика чтения")
		}
		readerIdentities[identity] = struct{}{}
	}
	return nil
}

func validateLoadedManifest(manifest DeploymentManifest) error {
	seal, err := deploymentManifestSeal(manifest)
	if err != nil || seal != manifest.trustedSeal || manifest.trustedSeal == ([sha256.Size]byte{}) {
		return attestationError("manifest не был доверенно загружен или изменён после проверки")
	}
	if len(manifest.linkedRuntime) == 0 || uint64(len(manifest.linkedRuntime)) != manifest.Runtime.ByteLength {
		return attestationError("trusted manifest не содержит linked runtime")
	}
	if len(manifest.deploymentData) <= 64 {
		return attestationError("trusted manifest не содержит deployment input")
	}
	if crypto.Keccak256Hash(manifest.linkedRuntime).Hex() != manifest.Runtime.Keccak256 {
		return attestationError("linked runtime в trusted manifest повреждён")
	}
	if _, ok := parseDecimal(manifest.ChainID, false); !ok {
		return attestationError("chain ID в trusted manifest повреждён")
	}
	if _, ok := parseDecimal(manifest.DeploymentBlockNumber, true); !ok {
		return attestationError("номер deployment block в trusted manifest повреждён")
	}
	return nil
}

func timedRead[T any](parent context.Context, timeout time.Duration, read func(context.Context) (T, error)) (T, bool) {
	callContext, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	value, err := read(callContext)
	if err != nil || callContext.Err() != nil {
		var zero T
		return zero, false
	}
	return value, true
}

func validHeader(header *types.Header) bool {
	return header != nil && header.Number != nil && header.Number.Sign() >= 0
}

func decodeCanonicalAddress(data []byte) (common.Address, bool) {
	if len(data) != 32 || !allZero(data[:12]) {
		return common.Address{}, false
	}
	return common.BytesToAddress(data[12:]), true
}

func buildDeploymentData(creationCode []byte, destination, sponsor common.Address) []byte {
	deploymentData := make([]byte, len(creationCode)+64)
	copy(deploymentData, creationCode)
	copy(deploymentData[len(creationCode)+12:len(creationCode)+32], destination[:])
	copy(deploymentData[len(creationCode)+44:len(creationCode)+64], sponsor[:])
	return deploymentData
}

func methodSelector(signature string) []byte {
	return crypto.Keccak256([]byte(signature))[:4]
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(data)) > limit || len(data) == 0 {
		return nil, errors.New("invalid bounded input")
	}
	return data, nil
}

func decodeStrictJSON(data []byte, destination any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func validManifestJSONShape(data []byte) bool {
	root, ok := exactJSONObject(data,
		"schemaVersion",
		"contractRole",
		"chainId",
		"address",
		"deploymentTransactionHash",
		"deploymentBlockNumber",
		"deploymentBlockHash",
		"compiler",
		"source",
		"artifact",
		"runtime",
		"constructorArguments",
		"immutables",
	)
	if !ok {
		return false
	}
	_, compilerOK := exactJSONObject(root["compiler"], "version", "settings")
	_, sourceOK := exactJSONObject(root["source"], "kind", "value")
	_, artifactOK := exactJSONObject(root["artifact"], "sha256")
	_, runtimeOK := exactJSONObject(root["runtime"], "keccak256", "byteLength")
	_, constructorOK := exactJSONObject(root["constructorArguments"], "destination", "sponsor")
	_, immutablesOK := exactJSONObject(root["immutables"], "destination", "self", "sponsor")
	return compilerOK && sourceOK && artifactOK && runtimeOK && constructorOK && immutablesOK
}

func validArtifactJSONShape(data []byte) bool {
	root, ok := exactJSONObject(data,
		"artifactVersion",
		"contractName",
		"sourceName",
		"compilerVersion",
		"settings",
		"sourceTreeSha256",
		"compilerInputSha256",
		"abi",
		"bytecode",
		"deployedBytecode",
		"immutableReferences",
	)
	if !ok {
		return false
	}
	references, ok := exactJSONObject(root["immutableReferences"], "destination", "self", "sponsor")
	if !ok {
		return false
	}
	for _, name := range []string{"destination", "self", "sponsor"} {
		var entries []json.RawMessage
		if err := json.Unmarshal(references[name], &entries); err != nil || entries == nil {
			return false
		}
		for _, entry := range entries {
			if _, entryOK := exactJSONObject(entry, "start", "length"); !entryOK {
				return false
			}
		}
	}
	return true
}

func exactJSONObject(data []byte, names ...string) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil || len(object) != len(names) {
		return nil, false
	}
	for _, name := range names {
		if _, exists := object[name]; !exists {
			return nil, false
		}
	}
	return object, true
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}

	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, duplicate := keys[key]; duplicate {
				return errors.New("duplicate JSON object key")
			}
			keys[key] = struct{}{}
			if valueErr := consumeJSONValue(decoder); valueErr != nil {
				return valueErr
			}
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if valueErr := consumeJSONValue(decoder); valueErr != nil {
				return valueErr
			}
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func decodeCanonicalBytecode(value string) ([]byte, error) {
	if len(value) < 2 || !strings.HasPrefix(value, "0x") || (len(value)-2)%2 != 0 || !lowerHex(value[2:]) {
		return nil, errors.New("invalid bytecode")
	}
	return hex.DecodeString(value[2:])
}

func validSHA256(value string) bool {
	return len(value) == len("sha256:")+64 && strings.HasPrefix(value, "sha256:") && lowerHex(value[len("sha256:"):])
}

func validKeccak256(value string) bool {
	return len(value) == 66 && strings.HasPrefix(value, "0x") && lowerHex(value[2:])
}

func validSourceProvenance(source SourceProvenance) bool {
	switch source.Kind {
	case "source-tree-sha256":
		return validSHA256(source.Value)
	case "git-commit":
		return len(source.Value) == 40 && lowerHex(source.Value)
	default:
		return false
	}
}

func lowerHex(value string) bool {
	if value == "" {
		return true
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func parseDecimal(value string, allowZero bool) (*big.Int, bool) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return nil, false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return nil, false
		}
	}
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok || parsed.Sign() < 0 || (!allowZero && parsed.Sign() == 0) {
		return nil, false
	}
	return parsed, true
}

func validJSONObject(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(trimmed, &object) == nil && object != nil
}

func validJSONArray(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) < 2 || trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
		return false
	}
	var array []json.RawMessage
	return json.Unmarshal(trimmed, &array) == nil && array != nil
}

func equalJSON(left, right json.RawMessage) bool {
	var leftValue any
	var rightValue any
	leftDecoder := json.NewDecoder(bytes.NewReader(left))
	rightDecoder := json.NewDecoder(bytes.NewReader(right))
	leftDecoder.UseNumber()
	rightDecoder.UseNumber()
	if leftDecoder.Decode(&leftValue) != nil || rightDecoder.Decode(&rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func deploymentManifestSeal(manifest DeploymentManifest) ([sha256.Size]byte, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	runtimeDigest := sha256.Sum256(manifest.linkedRuntime)
	deploymentDigest := sha256.Sum256(manifest.deploymentData)
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("deployment-manifest-seal-v1\x00"))
	_, _ = hasher.Write(encoded)
	_, _ = hasher.Write(runtimeDigest[:])
	_, _ = hasher.Write(deploymentDigest[:])
	var seal [sha256.Size]byte
	copy(seal[:], hasher.Sum(nil))
	return seal, nil
}

func readerIdentity(reader AttestationReader) (attestationReaderIdentity, bool) {
	if reader == nil {
		return attestationReaderIdentity{}, false
	}
	reflected := reflect.ValueOf(reader)
	if reflected.Kind() != reflect.Pointer || reflected.IsNil() {
		return attestationReaderIdentity{}, false
	}
	return attestationReaderIdentity{typeOf: reflected.Type(), pointer: reflected.Pointer()}, true
}

func operatorError(reason string) error {
	return fmt.Errorf("доверенная загрузка deployment manifest отклонена: %s", reason)
}

func attestationError(reason string) error {
	return fmt.Errorf("аттестация deployment заблокирована: %s", reason)
}
