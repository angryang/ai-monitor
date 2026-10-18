// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Cipher is the encryption / decryption interface used by ConfigStore. We
// keep it small so the implementation can be swapped (e.g. KMS-backed) in P2.
type Cipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// AESCipher implements Cipher using AES-256-GCM. The on-wire layout is:
//
//	nonce (12 bytes) || ciphertext || tag (16 bytes)
//
// which matches every other AES-GCM consumer (e.g. libsodium's aead). This
// shape also means we never store the nonce separately; one AEAD instance per
// master key ensures concurrency safety.
type AESCipher struct {
	aead cipher.AEAD
}

// ErrInvalidKey is returned when the supplied key doesn't fit AES-256.
var ErrInvalidKey = errors.New("config: master key must be 32 bytes")

// NewAESCipher constructs an AES-GCM AEAD with a 32-byte key (AES-256). The
// returned cipher is safe for concurrent use across goroutines.
func NewAESCipher(masterKey []byte) (*AESCipher, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("got %d bytes: %w", len(masterKey), ErrInvalidKey)
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("config: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("config: cipher.NewGCM: %w", err)
	}
	return &AESCipher{aead: aead}, nil
}

// Encrypt seals plaintext under the master key. A fresh nonce is generated
// cryptographically for every call; callers MUST NOT reuse nonces manually.
func (c *AESCipher) Encrypt(plaintext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, fmt.Errorf("config: nil cipher")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("config: nonce gen: %w", err)
	}
	dst := c.aead.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(nonce)+len(dst))
	out = append(out, nonce...)
	out = append(out, dst...)
	return out, nil
}

// Decrypt opens a sealed blob produced by Encrypt. Wrong-key / tampered data
// surfaces as an error; we deliberately do not distinguish them so we don't
// leak oracle info.
func (c *AESCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, fmt.Errorf("config: nil cipher")
	}
	ns := c.aead.NonceSize()
	if len(ciphertext) < ns+c.aead.Overhead() {
		return nil, fmt.Errorf("config: ciphertext too short: %w", ErrInvalidKey)
	}
	nonce, body := ciphertext[:ns], ciphertext[ns:]
	plain, err := c.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("config: decrypt: %w", err)
	}
	return plain, nil
}
