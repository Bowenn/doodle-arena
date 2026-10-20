package registry

import (
	"strings"
	"testing"
)

// Record writes a server's entry and the set in one MULTI, and Servers reads them together:
// Redis Cluster allows that only within one slot, so both share a hash tag.
func TestRegistryKeysShareAHashTag(t *testing.T) {
	for _, key := range []string{serversKey, serverKey("gs-1")} {
		start := strings.IndexByte(key, '{')
		end := strings.IndexByte(key[start+1:], '}')
		if start < 0 || end <= 0 || key[start+1:start+1+end] != "registry" {
			t.Errorf("%s: want the hash tag {registry}", key)
		}
	}
}
