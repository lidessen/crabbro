package siwc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

func randomBytes(b []byte) error {
	if _, err := rand.Read(b); err != nil {
		return errors.New("generate random authorization value")
	}
	return nil
}

func randomValue() (string, error) {
	b := make([]byte, 32)
	if err := randomBytes(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
