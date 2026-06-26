package crypto

import (
        "crypto/sha512"
        "fmt"

        "filippo.io/edwards25519"
)

// StealthAddress is a one-time address computed per transaction output.
// The receiver can find it by scanning with their view key; the sender cannot
// link which output belongs to which address without the view key.
type StealthAddress struct {
        OneTimePub Point32 // P = H_s(rV)*G + S  (receiver derives spend key for this output)
        TxPubKey   Point32 // R = r*G             (included in transaction, per-output)
}

// StealthOutput is like StealthAddress but also exposes the HsScalar so the
// sender can encrypt the output amount (recipients decrypt with their view key).
type StealthOutput struct {
        OneTimePub Point32  // P = Hs·G + S
        TxPubKey   Point32  // R = r·G
        HsScalar   Scalar32 // Hs = hashToScalar(r·V, tag) — used for amount encryption
}

// CreateStealthOutput generates a stealth output for sending to (spendPub, viewPub),
// returning the one-time pub key, tx pub key, and the Hs scalar for amount encryption.
func CreateStealthOutput(spendPub, viewPub Point32) (*StealthOutput, error) {
        rScalar, err := randomScalar()
        if err != nil {
                return nil, fmt.Errorf("random scalar: %w", err)
        }

        R := (&edwards25519.Point{}).ScalarBaseMult(rScalar)

        V, err := PointFromBytes(viewPub[:])
        if err != nil {
                return nil, fmt.Errorf("view public key: %w", err)
        }
        shared := (&edwards25519.Point{}).ScalarMult(rScalar, V)

        hsScalar := hashToScalar(shared.Bytes(), "aperod-stealth")

        S, err := PointFromBytes(spendPub[:])
        if err != nil {
                return nil, fmt.Errorf("spend public key: %w", err)
        }
        HsG := (&edwards25519.Point{}).ScalarBaseMult(hsScalar)
        P := (&edwards25519.Point{}).Add(HsG, S)

        var txPub, oneTimePub Point32
        copy(txPub[:], R.Bytes())
        copy(oneTimePub[:], P.Bytes())

        var hs Scalar32
        copy(hs[:], hsScalar.Bytes())

        return &StealthOutput{
                OneTimePub: oneTimePub,
                TxPubKey:   txPub,
                HsScalar:   hs,
        }, nil
}

// CreateStealthAddress generates a one-time stealth address for sending to (spendPub, viewPub).
//
// Construction (Monero-style):
//
//      r         ← random scalar (ephemeral tx private key)
//      R         = r·G              (tx public key, goes in transaction)
//      shared    = r·V              (ECDH: sender uses receiver's view public key V)
//      Hs        = SHA3(shared)     (shared secret hash)
//      P         = Hs·G + S         (one-time public key, receiver uses: (Hs + s)·G)
func CreateStealthAddress(spendPub, viewPub Point32) (*StealthAddress, error) {
        // Random ephemeral scalar r
        rScalar, err := randomScalar()
        if err != nil {
                return nil, fmt.Errorf("random scalar: %w", err)
        }

        // R = r·G (tx public key)
        R := (&edwards25519.Point{}).ScalarBaseMult(rScalar)

        // shared = r·V
        V, err := PointFromBytes(viewPub[:])
        if err != nil {
                return nil, fmt.Errorf("view public key: %w", err)
        }
        shared := (&edwards25519.Point{}).ScalarMult(rScalar, V)

        // Hs = SHA3-256("aperod-stealth" || shared)
        hsScalar := hashToScalar(shared.Bytes(), "aperod-stealth")

        // P = Hs·G + S
        S, err := PointFromBytes(spendPub[:])
        if err != nil {
                return nil, fmt.Errorf("spend public key: %w", err)
        }
        HsG := (&edwards25519.Point{}).ScalarBaseMult(hsScalar)
        P := (&edwards25519.Point{}).Add(HsG, S)

        var txPub, oneTimePub Point32
        copy(txPub[:], R.Bytes())
        copy(oneTimePub[:], P.Bytes())

        return &StealthAddress{
                OneTimePub: oneTimePub,
                TxPubKey:   txPub,
        }, nil
}

// ScanForOutput checks whether a transaction output (described by txPubKey and oneTimePub)
// belongs to the wallet with (viewPriv, spendPub).
// Returns the one-time private key if it matches, nil otherwise.
func ScanForOutput(viewPriv Scalar32, spendPub Point32, txPubKey, oneTimePub Point32) (*Scalar32, error) {
        v, err := ScalarFromBytes(viewPriv[:])
        if err != nil {
                return nil, fmt.Errorf("view private scalar: %w", err)
        }

        // shared = v·R
        R, err := PointFromBytes(txPubKey[:])
        if err != nil {
                return nil, err
        }
        shared := (&edwards25519.Point{}).ScalarMult(v, R)

        // Hs = SHA3(shared)
        hsScalar := hashToScalar(shared.Bytes(), "aperod-stealth")

        // Expected P = Hs·G + S
        S, err := PointFromBytes(spendPub[:])
        if err != nil {
                return nil, err
        }
        HsG := (&edwards25519.Point{}).ScalarBaseMult(hsScalar)
        expectedP := (&edwards25519.Point{}).Add(HsG, S)

        var expectedPub Point32
        copy(expectedPub[:], expectedP.Bytes())

        if expectedPub != oneTimePub {
                return nil, nil // not our output
        }

        // Compute the one-time private key: x = Hs + s  (where s is spend private key)
        // NOTE: s is NOT available here (view-only scan). The wallet must combine:
        //   x = Hs + s  (done in wallet layer with spend key)
        // Return Hs so the caller can add spend key.
        var hs Scalar32
        copy(hs[:], hsScalar.Bytes())
        return &hs, nil
}

// hashToScalar computes SHA-512(data || tag) and reduces it modulo the group order.
func hashToScalar(data []byte, tag string) *edwards25519.Scalar {
        h := sha512.New()
        h.Write(data)
        h.Write([]byte(tag))
        var wide [64]byte
        copy(wide[:], h.Sum(nil))
        s, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
        return s
}

// randomScalar generates a cryptographically random Ed25519 scalar.
func randomScalar() (*edwards25519.Scalar, error) {
        var buf [64]byte
        if _, err := randRead(buf[:]); err != nil {
                return nil, err
        }
        return edwards25519.NewScalar().SetUniformBytes(buf[:])
}
