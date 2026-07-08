package crypto

import (
        "crypto/rand"
        "crypto/sha512"
        "encoding/binary"
        "fmt"

        "filippo.io/edwards25519"
)

// Scalar32 is a 32-byte Edwards25519 scalar.
type Scalar32 [32]byte

// Point32 is a 32-byte compressed Edwards25519 point.
type Point32 [32]byte

// SpendKey is the private key used to spend funds (analogous to Monero's spend key).
// It MUST stay secret — losing or exposing it means losing control of all funds.
type SpendKey struct {
        Private Scalar32 // 32-byte scalar
        Public  Point32  // 32-byte compressed point
}

// ViewKey is derived from the spend key and is used to scan for incoming transactions.
// It can be shared with a read-only wallet without granting spending power.
type ViewKey struct {
        Private Scalar32 // 32-byte scalar = H(spend_priv)
        Public  Point32  // 32-byte compressed point
}

// WalletKeyPair holds a spend/view key pair (the Monero-style dual-key system).
type WalletKeyPair struct {
        Spend SpendKey
        View  ViewKey
}

// GenerateWalletKeys creates a fresh random wallet key pair.
func GenerateWalletKeys() (*WalletKeyPair, error) {
        // Generate random 32-byte spend private key
        var seed [32]byte
        if _, err := rand.Read(seed[:]); err != nil {
                return nil, fmt.Errorf("generate wallet seed: %w", err)
        }
        return WalletKeysFromSeed(seed[:])
}

// WalletKeysFromSeed deterministically derives a wallet key pair from a 32-byte seed.
// Used by HD wallets: seed comes from BIP32/BIP44 derivation.
func WalletKeysFromSeed(seed []byte) (*WalletKeyPair, error) {
        if len(seed) < 32 {
                return nil, fmt.Errorf("seed must be at least 32 bytes, got %d", len(seed))
        }

        // Derive spend scalar: clamp SHA3-256(seed || "spend")
        spendScalar := deriveScalar(seed, "aperod-spend")
        spendPub, err := scalarMulBase(spendScalar)
        if err != nil {
                return nil, fmt.Errorf("spend public key: %w", err)
        }

        // Derive view scalar: SHA3-256(spend_scalar || "view")
        viewScalar := deriveScalar(spendScalar[:], "aperod-view")
        viewPub, err := scalarMulBase(viewScalar)
        if err != nil {
                return nil, fmt.Errorf("view public key: %w", err)
        }

        return &WalletKeyPair{
                Spend: SpendKey{Private: spendScalar, Public: spendPub},
                View:  ViewKey{Private: viewScalar, Public: viewPub},
        }, nil
}

// deriveScalar computes a valid Ed25519 scalar via SHA-512 → SetUniformBytes.
// This guarantees the result is a canonical scalar in [0, l) (group order),
// suitable for both ScalarBaseMult and ScalarFromBytes.
func deriveScalar(data []byte, tag string) Scalar32 {
        h := sha512.New()
        h.Write(data)
        h.Write([]byte(tag))
        var wide [64]byte
        copy(wide[:], h.Sum(nil))
        sc, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
        var s Scalar32
        copy(s[:], sc.Bytes())
        return s
}

// scalarMulBase computes scalar * G (base point) and returns the compressed point.
func scalarMulBase(s Scalar32) (Point32, error) {
        sc, err := edwards25519.NewScalar().SetCanonicalBytes(s[:])
        if err != nil {
                return Point32{}, fmt.Errorf("invalid scalar: %w", err)
        }
        pt := (&edwards25519.Point{}).ScalarBaseMult(sc)
        var out Point32
        copy(out[:], pt.Bytes())
        return out, nil
}

// PublicKeyFromPrivate derives the Ed25519 public key from a private scalar.
// Used by the API server when it receives a spend/view private key from the wallet.
func PublicKeyFromPrivate(priv Scalar32) (Point32, error) {
        return scalarMulBase(priv)
}

// ScalarMulBase is exported for use in other crypto sub-packages.
func ScalarMulBase(s Scalar32) (Point32, error) {
        return scalarMulBase(s)
}

// AddScalars returns (a + b) mod group order.
// Used to derive one-time spend private keys: one_time_priv = Hs + spend_priv.
func AddScalars(a, b Scalar32) (Scalar32, error) {
        sa, err := ScalarFromBytes(a[:])
        if err != nil {
                return Scalar32{}, fmt.Errorf("AddScalars a: %w", err)
        }
        sb, err := ScalarFromBytes(b[:])
        if err != nil {
                return Scalar32{}, fmt.Errorf("AddScalars b: %w", err)
        }
        sum := edwards25519.NewScalar().Add(sa, sb)
        var out Scalar32
        copy(out[:], sum.Bytes())
        return out, nil
}

// AddPoints returns a + b on the Edwards25519 curve as a compressed point.
// Used to derive per-block coinbase one-time keys: mint_pub = spend_pub + height*G.
func AddPoints(a, b Point32) (Point32, error) {
        pa, err := PointFromBytes(a[:])
        if err != nil {
                return Point32{}, fmt.Errorf("AddPoints a: %w", err)
        }
        pb, err := PointFromBytes(b[:])
        if err != nil {
                return Point32{}, fmt.Errorf("AddPoints b: %w", err)
        }
        sum := (&edwards25519.Point{}).Add(pa, pb)
        var out Point32
        copy(out[:], sum.Bytes())
        return out, nil
}

// ScalarFromUint64 encodes a small non-negative integer as a canonical
// Edwards25519 scalar (little-endian, zero-padded). Safe for any uint64
// value since 2^64 is far smaller than the group order L (~2^252).
// Used to make each block's coinbase reward output cryptographically unique
// (mint_pub = spend_pub + height*G) so identical reward amounts to the same
// validator address no longer collide on tx hash / key image across blocks.
func ScalarFromUint64(n uint64) Scalar32 {
        var s Scalar32
        binary.LittleEndian.PutUint64(s[:8], n)
        return s
}

// PointFromBytes decodes a compressed Ed25519 point. Returns error if invalid.
func PointFromBytes(b []byte) (*edwards25519.Point, error) {
        if len(b) != 32 {
                return nil, fmt.Errorf("point must be 32 bytes, got %d", len(b))
        }
        p := new(edwards25519.Point)
        if _, err := p.SetBytes(b); err != nil {
                return nil, fmt.Errorf("invalid curve point: %w", err)
        }
        return p, nil
}

// ScalarFromBytes decodes a canonical scalar. Returns error if not in range.
func ScalarFromBytes(b []byte) (*edwards25519.Scalar, error) {
        if len(b) != 32 {
                return nil, fmt.Errorf("scalar must be 32 bytes, got %d", len(b))
        }
        s := new(edwards25519.Scalar)
        if _, err := s.SetCanonicalBytes(b); err != nil {
                return nil, fmt.Errorf("invalid scalar: %w", err)
        }
        return s, nil
}
