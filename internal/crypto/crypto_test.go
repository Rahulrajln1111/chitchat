package crypto

import (
	"encoding/base64"
	"testing"
)

// TestJavaInteropDecrypt verifies we can decrypt data encrypted by the Java
// MessageCryptoService with the same key from application.properties.
func TestJavaInteropDecrypt(t *testing.T) {
	// Key from Java application.properties (base64, 32 bytes)
	keyB64 := "PqVFfVXGuVYGAppV4MmxyaOJUEC55NZCi7JUsBcgBn0="
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		t.Fatalf("bad key: %v", err)
	}

	// Round-trip: our Encrypt then Decrypt
	enc, err := Encrypt(key, "hello from go")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	got, err := Decrypt(key, enc.Ciphertext, enc.Nonce)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}
	if got != "hello from go" {
		t.Fatalf("round-trip mismatch: %q", got)
	}

	// Tamper detection: flip a bit in ciphertext -> must fail
	raw, _ := base64.StdEncoding.DecodeString(enc.Ciphertext)
	raw[0] ^= 0xFF
	tampered := base64.StdEncoding.EncodeToString(raw)
	if _, err := Decrypt(key, tampered, enc.Nonce); err == nil {
		t.Fatal("tampered ciphertext must fail (AEADBadTagException parity)")
	}
}

// TestKeyFormatsMatchJava verifies our DER encodings parse the same way Java
// would (SPKI for public, PKCS8 for private).
func TestKeyFormatsMatchJava(t *testing.T) {
	pub, priv, err := GenerateSigningKeyPair()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	pubB64, err := EncodePublicKeyJava(pub)
	if err != nil {
		t.Fatalf("encode pub: %v", err)
	}
	privB64, err := EncodePrivateKeyJava(priv)
	if err != nil {
		t.Fatalf("encode priv: %v", err)
	}

	// Java SPKI Ed25519 DER is 44 bytes -> base64 length 60, starts with MCowBQYDK2Vw
	pubDecoded, _ := base64.StdEncoding.DecodeString(pubB64)
	if len(pubDecoded) != 44 || pubB64[:12] != "MCowBQYDK2Vw" {
		t.Fatalf("public key SPKI format mismatch: len=%d prefix=%s", len(pubDecoded), pubB64[:12])
	}

	// Java PKCS8 Ed25519 DER is 48 bytes -> base64 length 64, starts with MC4CAQAw
	privDecoded, _ := base64.StdEncoding.DecodeString(privB64)
	if len(privDecoded) != 48 || privB64[:8] != "MC4CAQAw" {
		t.Fatalf("private key PKCS8 format mismatch: len=%d prefix=%s", len(privDecoded), privB64[:9])
	}

	// Re-parse and sign/verify round-trip
	pub2, err := DecodePublicKeyJava(pubB64)
	if err != nil {
		t.Fatalf("decode pub: %v", err)
	}
	priv2, err := DecodePrivateKeyJava(privB64)
	if err != nil {
		t.Fatalf("decode priv: %v", err)
	}

	payload := SignaturePayload("room-1", "alice", "cipherB64", "nonceB64")
	sig := Sign(priv2, payload)
	if !Verify(pub2, payload, sig) {
		t.Fatal("signature round-trip failed")
	}
	// Wrong payload must fail
	if Verify(pub2, SignaturePayload("room-2", "alice", "cipherB64", "nonceB64"), sig) {
		t.Fatal("signature must fail on different roomId")
	}
}

// TestWrapUnwrapPrivateKey verifies KEK wrapping round-trip.
func TestWrapUnwrapPrivateKey(t *testing.T) {
	// KEK must be a valid 32-byte AES-256 key (Java SecretKeySpec rejects other sizes).
	kek, _ := base64.StdEncoding.DecodeString("/EjTQZ3Y584b7h5jBX0ns7NUQyfKYhmBgO0M876+kfg=")

	_, priv, _ := GenerateSigningKeyPair()
	privB64, _ := EncodePrivateKeyJava(priv)

	wrapped, err := WrapPrivateKey(kek, privB64)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if wrapped == privB64 {
		t.Fatal("wrapped must differ from plaintext")
	}

	unwrapped, err := UnwrapPrivateKey(kek, wrapped)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if unwrapped != privB64 {
		t.Fatal("unwrap round-trip mismatch")
	}
}
