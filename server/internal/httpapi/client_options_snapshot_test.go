package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

func TestClientOptionsCachePreservesCurrentNodeSnapshot(t *testing.T) {
	for _, age := range []time.Duration{time.Minute, 10 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			current := subscriptionNode{ID: "snapshot-node", Name: "renamed-node", Host: "example.com", Port: 443, AccountUserID: "user", Password: "password", InStore: true, Reality: &nodeReality{PublicKey: "new-key", ShortID: "ab", DestSNI: "example.org", Port: 8443}}
			cached := current
			cached.Name = "old-name"
			cached.InStore = false
			cached.Reality = &nodeReality{PublicKey: "old-key", ShortID: "cd", DestSNI: "old.example.org", Port: 9443}
			cached.SNI = "transport.example.com"
			cached.Insecure = true
			cached.PinSHA256 = "cached-pin"
			cached.ObfsPass = "cached-obfs"
			cached.HopRange = "20000-30000"
			sum := sha256.Sum256([]byte(current.ID + "\x00" + current.AccountUserID + "\x00" + current.Password + "\x00" + current.Host + "\x00" + strconv.Itoa(current.Port)))
			key := hex.EncodeToString(sum[:])
			clientOptionsCache.Lock()
			clientOptionsCache.m[key] = clientOptionsEntry{node: cached, storedAt: time.Now().Add(-age)}
			clientOptionsCache.Unlock()
			t.Cleanup(func() { clientOptionsCache.Lock(); delete(clientOptionsCache.m, key); clientOptionsCache.Unlock() })
			// With no DB, the stale branch exercises the existing refresh-error fallback.
			got, err := (&Server{}).cachedSubscriptionClientOptions(context.Background(), current)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != current.Name || got.Reality != current.Reality || got.InStore != current.InStore {
				t.Fatalf("cache restored obsolete node metadata: %+v", got)
			}
			if got.SNI != cached.SNI || got.Insecure != cached.Insecure || got.PinSHA256 != cached.PinSHA256 || got.ObfsPass != cached.ObfsPass || got.HopRange != cached.HopRange {
				t.Fatalf("cached transport options lost: %+v", got)
			}
			body, err := renderSingBox([]subscriptionNode{got})
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Outbounds []map[string]any `json:"outbounds"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Outbounds[0]["tag"] != current.Name || doc.Outbounds[1]["server_port"] != float64(current.Reality.Port) {
				t.Fatalf("subscription rendered obsolete name or Reality endpoint: %s", body)
			}
		})
	}
}
