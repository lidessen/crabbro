package siwc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/secret"
)

// Tokens are unverified OAuth credentials. They do not establish an identity.
// Callers must validate the ID token and granted scope before using a session.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	TokenType    string    `json:"token_type"`
	Scope        string    `json:"scope"`
	ExpiresAt    time.Time `json:"expires_at"`
	ClientID     string    `json:"client_id"`
}

// String and GoString prevent accidental disclosure in formatted logs.
func (Tokens) String() string   { return "[SIWC credentials redacted]" }
func (Tokens) GoString() string { return "[SIWC credentials redacted]" }

// GrantedDirect reports whether the token response includes the required scope.
func (t Tokens) GrantedDirect() bool {
	for _, scope := range strings.Fields(t.Scope) {
		if scope == DirectScope {
			return true
		}
	}
	return false
}

// Store keeps each complete token set in one atomic secret file. Individual token
// values must also be included in any capture/log scrubbing performed by callers;
// secret.Store.AllValues only sees the serialized record.
type Store struct{ Secrets *secret.Store }

func (s Store) Save(ref string, tokens Tokens) error {
	if s.Secrets == nil {
		return errors.New("SIWC secret store is not configured")
	}
	b, err := json.Marshal(tokens)
	if err != nil {
		return errors.New("encode SIWC credentials")
	}
	if err := s.Secrets.Set(ref, string(b)); err != nil {
		return errors.New("store SIWC credentials")
	}
	return nil
}
func (s Store) Load(ref string) (Tokens, error) {
	var t Tokens
	if s.Secrets == nil {
		return t, errors.New("SIWC secret store is not configured")
	}
	v, err := s.Secrets.Get(ref)
	if errors.Is(err, os.ErrNotExist) {
		return t, os.ErrNotExist
	}
	if err != nil {
		return t, errors.New("read SIWC credentials")
	}
	if json.Unmarshal([]byte(v), &t) != nil {
		return Tokens{}, errors.New("decode SIWC credentials")
	}
	return t, nil
}
func (s Store) Delete(ref string) error {
	if s.Secrets == nil {
		return errors.New("SIWC secret store is not configured")
	}
	if err := s.Secrets.Delete(ref); errors.Is(err, os.ErrNotExist) {
		return os.ErrNotExist
	} else if err != nil {
		return errors.New("delete SIWC credentials")
	}
	return nil
}

// HostID returns a persistent opaque identifier shared across processes.
func (s Store) HostID(ref string) (string, error) {
	if s.Secrets == nil || strings.TrimSpace(s.Secrets.Dir) == "" || secret.ValidRef(ref) != nil {
		return "", errors.New("invalid SIWC host store or reference")
	}
	id, err := s.Secrets.Get(ref)
	if err == nil {
		if id == "" {
			return "", errors.New("empty SIWC host ID")
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("read SIWC host ID")
	}
	var raw [16]byte
	// UUID version 4 format is the documented host identifier form.
	if err := randomBytes(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	id = fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
	if os.MkdirAll(s.Secrets.Dir, 0700) != nil {
		return "", errors.New("create SIWC host store")
	}
	f, err := os.CreateTemp(s.Secrets.Dir, ".siwc-host-*")
	if err != nil {
		return "", errors.New("create SIWC host ID")
	}
	defer os.Remove(f.Name())
	if f.Chmod(0600) != nil {
		f.Close()
		return "", errors.New("protect SIWC host ID")
	}
	if _, err = f.WriteString(id); err != nil {
		f.Close()
		return "", errors.New("write SIWC host ID")
	}
	if f.Close() != nil {
		return "", errors.New("write SIWC host ID")
	}
	if err = os.Link(f.Name(), filepath.Join(s.Secrets.Dir, ref)); err != nil && !errors.Is(err, os.ErrExist) {
		return "", errors.New("persist SIWC host ID")
	}
	id, err = s.Secrets.Get(ref)
	if err != nil || id == "" {
		return "", errors.New("read SIWC host ID")
	}
	return id, nil
}
