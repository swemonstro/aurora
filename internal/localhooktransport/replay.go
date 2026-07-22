package localhooktransport

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"
)

type replayEntry struct {
	digest    [sha256.Size]byte
	response  Response
	expiresAt time.Time
}

type replayCache struct {
	mutex    sync.Mutex
	entries  map[string]replayEntry
	capacity int
	ttl      time.Duration
	clock    Clock
}

func newReplayCache(capacity int, ttl time.Duration, clock Clock) *replayCache {
	return &replayCache{entries: make(map[string]replayEntry), capacity: capacity, ttl: ttl, clock: clock}
}

func requestDigest(request Request) ([sha256.Size]byte, error) {
	data, err := json.Marshal(canonicalRequest(request))
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func (cache *replayCache) lookup(request Request) (Response, bool, error) {
	digest, err := requestDigest(request)
	if err != nil {
		return Response{}, false, err
	}
	now := canonicalTime(cache.clock.Now())
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.prune(now)
	entry, exists := cache.entries[request.RequestID]
	if !exists {
		return Response{}, false, nil
	}
	if entry.digest != digest {
		return Response{}, false, ErrReplayConflict
	}
	return entry.response, true, nil
}

func (cache *replayCache) store(request Request, response Response) error {
	digest, err := requestDigest(request)
	if err != nil {
		return err
	}
	now := canonicalTime(cache.clock.Now())
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.prune(now)
	if _, exists := cache.entries[request.RequestID]; !exists && len(cache.entries) >= cache.capacity {
		return ErrReplayCacheFull
	}
	cache.entries[request.RequestID] = replayEntry{digest: digest, response: response, expiresAt: now.Add(cache.ttl)}
	return nil
}

func (cache *replayCache) prune(now time.Time) {
	for requestID, entry := range cache.entries {
		if !now.Before(entry.expiresAt) {
			delete(cache.entries, requestID)
		}
	}
}
