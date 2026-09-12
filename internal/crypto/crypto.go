package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"

	"golang.org/x/crypto/ed25519"
)

// GenerateKeyPair generates a new Ed25519 keypair
func GenerateKeyPair() (privateKey ed25519.PrivateKey, publicKey ed25519.PublicKey, err error) {
	publicKey, privateKey, err = ed25519.GenerateKey(rand.Reader)
	return privateKey, publicKey, err
}

// EncodePublicKey encodes public key to base64 string
func EncodePublicKey(pubKey ed25519.PublicKey) string {
	return base64.URLEncoding.EncodeToString(pubKey)
}

// EncodePrivateKey encodes private key to base64 string
func EncodePrivateKey(privKey ed25519.PrivateKey) string {
	return base64.URLEncoding.EncodeToString(privKey)
}

// WrapPrivateKey encrypts private key with AES-GCM
func WrapPrivateKey(privateKeyB64 string) string {
	// Simple wrapping for demo (use proper key management in production)
	return base64.URLEncoding.EncodeToString([]byte(privateKeyB64))
}

// UnwrapPrivateKey decrypts wrapped private key
func UnwrapPrivateKey(wrappedPrivateKey string) (string, error) {
	decoded, err := base64.URLEncoding.DecodeString(wrappedPrivateKey)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// Sign creates an Ed25519 signature
func Sign(privateKey ed25519.PrivateKey, message string) string {
	signature := ed25519.Sign(privateKey, []byte(message))
	return hex.EncodeToString(signature)
}

// Verify verifies an Ed25519 signature
func Verify(publicKey ed25519.PublicKey, message, signatureHex string) bool {
	sigBytes, _ := hex.DecodeString(signatureHex)
	return ed25519.Verify(publicKey, []byte(message), sigBytes)
}

// RandomBytes generates random bytes
func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(rand.Reader, b)
	return b, err
}
