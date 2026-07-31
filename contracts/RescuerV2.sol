// SPDX-License-Identifier: MIT
pragma solidity 0.8.36;

interface IERC20Balance {
    function balanceOf(address account) external view returns (uint256);
}

/// @title RescuerV2
/// @notice Переводит активы делегировавшего EOA только на неизменяемый безопасный адрес.
/// @dev Код должен исполняться через EIP-7702 delegation designator, указывающий на `self`.
contract RescuerV2 {
    address public immutable destination;
    address public immutable self;
    address public immutable sponsor;

    event Swept(address indexed source, address indexed token, address indexed destination, uint256 amount);

    error DelegationNotActive();
    error ExternalCallFailed(address target);
    error NotSponsor();
    error RolesMustDiffer();
    error TokenBalanceQueryFailed(address token);
    error TokenTransferFailed(address token);
    error TransferFailed();
    error ZeroDestination();
    error ZeroSponsor();
    error ZeroTarget();

    modifier onlySponsor() {
        if (msg.sender != sponsor) revert NotSponsor();
        _;
    }

    constructor(address destination_, address sponsor_) {
        if (destination_ == address(0)) revert ZeroDestination();
        if (sponsor_ == address(0)) revert ZeroSponsor();
        if (destination_ == sponsor_) revert RolesMustDiffer();

        destination = destination_;
        sponsor = sponsor_;
        self = address(this);
    }

    /// @notice Переводит полные балансы перечисленных токенов на `destination`.
    /// @dev Permissionless-вызов безопасен относительно получателя: caller не задаёт адрес назначения.
    function sweepAll(address[] calldata tokens) external {
        _verifyDelegation();
        _sweep(tokens);
    }

    /// @notice Переводит полный ETH-баланс делегировавшего EOA на `destination`.
    function sweepEth() external {
        _verifyDelegation();
        _sweepEth();
    }

    /// @notice Выполняет доверенную sponsor-операцию и переводит полученные токены на `destination`.
    /// @dev Sponsor является границей доверия для target и calldata; посторонний caller всегда отклоняется.
    function executeAndSweep(address target, bytes calldata data, address[] calldata tokens)
        external
        payable
        onlySponsor
    {
        _verifyDelegation();
        if (target == address(0)) revert ZeroTarget();

        (bool success,) = target.call{value: msg.value}(data);
        if (!success) revert ExternalCallFailed(target);

        _sweep(tokens);
    }

    function _verifyDelegation() internal view {
        bytes memory code = address(this).code;
        if (code.length != 23 || code[0] != 0xef || code[1] != 0x01 || code[2] != 0x00) {
            revert DelegationNotActive();
        }

        address delegatedTo;
        assembly ("memory-safe") {
            delegatedTo := shr(96, mload(add(code, 0x23)))
        }
        if (delegatedTo != self) revert DelegationNotActive();
    }

    function _sweep(address[] calldata tokens) internal {
        for (uint256 i; i < tokens.length; ++i) {
            address token = tokens[i];
            uint256 balance = _balanceOf(token);
            if (balance == 0) continue;

            _transfer(token, balance);
            emit Swept(address(this), token, destination, balance);
        }
    }

    function _balanceOf(address token) internal view returns (uint256 tokenBalance) {
        (bool success, bytes memory result) = token.staticcall(abi.encodeCall(IERC20Balance.balanceOf, (address(this))));
        if (!success || result.length != 32) revert TokenBalanceQueryFailed(token);

        assembly ("memory-safe") {
            tokenBalance := mload(add(result, 0x20))
        }
    }

    function _transfer(address token, uint256 amount) internal {
        (bool success, bytes memory result) =
            token.call(abi.encodeWithSelector(bytes4(keccak256("transfer(address,uint256)")), destination, amount));
        if (!success) revert TokenTransferFailed(token);
        if (result.length == 0) return;
        if (result.length != 32) revert TokenTransferFailed(token);

        uint256 returned;
        assembly ("memory-safe") {
            returned := mload(add(result, 0x20))
        }
        if (returned != 1) revert TokenTransferFailed(token);
    }

    function _sweepEth() internal {
        uint256 balance = address(this).balance;
        if (balance > 0) {
            (bool success,) = destination.call{value: balance}("");
            if (!success) revert TransferFailed();
            emit Swept(address(this), address(0), destination, balance);
        }
    }

    receive() external payable {}
}
