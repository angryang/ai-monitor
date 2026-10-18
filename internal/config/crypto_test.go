// Copyright (c) 2026 yangchun <825660677@qq.com>
// SPDX-License-Identifier: MIT

package config

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func mustCipher(t *testing.T, key []byte) *AESCipher {
	t.Helper()
	c, err := NewAESCipher(key)
	if err != nil {
		t.Fatalf("NewAESCipher: %v", err)
	}
	return c
}

func TestAESCipher_RoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	c := mustCipher(t, key)

	cases := [][]byte{
		[]byte("hello"),
		[]byte(""),
		bytes.Repeat([]byte("x"), 1<<10),
		{0, 1, 2, 3, 0xa5, 0xff},
	}
	for _, plain := range cases {
		ct, err := c.Encrypt(plain)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		got, err := c.Decrypt(ct)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("roundtrip mismatch: got %x, want %x", got, plain)
		}
	}
}

func TestAESCipher_WrongKeyFail(t *testing.T) {
	key1 := bytes.Repeat([]byte("a"), 32)
	key2 := bytes.Repeat([]byte("b"), 32)
	c1 := mustCipher(t, key1)
	c2 := mustCipher(t, key2)
	ct, err := c1.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	_, err = c2.Decrypt(ct)
	if err == nil {
		t.Fatalf("expected wrong-key error")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("error should be decrypt-related: %v", err)
	}
}

func TestAESCipher_TamperedCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	c := mustCipher(t, key)
	ct, _ := c.Encrypt([]byte("secret"))
	ct[len(ct)-1] ^= 0x55 // flip a tag bit
	_, err := c.Decrypt(ct)
	if err == nil {
		t.Fatalf("expected tamper error")
	}
}

func TestNewAESCipher_BadKey(t *testing.T) {
	for _, key := range [][]byte{nil, []byte("short"), bytes.Repeat([]byte("a"), 31), bytes.Repeat([]byte("a"), 33)} {
		if _, err := NewAESCipher(key); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("key len %d: got %v, want ErrInvalidKey", len(key), err)
		}
	}
}

func TestAESCipher_NonceRandomness(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	c := mustCipher(t, key)
	seen := make(map[string]struct{})
	for i := 0; i < 32; i++ {
		ct, err := c.Encrypt([]byte("x"))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		nonce := string(ct[:12])
		if _, dup := seen[nonce]; dup {
			t.Fatalf("nonce reused: %x", ct[:12])
		}
		seen[nonce] = struct{}{}
	}
}
