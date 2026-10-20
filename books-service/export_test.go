package main

import (
	"sync"
	"testing"
	"time"
)

// resetHTTPClientForTest сбрасывает закэшированный клиент: тесты меняют
// HTTP_PROXY через t.Setenv, а клиент собирается лениво.
func resetHTTPClientForTest(t *testing.T) {
	t.Helper()

	reset := func() {
		clientOnce = sync.Once{}
		sharedClient = nil
	}
	reset()
	t.Cleanup(reset)
}

// resetMirrorCacheForTest сбрасывает кэш активного зеркала и negative cache.
func resetMirrorCacheForTest(t *testing.T) {
	t.Helper()

	mirrorMu.Lock()
	prevM, prevAt, prevFail := activeMirror, activeMirrorAt, mirrorFailedAt
	activeMirror, activeMirrorAt, mirrorFailedAt = "", time.Time{}, time.Time{}
	mirrorMu.Unlock()

	t.Cleanup(func() {
		mirrorMu.Lock()
		activeMirror, activeMirrorAt, mirrorFailedAt = prevM, prevAt, prevFail
		mirrorMu.Unlock()
	})
}

// resetSourcesCacheForTest сбрасывает кэш списка источников.
func resetSourcesCacheForTest(t *testing.T) {
	t.Helper()

	clear := func() {
		sourcesMu.Lock()
		sourcesList, sourcesAt, sourcesKeyAt = nil, time.Time{}, ""
		sourcesMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}
