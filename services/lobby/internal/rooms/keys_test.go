package rooms

import (
	"strings"
	"testing"
)

// hashTag is the part of a key Redis Cluster hashes: the text between the first { and the next }.
func hashTag(t *testing.T, key string) string {
	t.Helper()
	start := strings.IndexByte(key, '{')
	end := strings.IndexByte(key[start+1:], '}')
	if start < 0 || end <= 0 {
		t.Fatalf("%s has no hash tag", key)
	}
	return key[start+1 : start+1+end]
}

// Save, Move and Remove change a room and its indexes in one MULTI or Lua script, which Redis
// Cluster refuses across slots (CROSSSLOT): every key of the room index shares one hash tag.
func TestRoomIndexKeysShareAHashTag(t *testing.T) {
	for _, key := range []string{roomKey("r1"), publicKey, hostsKey, serverRoomsKey("gs-1"), serverRoomsKey("gs-2")} {
		if got := hashTag(t, key); got != "rooms" {
			t.Errorf("%s: hash tag %q, want rooms", key, got)
		}
	}
}
