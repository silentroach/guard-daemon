// SPDX-License-Identifier: MIT
pragma solidity ^0.8.23;

/**
 * @title RescuerV2
 * @notice Token rescuer with EIP-7702 delegation verification.
 * 
 * Key security feature: Before sweeping tokens, verifies that the source
 * address is delegated to this contract via EIP-7702.
 * 
 * If scammer overwrites the delegation, sweep() will fail with clear error.
 * This prevents silent failures where scammer's contract intercepts the call.
 */

interface IERC20 {
    function balanceOf(address account) external view returns (uint256);
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
}

contract RescuerV2 {
    address public immutable destination;

    // Captured at deployment time, when address(this) correctly refers to
    // THIS contract's own address (constructors always run in the
    // deploying contract's own context, never via delegation). This is
    // the only reliable way to know "my own address" later, because during
    // delegated execution address(this) shifts to mean the DELEGATOR
    // (the compromised EOA), not this contract's own deployed address.
    address public immutable self;

    // The only address allowed to call executeAndSweep(). Without this,
    // ANYONE could send a transaction to the delegated EOA invoking
    // executeAndSweep() with an arbitrary claimContract/claimData — e.g.
    // an approve(attacker, type(uint256).max) call on any token the EOA
    // holds. The immutable `destination` only protects sweepAll/sweepEth's
    // own transfer logic; it does nothing to stop an attacker-controlled
    // low-level call from doing damage unrelated to sweeping. sweepAll()
    // and sweepEth() are left open to anyone — they can only ever move
    // funds to the fixed `destination`, so an outside caller triggering
    // them early is harmless (at most "helps" sweep for free).
    address public immutable sponsor;
    
    event Swept(address indexed token, uint256 amount);
    event EthSwept(uint256 amount);

    error ZeroDestination();
    error ZeroSponsor();
    error DelegationNotActive();
    error TransferFailed();
    error NotSponsor();

    modifier onlySponsor() {
        if (msg.sender != sponsor) revert NotSponsor();
        _;
    }

    constructor(address _destination, address _sponsor) {
        if (_destination == address(0)) revert ZeroDestination();
        if (_sponsor == address(0)) revert ZeroSponsor();
        destination = _destination;
        sponsor = _sponsor;
        self = address(this);
    }

    /**
     * @notice Sweep multiple tokens to destination.
     * Verifies delegation is active before proceeding.
     * 
     * Reverts if source address is not delegated to this contract via EIP-7702.
     * This prevents scammer from intercepting the call by overwriting delegation.
     */
    function sweepAll(address[] calldata tokens) external {
        _verifyDelegation();
        _sweep(tokens);
    }

    /**
     * @notice Sweep ETH to destination.
     * Verifies delegation is active before proceeding.
     */
    function sweepEth() external {
        _verifyDelegation();
        _sweepEth();
    }

    /**
     * @notice Execute an arbitrary call on a known, reviewed target contract,
     * forwarding any ETH sent with THIS transaction (msg.value), then sweep
     * the resulting tokens. Needed for claim/interaction functions that
     * require payment (payable functions) — e.g. a small activation fee.
     * The sponsor includes the required ETH as the outer transaction's
     * value field; it's credited to the delegated EOA's balance as part
     * of the same atomic call, then forwarded on to claimContract here.
     *
     * RESTRICTED to sponsor only: unlike sweepAll/sweepEth (which can only
     * ever move funds to the fixed `destination`), this function lets the
     * caller specify an arbitrary target contract and calldata — if left
     * open, anyone could hijack the delegated EOA into calling approve(),
     * or any other state-changing function, on any token it holds.
     */
    function executeAndSweep(
        address claimContract,
        bytes calldata claimData,
        address[] calldata tokens
    ) external payable onlySponsor {
        _verifyDelegation();
        
        (bool success, bytes memory reason) = claimContract.call{value: msg.value}(claimData);
        require(success, string(reason));
        
        _sweep(tokens);
    }

    /**
     * @notice Verify that the account currently executing this code (i.e.
     * address(this), which — because we're running via EIP-7702 delegation —
     * equals the compromised EOA, e.g. anaxine.eth, NOT this contract's own
     * deployed address) still carries a valid delegation designator
     * pointing at THIS contract specifically.
     * 
     * EIP-7702 delegation format in bytecode:
     *   0xef0100 (3 bytes) + address (20 bytes)
     * 
     * Two distinct addresses matter here and must not be confused:
     *   - address(this) at call time = the DELEGATOR (anaxine.eth). This is
     *     whose code we read via EXTCODE* to find the designator — correct,
     *     because that's the account whose delegation status we care about.
     *   - `self` (immutable, captured in the constructor) = this contract's
     *     OWN deployed address. This is what the designator's embedded
     *     address must match — NOT address(this), which by call time no
     *     longer refers to this contract at all.
     * 
     * Getting this backwards (comparing against address(this) instead of
     * `self`) makes the check permanently unsatisfiable: the designator
     * always points at this contract's real address, which can never equal
     * the delegator's own address, so it would revert unconditionally
     * regardless of whether delegation is actually correct.
     */
    function _verifyDelegation() internal view {
        address source = address(this);
        
        // Get the bytecode of the source address (the delegator's designator)
        bytes memory code = source.code;
        
        // EIP-7702 delegation must be at least 23 bytes
        // (0xef + 01 + 00 + 20-byte address)
        if (code.length < 23) {
            revert DelegationNotActive();
        }
        
        // Check magic bytes: 0xef0100
        if (code[0] != 0xef || code[1] != 0x01 || code[2] != 0x00) {
            revert DelegationNotActive();
        }
        
        // Extract delegated-to address from bytes 3-22 (20 bytes)
        address delegatedTo;
        assembly {
            // Skip first 32 bytes (length) + 3 bytes of magic
            delegatedTo := shr(96, mload(add(code, 0x23)))
        }
        
        // Verify it's delegated to US — compare against the immutable
        // `self`, captured at deploy time, NOT against address(this).
        if (delegatedTo != self) {
            revert DelegationNotActive();
        }
    }

    /**
     * @notice Internal: sweep tokens to destination
     */
    function _sweep(address[] calldata tokens) internal {
        for (uint256 i = 0; i < tokens.length; i++) {
            address token = tokens[i];
            uint256 balance = IERC20(token).balanceOf(address(this));
            
            if (balance > 0) {
                bool success = IERC20(token).transfer(destination, balance);
                if (!success) revert TransferFailed();
                emit Swept(token, balance);
            }
        }
    }

    /**
     * @notice Internal: sweep ETH to destination
     */
    function _sweepEth() internal {
        uint256 balance = address(this).balance;
        if (balance > 0) {
            (bool success, ) = destination.call{value: balance}("");
            if (!success) revert TransferFailed();
            emit EthSwept(balance);
        }
    }

    /**
     * @notice Receive ETH
     */
    receive() external payable {}
}
