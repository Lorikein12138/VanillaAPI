package repository

import (
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type integrationHarnessCleanup struct {
	steps []func(context.Context) error
}

func (c *integrationHarnessCleanup) add(fn func(context.Context) error) {
	c.steps = append(c.steps, fn)
}

func runIntegrationHarnessLifecycle(run func(*integrationHarnessCleanup) int) (code int) {
	cleanup := &integrationHarnessCleanup{}
	defer func() {
		for i := len(cleanup.steps) - 1; i >= 0; i-- {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			done := make(chan error, 1)
			fn := cleanup.steps[i]
			go func() { done <- fn(ctx) }()
			select {
			case err := <-done:
				if err != nil {
					log.Printf("integration cleanup step %d failed: %v", i, err)
					code = 1
				}
			case <-ctx.Done():
				log.Printf("integration cleanup step %d timed out: %v", i, ctx.Err())
				code = 1
			}
			cancel()
		}
	}()
	return run(cleanup)
}

func TestIntegrationHarnessCleanupLifecycle(t *testing.T) {
	for _, scenario := range []struct {
		name               string
		resources, runCode int
		fail               bool
	}{
		{"normal", 5, 0, false}, {"partial_initialization", 2, 1, false}, {"cleanup_failure", 3, 0, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var order []int
			code := runIntegrationHarnessLifecycle(func(cleanup *integrationHarnessCleanup) int {
				for i := range scenario.resources {
					cleanup.add(func(ctx context.Context) error {
						_, bounded := ctx.Deadline()
						require.True(t, bounded)
						order = append(order, i)
						if scenario.fail && i == 1 {
							return errors.New("close failed")
						}
						return nil
					})
				}
				return scenario.runCode
			})
			for i, actual := range order {
				require.Equal(t, scenario.resources-i-1, actual)
			}
			require.Len(t, order, scenario.resources)
			if scenario.fail {
				require.Equal(t, 1, code)
			} else {
				require.Equal(t, scenario.runCode, code)
			}
		})
	}
}
