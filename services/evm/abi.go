// Package evm wraps the bits of go-ethereum we need to interact with
// settlement's Gateway contract + the ERC-20 we transfer through it.
//
// Runtime ABI binding (via bind.NewBoundContract + abi.JSON) is used
// instead of abigen-generated bindings — keeps the toolchain to just
// `go build`, no `go generate` step required.
//
// The two ABI JSON strings below are minimal: only the functions and
// events we actually call/parse. Full source for Gateway lives at
// github.com/settlement/contracts. Full ERC-20 is well-known.
package evm

// GatewayABI is the subset of settlement's Gateway we use:
//
//   - createOrder  : commit USDC to escrow + emit OrderCreated.
//   - getOrderInfo : view fn to cross-check on-chain state.
//   - OrderCreated : event used to extract the bytes32 orderId after submit.
//   - OrderSettled / OrderRefunded : events watched by the BSC indexer
//     (v1.5 — not yet wired; included so the ABI is forward-compatible).
const GatewayABI = `[
  {
    "type":"function","name":"createOrder","stateMutability":"nonpayable",
    "inputs":[
      {"name":"_token","type":"address"},
      {"name":"_amount","type":"uint256"},
      {"name":"_rate","type":"uint96"},
      {"name":"_senderFeeRecipient","type":"address"},
      {"name":"_senderFee","type":"uint256"},
      {"name":"_refundAddress","type":"address"},
      {"name":"messageHash","type":"string"}
    ],
    "outputs":[{"name":"orderId","type":"bytes32"}]
  },
  {
    "type":"function","name":"getOrderInfo","stateMutability":"view",
    "inputs":[{"name":"_orderId","type":"bytes32"}],
    "outputs":[{
      "components":[
        {"name":"sender","type":"address"},
        {"name":"token","type":"address"},
        {"name":"senderFeeRecipient","type":"address"},
        {"name":"senderFee","type":"uint256"},
        {"name":"protocolFee","type":"uint256"},
        {"name":"isFulfilled","type":"bool"},
        {"name":"isRefunded","type":"bool"},
        {"name":"refundAddress","type":"address"},
        {"name":"currentBPS","type":"uint96"},
        {"name":"amount","type":"uint256"}
      ],
      "name":"","type":"tuple"
    }]
  },
  {
    "type":"event","name":"OrderCreated","anonymous":false,
    "inputs":[
      {"indexed":true, "name":"sender","type":"address"},
      {"indexed":true, "name":"token","type":"address"},
      {"indexed":true, "name":"amount","type":"uint256"},
      {"indexed":false,"name":"protocolFee","type":"uint256"},
      {"indexed":false,"name":"orderId","type":"bytes32"},
      {"indexed":false,"name":"rate","type":"uint256"},
      {"indexed":false,"name":"messageHash","type":"string"}
    ]
  },
  {
    "type":"event","name":"OrderSettled","anonymous":false,
    "inputs":[
      {"indexed":false,"name":"splitOrderId","type":"bytes32"},
      {"indexed":true, "name":"orderId","type":"bytes32"},
      {"indexed":true, "name":"liquidityProvider","type":"address"},
      {"indexed":false,"name":"settlePercent","type":"uint96"}
    ]
  },
  {
    "type":"event","name":"OrderRefunded","anonymous":false,
    "inputs":[
      {"indexed":false,"name":"fee","type":"uint256"},
      {"indexed":true, "name":"orderId","type":"bytes32"}
    ]
  }
]`

// ERC20ABI covers the standard methods we use (approve, allowance,
// balanceOf, decimals) plus the Approval/Transfer events for any future
// receipt parsing.
const ERC20ABI = `[
  {
    "type":"function","name":"approve","stateMutability":"nonpayable",
    "inputs":[{"name":"spender","type":"address"},{"name":"value","type":"uint256"}],
    "outputs":[{"name":"","type":"bool"}]
  },
  {
    "type":"function","name":"allowance","stateMutability":"view",
    "inputs":[{"name":"owner","type":"address"},{"name":"spender","type":"address"}],
    "outputs":[{"name":"","type":"uint256"}]
  },
  {
    "type":"function","name":"balanceOf","stateMutability":"view",
    "inputs":[{"name":"owner","type":"address"}],
    "outputs":[{"name":"","type":"uint256"}]
  },
  {
    "type":"function","name":"decimals","stateMutability":"view",
    "inputs":[],
    "outputs":[{"name":"","type":"uint8"}]
  },
  {
    "type":"event","name":"Approval","anonymous":false,
    "inputs":[
      {"indexed":true,"name":"owner","type":"address"},
      {"indexed":true,"name":"spender","type":"address"},
      {"indexed":false,"name":"value","type":"uint256"}
    ]
  },
  {
    "type":"event","name":"Transfer","anonymous":false,
    "inputs":[
      {"indexed":true,"name":"from","type":"address"},
      {"indexed":true,"name":"to","type":"address"},
      {"indexed":false,"name":"value","type":"uint256"}
    ]
  }
]`
