// Package security implements the credential primitives of the platform: a
// PBKDF2-HMAC-SHA256 password hash and opaque, hashed bearer session tokens.
package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// DefaultIterations is the PBKDF2 iteration count for new credentials.
const DefaultIterations = 120000

// saltBytes and tokenBytes size the random material of the platform.
const (
	saltBytes  = 16
	tokenBytes = 32
	keyBytes   = 32
)

// Hasher derives and verifies password hashes.
type Hasher struct {
	iterations int
}

// NewHasher builds a hasher. A non positive iteration count falls back to the
// platform default so misconfiguration cannot weaken stored credentials.
func NewHasher(iterations int) *Hasher {
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	return &Hasher{iterations: iterations}
}

// Iterations reports the configured PBKDF2 iteration count.
func (h *Hasher) Iterations() int { return h.iterations }

// Credential is the stored representation of a password.
type Credential struct {
	Hash       string
	Salt       string
	Iterations int
}

// Hash derives a new credential for the given password.
func (h *Hasher) Hash(password string) (Credential, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return Credential{}, apperr.Wrap(apperr.CodeInternal, "无法生成密码盐值", err)
	}
	derived := pbkdf2SHA256([]byte(password), salt, h.iterations, keyBytes)
	return Credential{
		Hash:       hex.EncodeToString(derived),
		Salt:       hex.EncodeToString(salt),
		Iterations: h.iterations,
	}, nil
}

// Verify compares a password against a stored credential in constant time.
func (h *Hasher) Verify(credential Credential, password string) error {
	salt, err := hex.DecodeString(credential.Salt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "存储的密码盐值已损坏", err)
	}
	expected, err := hex.DecodeString(credential.Hash)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "存储的密码摘要已损坏", err)
	}
	iterations := credential.Iterations
	if iterations <= 0 {
		iterations = h.iterations
	}
	derived := pbkdf2SHA256([]byte(password), salt, iterations, len(expected))
	if subtle.ConstantTimeCompare(derived, expected) != 1 {
		return apperr.New(apperr.CodeUnauthenticated, "邮箱或密码不正确")
	}
	return nil
}

// NeedsRehash reports whether a stored credential uses an outdated cost.
func (h *Hasher) NeedsRehash(credential Credential) bool {
	return credential.Iterations < h.iterations
}

// pbkdf2SHA256 implements PBKDF2 with HMAC-SHA256 as specified by RFC 8018. The
// platform keeps its own implementation so the server has no external
// cryptography dependency.
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	if iterations < 1 {
		iterations = 1
	}
	mac := hmac.New(sha256.New, password)
	hashLen := mac.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	block := make([]byte, 4)
	for index := 1; index <= blocks; index++ {
		binary.BigEndian.PutUint32(block, uint32(index))
		mac.Reset()
		mac.Write(salt)
		mac.Write(block)
		current := mac.Sum(nil)
		result := make([]byte, hashLen)
		copy(result, current)
		for round := 1; round < iterations; round++ {
			mac.Reset()
			mac.Write(current)
			current = mac.Sum(current[:0])
			for i := range result {
				result[i] ^= current[i]
			}
		}
		out = append(out, result...)
	}
	return out[:keyLen]
}

// Token is a freshly minted bearer token plus its stored hash.
type Token struct {
	Plaintext string
	Hash      string
}

// NewToken mints an opaque session token. Only the hash is ever persisted.
func NewToken() (Token, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, apperr.Wrap(apperr.CodeInternal, "无法生成会话令牌", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(raw)
	return Token{Plaintext: plaintext, Hash: HashToken(plaintext)}, nil
}

// HashToken maps a bearer token to its stored lookup hash.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// NewReference mints a short opaque business reference such as a settlement
// receipt number or a party code suffix.
func NewReference(prefix string, size int) (string, error) {
	if size <= 0 {
		size = 6
	}
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "无法生成业务流水号", err)
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(raw)), nil
}
