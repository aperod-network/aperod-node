package core

import (
	"encoding/binary"
	"fmt"

	"github.com/aperod/aperod/crypto"
)

// RewardAuthorization is the consensus-verifiable authorization attached to a
// validator reward transaction.  It is stored in the coinbase transaction's
// Extra field, so it is part of the block Merkle root and therefore part of the
// proposer's signed block hash.
//
// The validator signature does not authorize an arbitrary amount: consensus
// also checks that Amount equals the immutable protocol reward for the block
// plus its priority tips. The signature authorizes the exact recipient and
// exact claimed amount without making other validators trust that node's local
// wallet configuration.
type RewardAuthorization struct {
	Version           uint8
	Height            uint64
	ParentHash        crypto.Hash32
	RecipientNetwork  crypto.NetworkByte
	RecipientSpendPub crypto.Point32
	RecipientViewPub  crypto.Point32
	Amount            uint64
	RewardID          crypto.Hash32
	Signature         []byte
}

const (
	// RewardAuthorizationVersion is the initial wire format for authorized
	// validator rewards.
	RewardAuthorizationVersion uint8 = 1
	// RewardAuthorizationSize is the exact encoded size in Transaction.Extra.
	RewardAuthorizationSize = 1 + 8 + 32 + 1 + 32 + 32 + 8 + 32 + 64
)

// RewardAuthorizationID derives the replay-protection identifier.  Binding the
// parent hash and height prevents the same authorization from being reused at
// another height or on a different chain branch.
func RewardAuthorizationID(
	version uint8,
	height uint64,
	parentHash crypto.Hash32,
	recipientNetwork crypto.NetworkByte,
	recipientSpendPub crypto.Point32,
	recipientViewPub crypto.Point32,
	amount uint64,
) crypto.Hash32 {
	return crypto.HashBytes(
		[]byte("aperod/reward-id/v1"),
		[]byte{version},
		encodeRewardUint64(height),
		parentHash[:],
		[]byte{byte(recipientNetwork)},
		recipientSpendPub[:],
		recipientViewPub[:],
		encodeRewardUint64(amount),
	)
}

// RewardAuthorizationSignMsg returns the canonical message signed by the
// block's ValidatorPub.  RewardID is included so the signature covers the
// replay-protection value as well as all authorization fields.
func (a *RewardAuthorization) RewardAuthorizationSignMsg() crypto.Hash32 {
	return crypto.HashBytes(
		[]byte("aperod/reward-authorization/v1"),
		[]byte{a.Version},
		encodeRewardUint64(a.Height),
		a.ParentHash[:],
		[]byte{byte(a.RecipientNetwork)},
		a.RecipientSpendPub[:],
		a.RecipientViewPub[:],
		encodeRewardUint64(a.Amount),
		a.RewardID[:],
	)
}

// Sign fills RewardID and signs the authorization with the scheduled
// validator's private key.
func (a *RewardAuthorization) Sign(priv crypto.ValidatorPrivKey) error {
	if a == nil {
		return fmt.Errorf("nil reward authorization")
	}
	a.RewardID = RewardAuthorizationID(
		a.Version,
		a.Height,
		a.ParentHash,
		a.RecipientNetwork,
		a.RecipientSpendPub,
		a.RecipientViewPub,
		a.Amount,
	)
	sig, err := priv.Sign(a.RewardAuthorizationSignMsg())
	if err != nil {
		return fmt.Errorf("sign reward authorization: %w", err)
	}
	a.Signature = sig
	return nil
}

// Validate checks the authorization against the containing block and proposer.
func (a *RewardAuthorization) Validate(
	blockHeight uint64,
	parentHash crypto.Hash32,
	validatorPub crypto.ValidatorPubKey,
) error {
	if a == nil {
		return fmt.Errorf("nil reward authorization")
	}
	if a.Version != RewardAuthorizationVersion {
		return fmt.Errorf("unsupported reward authorization version %d", a.Version)
	}
	if a.Height != blockHeight {
		return fmt.Errorf("reward height %d does not match block height %d", a.Height, blockHeight)
	}
	if a.ParentHash != parentHash {
		return fmt.Errorf("reward parent hash does not match block parent")
	}
	if a.Amount == 0 {
		return fmt.Errorf("reward amount must be > 0")
	}
	switch a.RecipientNetwork {
	case crypto.MainnetByte, crypto.TestnetByte, crypto.DevnetByte:
	default:
		return fmt.Errorf("unsupported reward recipient network byte 0x%02x", byte(a.RecipientNetwork))
	}
	expectedID := RewardAuthorizationID(
		a.Version,
		a.Height,
		a.ParentHash,
		a.RecipientNetwork,
		a.RecipientSpendPub,
		a.RecipientViewPub,
		a.Amount,
	)
	if a.RewardID != expectedID {
		return fmt.Errorf("reward authorization id mismatch")
	}
	if !validatorPub.Verify(a.RewardAuthorizationSignMsg(), a.Signature) {
		return fmt.Errorf("invalid reward authorization signature")
	}
	return nil
}

