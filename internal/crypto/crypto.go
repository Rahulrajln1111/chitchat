package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// NonceBytes is the 96-bit GCM nonce size, standard for AES-GCM (matches Java).
const NonceBytes = 12

// EncryptedData mirrors Java MessageCryptoService.EncryptedData record.
type EncryptedData struct {
	Ciphertext string // base64 (std) of ciphertext||tag
	Nonce      string // base64 (std) of 12-byte nonce
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Encrypt encrypts plaintext with AES-256-GCM using the given key.
// Output is compatible with Java AES/GCM/NoPadding (tag appended).
func Encrypt(key []byte, plaintext string) (EncryptedData, error) {
	a, err := gcm(key)
	if err != nil {
		return EncryptedData{}, err
	}
	nonce := make([]byte, NonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return EncryptedData{}, err
	}
	ct := a.Seal(nil, nonce, []byte(plaintext), nil)
	return EncryptedData{
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
	}, nil
}

// Decrypt decrypts AES-GCM ciphertext; returns error on tamper (like AEADBadTagException).
func Decrypt(key []byte, ciphertextB64, nonceB64 string) (string, error) {
	a, err := gcm(key)
	if err != nil {
		return "", err
	}
	ct, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return "", err
	}
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return "", err
	}
	pt, err := a.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// WrapPrivateKey encrypts a base64 private key with the KEK.
// Stored format: "ciphertext.nonce" (matches Java).
func WrapPrivateKey(kek []byte, privateKeyB64 string) (string, error) {
	enc, err := Encrypt(kek, privateKeyB64)
	if err != nil {
		return "", err
	}
	return enc.Ciphertext + "." + enc.Nonce, nil
}

// UnwrapPrivateKey decrypts a "ciphertext.nonce" wrapped key.
func UnwrapPrivateKey(kek []byte, wrapped string) (string, error) {
	parts := strings.SplitN(wrapped, ".", 2)
	if len(parts) != 2 {
		return "", errors.New("malformed wrapped key")
	}
	return Decrypt(kek, parts[0], parts[1])
}

// GenerateSigningKeyPair returns a new Ed25519 keypair.
func GenerateSigningKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	return pub, priv, err
}

// EncodePublicKeyJava encodes an Ed25519 public key as base64(SPKI DER),
// matching Java X509EncodedKeySpec output.
func EncodePublicKeyJava(pub ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// EncodePrivateKeyJava encodes an Ed25519 private key as base64(PKCS8 DER),
// matching Java PKCS8EncodedKeySpec output.
func EncodePrivateKeyJava(priv ed25519.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// DecodePublicKeyJava parses base64(SPKI DER) into an Ed25519 public key.
func DecodePublicKeyJava(publicKeyB64 string) (ed25519.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not an Ed25519 public key")
	}
	return pub, nil
}

// DecodePrivateKeyJava parses base64(PKCS8 DER) into an Ed25519 private key.
func DecodePrivateKeyJava(privateKeyB64 string) (ed25519.PrivateKey, error) {
	der, err := base64.StdEncoding.DecodeString(privateKeyB64)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an Ed25519 private key")
	}
	return priv, nil
}

// SignaturePayload builds the signed payload: roomId|sender|ciphertext|nonce.
// Signature is over ciphertext so verification runs before decryption (Java parity).
func SignaturePayload(roomID, sender, ciphertextB64, nonceB64 string) []byte {
	return []byte(roomID + "|" + sender + "|" + ciphertextB64 + "|" + nonceB64)
}

// Sign returns base64(Ed25519 signature) — Java parity.
func Sign(priv ed25519.PrivateKey, payload []byte) string {
	sig := ed25519.Sign(priv, payload)
	return base64.StdEncoding.EncodeToString(sig)
}

// Verify checks a base64 Ed25519 signature — Java parity.
func Verify(pub ed25519.PublicKey, payload []byte, signatureB64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, payload, sig)
}
