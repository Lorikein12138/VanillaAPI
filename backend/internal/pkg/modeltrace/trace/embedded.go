package trace

import (
	_ "embed"
	"sync"
)

//go:embed bank.fb
var embeddedBank []byte

// EmbeddedBank loads the pinned bank once. Callers share it read-only.
var EmbeddedBank = sync.OnceValues(func() (*Bank, error) { return Open(embeddedBank) })
