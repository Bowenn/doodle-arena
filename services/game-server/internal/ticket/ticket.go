// Package ticket checks the join tickets that the lobby signs (ADR 0005). Game servers hold
// only the lobby's Ed25519 public keys, so they can verify tickets but never mint them. The
// rules match services/lobby/internal/ticket (another module's internal package can't be
// imported, so they are kept in step by tests on both sides).
package ticket

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/roomstore"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

const (
	// Issuer is the lobby.
	Issuer = "doodle-lobby"
	// Audience is every game server.
	Audience = "doodle-game"
	// TTL is the longest lifetime a ticket may have.
	TTL = 60 * time.Second
	// Leeway is the clock skew allowed on exp and iat.
	Leeway = 5 * time.Second
	// DefaultName is shown for a player whose ticket has no name.
	DefaultName  = "Player"
	maxNameRunes = 32
)

// Errors returned by Verify and Redeem.
var (
	ErrInvalid     = errors.New("invalid join ticket")
	ErrReused      = errors.New("join ticket already used")
	ErrUnavailable = errors.New("ticket store unavailable")
	// ErrNotOwner means this server doesn't hold the room's lease (it moved): the client
	// should ask the lobby where the room is now.
	ErrNotOwner = errors.New("this server doesn't own the room")
)

// Ticket is a verified ticket.
type Ticket struct {
	ID        string
	UserID    string
	Name      string
	RoomID    string
	ExpiresAt time.Time
}

type claims struct {
	jwt.RegisteredClaims
	RoomID string `json:"room_id"`
	Name   string `json:"name"`
}

// Verifier checks tickets against the lobby's public keys.
type Verifier struct {
	keys map[string]ed25519.PublicKey
	now  func() time.Time
}

// NewVerifier accepts tickets signed by any of keys (more than one during a rotation).
func NewVerifier(keys []ed25519.PublicKey, now func() time.Time) *Verifier {
	m := make(map[string]ed25519.PublicKey, len(keys))
	for _, k := range keys {
		m[Thumbprint(k)] = k
	}
	return &Verifier{keys: m, now: now}
}

// LoadPublicKeys reads one or more PEM public keys from comma-separated file paths.
func LoadPublicKeys(paths string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, path := range strings.Split(paths, ",") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read ticket public key: %w", err)
		}
		found, err := parsePublicKeys(data, path)
		if err != nil {
			return nil, err
		}
		keys = append(keys, found...)
	}
	if len(keys) == 0 {
		return nil, errors.New("no ticket public keys (TICKET_PUBLIC_KEY_PATH)")
	}
	return keys, nil
}

// ParsePublicKeys reads one or more PEM public keys from text: in AWS they arrive in an
// environment variable, because ECS injects secrets only as variables (ADR 0003).
func ParsePublicKeys(data []byte) ([]ed25519.PublicKey, error) {
	keys, err := parsePublicKeys(data, "TICKET_PUBLIC_KEY_PEM")
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errors.New("no ticket public keys (TICKET_PUBLIC_KEY_PEM)")
	}
	return keys, nil
}

func parsePublicKeys(data []byte, source string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("ticket public key %s: %w", source, err)
		}
		k, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("ticket public key %s: %T is not Ed25519", source, parsed)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// Verify checks the signature (EdDSA only), issuer, audience, expiry, lifetime and room.
// Single use is checked separately, by Redeem.
func (v *Verifier) Verify(token, roomID string) (Ticket, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(Issuer),
		jwt.WithAudience(Audience),
		jwt.WithLeeway(Leeway),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(v.now),
	)
	var c claims
	_, err := parser.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if k, ok := v.keys[kid]; ok {
			return k, nil
		}
		return nil, fmt.Errorf("unknown ticket key %q", kid)
	})
	if err != nil {
		return Ticket{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if c.Subject == "" || c.ID == "" || c.IssuedAt == nil {
		return Ticket{}, fmt.Errorf("%w: sub, jti and iat are required", ErrInvalid)
	}
	if c.ExpiresAt.Sub(c.IssuedAt.Time) > TTL {
		return Ticket{}, fmt.Errorf("%w: lifetime longer than %s", ErrInvalid, TTL)
	}
	if c.RoomID != roomID {
		return Ticket{}, fmt.Errorf("%w: ticket is for another room", ErrInvalid)
	}
	return Ticket{ID: c.ID, UserID: c.Subject, Name: cleanName(c.Name), RoomID: c.RoomID, ExpiresAt: c.ExpiresAt.UTC()}, nil
}

// cleanName keeps a display name printable and short; it comes from auth, but it is shown
// to every player in the room.
func cleanName(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s))
	if s == "" {
		return DefaultName
	}
	if r := []rune(s); len(r) > maxNameRunes {
		s = string(r[:maxNameRunes])
	}
	return s
}

// Thumbprint is the RFC 7638 JWK thumbprint of an Ed25519 public key (the lobby's kid).
func Thumbprint(pub ed25519.PublicKey) string {
	canonical, _ := json.Marshal(struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
	}{"Ed25519", "OKP", base64.RawURLEncoding.EncodeToString(pub)})
	sum := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Redeemer records that a ticket has been used.
type Redeemer interface {
	// Redeem returns nil the first time, ErrReused after that, ErrNotOwner when this server
	// no longer holds the room's lease with token (the fencing token the room was taken with),
	// or ErrUnavailable when it can't tell (the join must then fail: single use can't be
	// guaranteed).
	Redeem(ctx context.Context, t Ticket, token int64) error
}

// RedisRedeemer stores used ticket ids under the game server's own prefix
// (room:{room_id}:ticket:{ticket_id}) with SET NX, only while this server holds the room's
// lease with the room's fencing token: like every write to a room's keys, it is checked
// against lease == <server_id>:<token> (ADR 0007).
type RedisRedeemer struct {
	rdb      redis.UniversalClient
	serverID string
	now      func() time.Time
}

// NewRedisRedeemer returns a redeemer backed by Redis for the game server serverID.
func NewRedisRedeemer(rdb redis.UniversalClient, serverID string, now func() time.Time) *RedisRedeemer {
	return &RedisRedeemer{rdb: rdb, serverID: serverID, now: now}
}

var redeemScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return -1 end
if redis.call('SET', KEYS[2], '1', 'NX', 'PX', ARGV[2]) then return 1 end
return 0
`)

// Redeem implements Redeemer. The record lives as long as the ticket could still verify
// (exp plus the leeway), so a replay can never outlive it.
func (r *RedisRedeemer) Redeem(ctx context.Context, t Ticket, token int64) error {
	ttl := t.ExpiresAt.Add(Leeway).Sub(r.now())
	if ttl < time.Second {
		ttl = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	res, err := redeemScript.Run(ctx, r.rdb, []string{roomstore.LeaseKey(t.RoomID), roomstore.TicketKey(t.RoomID, t.ID)},
		roomstore.LeaseValue(r.serverID, token), ttl.Milliseconds()).Int()
	switch {
	case err != nil:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	case res < 0:
		return ErrNotOwner
	case res == 0:
		return ErrReused
	}
	return nil
}
