package accesstest

import (
	"testing"

	"github.com/kirkmadraga/vox-engine/internal/access"
)

func TestMemoryBackend(t *testing.T) {
	BackendContract(t, func(t *testing.T) (access.Backend, func(*testing.T) access.Backend) {
		m := NewMemory()
		return m, func(*testing.T) access.Backend { return m }
	})
}
