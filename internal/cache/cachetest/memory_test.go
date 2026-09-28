package cachetest

import (
	"testing"

	"bot/internal/cache"
)

func TestMemoryIndex(t *testing.T) {
	IndexContract(t, func(t *testing.T) (cache.Index, func(*testing.T) cache.Index) {
		m := NewMemory()
		return m, func(*testing.T) cache.Index { return m }
	})
}
