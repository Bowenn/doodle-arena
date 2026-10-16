// Package ticket signs the join tickets a client sends in its first WebSocket message
// (ADR 0005). The lobby holds the Ed25519 private key; game servers hold only the public keys,
// so they can verify tickets but never mint them. A ticket is a JWT with alg EdDSA that names
// the user and the room, lives for 60 seconds, and has a unique id (jti) that the game server
// records with SET NX, so each ticket works once.
package ticket

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// Issuer is the iss claim of every ticket.
	Issuer = "doodle-lobby"
	// Audience is the aud claim: only game servers accept tickets.
	Audience = "doodle-game"
	// TTL is how long a ticket is valid.
	TTL = 60 * time.Second
	// Leeway is the clock skew a verifier allows; tickets are short, so it is small.
	Leeway = 5 * time.Second
)

// ErrInvalid means the ticket can't be used: malformed, expired, for another room, or not
// signed by the lobby.
var ErrInvalid = errors.New("invalid join ticket")

// Claims is the content of a ticket.
type Claims struct {
	jwt.RegisteredClaims
	// RoomID is the only room the ticket opens.
	RoomID string `json:"room_id"`
}

// Signer mints tickets with the lobby's private key.
type Signer struct {
	key ed25519.PrivateKey
	kid string
	now func() time.Time
}

// NewSigner returns a signer for key. now is the clock (time.Now outside tests).
func NewSigner(key ed25519.PrivateKey, now func() time.Time) *Signer {
	return &Signer{key: key, kid: Thumbprint(key.Public().(ed25519.PublicKey)), now: now}
}

// LoadSigner reads a PKCS#8 PEM Ed25519 private key (scripts/gen-ticket-key.sh).
func LoadSigner(path string) (*Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ticket key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("ticket key: no PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ticket key: %w", err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("ticket key: %T is not an Ed25519 key", parsed)
	}
	return NewSigner(key, time.Now), nil
}

// KeyID is the RFC 7638 thumbprint of the signer's public key, sent as the kid header.
func (s *Signer) KeyID() string { return s.kid }

// Sign returns a ticket for userID in roomID and its expiry time.
func (s *Signer) Sign(userID, roomID string) (string, time.Time, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("ticket id: %w", err)
	}
	now := s.now().Truncate(time.Second)
	exp := now.Add(TTL)
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Audience:  jwt.ClaimStrings{Audience},
			Subject:   userID,
			ID:        hex.EncodeToString(id[:]),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		RoomID: roomID,
	})
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign ticket: %w", err)
	}
	return signed, exp, nil
}

// Verifier checks tickets against the lobby's public keys. Game servers implement the same
// rules (task 2.2); the lobby uses this one in its tests.
type Verifier struct {
	keys map[string]ed25519.PublicKey
	now  func() time.Time
}

// NewVerifier accepts tickets signed by any of keys. During a rotation it holds the old and
// the new key.
func NewVerifier(keys []ed25519.PublicKey, now func() time.Time) *Verifier {
	m := make(map[string]ed25519.PublicKey, len(keys))
	for _, k := range keys {
		m[Thumbprint(k)] = k
	}
	return &Verifier{keys: m, now: now}
}

// Verify checks the signature, issuer, audience, expiry and room, and returns the claims.
// It doesn't check single use; that needs the game server's Redis.
func (v *Verifier) Verify(token, roomID string) (Claims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(Issuer),
		jwt.WithAudience(Audience),
		jwt.WithLeeway(Leeway),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(v.now),
	)
	var c Claims
	_, err := parser.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if k, ok := v.keys[kid]; ok {
			return k, nil
		}
		return nil, fmt.Errorf("unknown ticket key %q", kid)
	})
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if c.Subject == "" || c.ID == "" {
		return Claims{}, fmt.Errorf("%w: sub and jti are required", ErrInvalid)
	}
	if c.ExpiresAt.Sub(c.IssuedAt.Time) > TTL {
		return Claims{}, fmt.Errorf("%w: lifetime longer than %s", ErrInvalid, TTL)
	}
	if c.RoomID != roomID {
		return Claims{}, fmt.Errorf("%w: ticket is for another room", ErrInvalid)
	}
	return c, nil
}

// Thumbprint is the RFC 7638 JWK thumbprint of an Ed25519 public key (RFC 8037 OKP key).
func Thumbprint(pub ed25519.PublicKey) string {
	// Members in lexicographic order, no whitespace, as RFC 7638 requires.
	canonical, _ := json.Marshal(struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
	}{"Ed25519", "OKP", base64.RawURLEncoding.EncodeToString(pub)})
	sum := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
