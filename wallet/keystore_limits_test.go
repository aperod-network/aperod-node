package wallet

import (
	"io"
	"strings"
	"testing"
)

func TestDecryptMnemonicRejectsExcessivePBKDF2Iterations(t *testing.T) {
	ks := &KeystoreV1{
		Version: 1,
		Crypto: keystoreCrypto{
			Cipher: "aes-256-gcm",
			KDFParams: pbkdf2Params{
				Iter: maxPBKDF2Iterations + 1,
			},
		},
	}

	_, err := DecryptMnemonic(ks, "password")
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("DecryptMnemonic error = %v, want iteration-limit error", err)
	}
}

func TestUnmarshalKeystoreRejectsOversizedJSON(t *testing.T) {
	data := []byte(strings.Repeat(" ", maxKeystoreJSONBytes+1))

	_, err := UnmarshalKeystore(data)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("UnmarshalKeystore error = %v, want size-limit error", err)
	}
}

type countingReader struct {
	reader io.Reader
	read   int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func TestUnmarshalKeystoreReaderStopsAtSizeLimit(t *testing.T) {
	source := &countingReader{reader: strings.NewReader(strings.Repeat("x", maxKeystoreJSONBytes+100))}
	_, err := UnmarshalKeystoreReader(source)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("UnmarshalKeystoreReader error = %v, want size-limit error", err)
	}
	if source.read != maxKeystoreJSONBytes+1 {
		t.Fatalf("read %d bytes, want exactly %d", source.read, maxKeystoreJSONBytes+1)
	}
}

func TestDecryptMnemonicRejectsMalformedFieldSizesBeforeKDF(t *testing.T) {
	keystore, err := EncryptMnemonic("test mnemonic", "test-password", "address")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*KeystoreV1)
		want   string
	}{
		{name: "salt", mutate: func(ks *KeystoreV1) { ks.Crypto.KDFParams.Salt = "00" }, want: "salt must be 32 bytes"},
		{name: "nonce", mutate: func(ks *KeystoreV1) { ks.Crypto.Nonce = "00" }, want: "nonce must be 12 bytes"},
		{name: "mac", mutate: func(ks *KeystoreV1) { ks.Crypto.MAC = "00" }, want: "MAC must be 32 bytes"},
		{name: "ciphertext", mutate: func(ks *KeystoreV1) { ks.Crypto.CipherText = "" }, want: "ciphertext length"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := *keystore
			copy.Crypto = keystore.Crypto
			copy.Crypto.KDFParams = keystore.Crypto.KDFParams
			test.mutate(&copy)
			_, err := DecryptMnemonic(&copy, "test-password")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecryptMnemonic error = %v, want %q", err, test.want)
			}
		})
	}
}
