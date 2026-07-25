// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

interface IERC20Permit {
    function permit(
        address owner,
        address spender,
        uint256 value,
        uint256 deadline,
        uint8 v,
        bytes32 r,
        bytes32 s
    ) external;
}

interface IERC20 {
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
}

contract PermitSweeper {
    error TransferFailed();
    error NotOwner();

    // Set at deployment. Constructors always run in the deploying
    // contract's own context (no delegation involved here — unlike
    // RescuerV2, this contract is called directly, never delegated-to —
    // so address(this) is always safe to use, this isn't fixing that
    // class of bug). This is a plain access-control owner.
    address public immutable owner;

    modifier onlyOwner() {
        if (msg.sender != owner) revert NotOwner();
        _;
    }

    constructor(address _owner) {
        owner = _owner;
    }

    /**
     * @notice Move tokens from `from` to the fixed owner address, using an
     * off-chain signed EIP-2612 permit.
     *
     * SECURITY NOTE: the permit signature only authorizes (owner=from,
     * spender=address(this), value=amount, deadline) — it does NOT commit
     * to a recipient. If `to` were a caller-supplied parameter (as in an
     * earlier version of this contract), anyone who observed a valid
     * permit signature — e.g. by watching the mempool for a pending
     * permitAndTransfer call — could front-run it with their OWN `to`
     * address and steal the approved funds, since the signature itself
     * says nothing about where the tokens should end up. Hardcoding the
     * destination to the immutable `owner` removes this attack surface
     * entirely: there's no `to` parameter left to redirect.
     */
    function permitAndTransfer(
        address token,
        address from,
        uint256 amount,
        uint256 deadline,
        uint8 v,
        bytes32 r,
        bytes32 s
    ) external {
        IERC20Permit(token).permit(from, address(this), amount, deadline, v, r, s);
        bool success = IERC20(token).transferFrom(from, owner, amount);
        if (!success) revert TransferFailed();
    }

    /**
     * @notice Rescue any tokens that ended up sitting on THIS contract's
     * own balance (e.g. dust from a non-standard token, or an accidental
     * direct transfer). Restricted to owner — without this check, anyone
     * could call this and redirect stray tokens to themselves the moment
     * this contract ever holds a balance.
     */
    function rescueTokens(address token, address to, uint256 amount) external onlyOwner {
        bool success = IERC20(token).transferFrom(address(this), to, amount);
        if (!success) revert TransferFailed();
    }
}