// EncodeRewardAuthorization serializes an authorization into the fixed-size
// coinbase Extra payload.
func EncodeRewardAuthorization(a *RewardAuthorization) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("nil reward authorization")
	}
	if len(a.Signature) != 64 {
		return nil, fmt.Errorf("reward authorization signature must be 64 bytes, got %d", len(a.Signature))
	}
	b := make([]byte, RewardAuthorizationSize)
	off := 0
	b[off] = a.Version
	off++
	binary.BigEndian.PutUint64(b[off:off+8], a.Height)
	off += 8
	copy(b[off:off+32], a.ParentHash[:])
	off += 32
	b[off] = byte(a.RecipientNetwork)
	off++
	copy(b[off:off+32], a.RecipientSpendPub[:])
	off += 32
	copy(b[off:off+32], a.RecipientViewPub[:])
	off += 32
	binary.BigEndian.PutUint64(b[off:off+8], a.Amount)
	off += 8
	copy(b[off:off+32], a.RewardID[:])
	off += 32
	copy(b[off:], a.Signature)
	return b, nil
}

// DecodeRewardAuthorization parses the fixed-size coinbase Extra payload.
func DecodeRewardAuthorization(b []byte) (*RewardAuthorization, error) {
	if len(b) != RewardAuthorizationSize {
		return nil, fmt.Errorf("reward authorization must be %d bytes, got %d",
			RewardAuthorizationSize, len(b))
	}
	a := &RewardAuthorization{}
	off := 0
	a.Version = b[off]
	off++
	a.Height = binary.BigEndian.Uint64(b[off : off+8])
	off += 8
	copy(a.ParentHash[:], b[off:off+32])
	off += 32
	a.RecipientNetwork = crypto.NetworkByte(b[off])
	off++
	copy(a.RecipientSpendPub[:], b[off:off+32])
	off += 32
	copy(a.RecipientViewPub[:], b[off:off+32])
	off += 32
	a.Amount = binary.BigEndian.Uint64(b[off : off+8])
	off += 8
	copy(a.RewardID[:], b[off:off+32])
	off += 32
	a.Signature = append([]byte(nil), b[off:]...)
	return a, nil
}

// BuildAuthorizedRewardTx creates a reward coinbase and attaches a signed
// authorization bound to its exact inclusion height and parent.
func BuildAuthorizedRewardTx(
	addr crypto.Address,
	amount uint64,
	height uint64,
	parentHash crypto.Hash32,
	validatorPriv crypto.ValidatorPrivKey,
) (*Transaction, error) {
	if amount == 0 {
		return nil, fmt.Errorf("authorized reward amount must be > 0")
	}
	network, spendPub, viewPub, err := crypto.DecodeAddress(addr)
	if err != nil {
		return nil, fmt.Errorf("decode reward address: %w", err)
	}
	tx, err := BuildMintTx(addr, amount, height)
	if err != nil {
		return nil, err
	}
	auth := &RewardAuthorization{
		Version:           RewardAuthorizationVersion,
		Height:            height,
		ParentHash:        parentHash,
		RecipientNetwork:  network,
		RecipientSpendPub: spendPub,
		RecipientViewPub:  viewPub,
		Amount:            amount,
	}
	if err := auth.Sign(validatorPriv); err != nil {
		return nil, err
	}
	tx.Extra, err = EncodeRewardAuthorization(auth)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// ValidateAuthorizedRewardTx verifies the authorization and the transparent
// deterministic mint output it authorizes.  This is called by consensus
// because TxVerifier intentionally skips all zero-input coinbase transactions.
func ValidateAuthorizedRewardTx(
	tx *Transaction,
	blockHeight uint64,
	parentHash crypto.Hash32,
	validatorPub crypto.ValidatorPubKey,
) (*RewardAuthorization, error) {
	if tx == nil {
		return nil, fmt.Errorf("nil reward transaction")
	}
	if !tx.IsCoinbase() {
		return nil, fmt.Errorf("authorized reward must be a coinbase transaction")
	}
	if tx.Version != TxVersionBase {
		return nil, fmt.Errorf("authorized reward must use transaction version %d", TxVersionBase)
	}
	if tx.Fee != 0 {
		return nil, fmt.Errorf("authorized reward fee must be zero")
	}
	if len(tx.Outputs) != 1 {
		return nil, fmt.Errorf("authorized reward must have exactly one output")
	}
	auth, err := DecodeRewardAuthorization(tx.Extra)
	if err != nil {
		return nil, err
	}
	if err := auth.Validate(blockHeight, parentHash, validatorPub); err != nil {
		return nil, err
	}
	if tx.Outputs[0].TxPubKey != (crypto.Point32{}) ||
		tx.Outputs[0].EncAmount != [8]byte{} {
		return nil, fmt.Errorf("authorized reward output must be transparent")
	}
	blind, err := crypto.DeterministicMintBlindV2(
		auth.RecipientSpendPub,
		auth.Amount,
		auth.Height,
	)
	if err != nil {
		return nil, fmt.Errorf("derive reward blind: %w", err)
	}
	commit, err := crypto.Commit(auth.Amount, blind)
	if err != nil {
		return nil, fmt.Errorf("derive reward commitment: %w", err)
	}
	if tx.Outputs[0].AmountCommit != commit {
		return nil, fmt.Errorf("reward commitment does not match authorized amount")
	}
	heightPub, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(auth.Height))
	if err != nil {
		return nil, fmt.Errorf("derive reward height key: %w", err)
	}
	oneTimePub, err := crypto.AddPoints(auth.RecipientSpendPub, heightPub)
	if err != nil {
		return nil, fmt.Errorf("derive reward one-time key: %w", err)
	}
	if tx.Outputs[0].OneTimePub != oneTimePub {
		return nil, fmt.Errorf("reward output key does not match authorized recipient and height")
	}
	return auth, nil
}

func encodeRewardUint64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}
