// SPDX-License-Identifier: MIT
pragma solidity 0.8.36;

import {RescuerV2} from "../../../contracts/RescuerV2.sol";

contract StandardToken {
    mapping(address => mapping(address => uint256)) public allowance;
    mapping(address => uint256) public balanceOf;

    function mint(address account, uint256 amount) external {
        balanceOf[account] += amount;
    }

    function approve(address spender, uint256 amount) external returns (bool) {
        allowance[msg.sender][spender] = amount;
        return true;
    }

    function transfer(address recipient, uint256 amount) external returns (bool) {
        balanceOf[msg.sender] -= amount;
        balanceOf[recipient] += amount;
        return true;
    }
}

contract NoReturnToken {
    mapping(address => uint256) public balanceOf;

    function mint(address account, uint256 amount) external {
        balanceOf[account] += amount;
    }

    function transfer(address recipient, uint256 amount) external {
        balanceOf[msg.sender] -= amount;
        balanceOf[recipient] += amount;
    }
}

contract FalseReturnToken {
    mapping(address => uint256) public balanceOf;

    function mint(address account, uint256 amount) external {
        balanceOf[account] += amount;
    }

    function transfer(address, uint256) external pure returns (bool) {
        return false;
    }
}

contract RevertingToken {
    mapping(address => uint256) public balanceOf;

    function mint(address account, uint256 amount) external {
        balanceOf[account] += amount;
    }

    function transfer(address, uint256) external pure returns (bool) {
        revert("TOKEN_REVERT");
    }
}

contract MalformedBalanceToken {
    fallback() external {
        assembly ("memory-safe") {
            mstore(0, 1)
            return(31, 1)
        }
    }
}

contract ConfigurableReturnToken {
    mapping(address => uint256) public balanceOf;

    bytes private response;
    bool private reverts;

    function mint(address account, uint256 amount) external {
        balanceOf[account] += amount;
    }

    function setResponse(bytes calldata response_, bool reverts_) external {
        response = response_;
        reverts = reverts_;
    }

    fallback() external {
        if (msg.sig != bytes4(keccak256("transfer(address,uint256)"))) revert();

        (address recipient, uint256 amount) = abi.decode(msg.data[4:], (address, uint256));
        bytes memory result = response;
        if (reverts) {
            assembly ("memory-safe") {
                revert(add(result, 0x20), mload(result))
            }
        }

        balanceOf[msg.sender] -= amount;
        balanceOf[recipient] += amount;
        assembly ("memory-safe") {
            return(add(result, 0x20), mload(result))
        }
    }
}

contract ClaimTarget {
    function claim(address token, uint256 amount) external payable {
        StandardToken(token).mint(msg.sender, amount);
    }
}

contract CallbackClaimTarget {
    bool public callbackSucceeded;

    function claim(address source, address token, uint256 amount) external {
        address[] memory tokens = new address[](0);
        (callbackSucceeded,) =
            source.call(abi.encodeCall(RescuerV2.executeAndSweep, (address(this), bytes(""), tokens)));
        StandardToken(token).mint(msg.sender, amount);
    }
}

contract ArbitraryCallTarget {
    bytes32 public dataHash;
    uint256 public received;

    fallback(bytes calldata data) external payable returns (bytes memory) {
        dataHash = keccak256(data);
        received += msg.value;
        return "";
    }

    receive() external payable {
        dataHash = keccak256("");
        received += msg.value;
    }
}

contract RevertingCallTarget {
    bytes private reason;

    function setReason(bytes calldata reason_) external {
        reason = reason_;
    }

    fallback() external payable {
        _revert();
    }

    receive() external payable {
        _revert();
    }

    function _revert() private view {
        bytes memory result = reason;
        assembly ("memory-safe") {
            revert(add(result, 0x20), mload(result))
        }
    }
}

contract MockNFT {
    mapping(address => mapping(address => bool)) public isApprovedForAll;
    mapping(uint256 => address) public ownerOf;

    function mint(address owner, uint256 tokenId) external {
        ownerOf[tokenId] = owner;
    }

    function setApprovalForAll(address operator, bool approved) external {
        isApprovedForAll[msg.sender][operator] = approved;
    }

    function transferFrom(address from, address to, uint256 tokenId) external {
        if (msg.sender != from && !isApprovedForAll[from][msg.sender]) revert();
        if (ownerOf[tokenId] != from) revert();
        ownerOf[tokenId] = to;
    }
}

contract ReentrantToken {
    mapping(address => uint256) public balanceOf;

    address private immutable nestedToken;
    address private immutable source;
    bool private entered;
    bool public executeCallbackSucceeded;
    bool public sweepCallbackSucceeded;

    constructor(address source_, address nestedToken_) {
        source = source_;
        nestedToken = nestedToken_;
    }

    function mint(address account, uint256 amount) external {
        balanceOf[account] += amount;
    }

    function transfer(address recipient, uint256 amount) external returns (bool) {
        balanceOf[msg.sender] -= amount;
        balanceOf[recipient] += amount;
        if (entered) return true;

        entered = true;
        address[] memory nestedTokens = new address[](1);
        nestedTokens[0] = nestedToken;
        (sweepCallbackSucceeded,) = source.call(abi.encodeCall(RescuerV2.sweepAll, (nestedTokens)));

        address[] memory noTokens = new address[](0);
        (executeCallbackSucceeded,) =
            source.call(abi.encodeCall(RescuerV2.executeAndSweep, (address(this), bytes(""), noTokens)));
        return true;
    }
}

contract DelegatingImplementation {
    address private immutable rescuer;

    constructor(address rescuer_) {
        rescuer = rescuer_;
    }

    fallback() external payable {
        (bool success, bytes memory result) = rescuer.delegatecall(msg.data);
        assembly ("memory-safe") {
            switch success
            case 0 { revert(add(result, 0x20), mload(result)) }
            default { return(add(result, 0x20), mload(result)) }
        }
    }
}

contract RejectingDestination {
    receive() external payable {
        revert();
    }
}
