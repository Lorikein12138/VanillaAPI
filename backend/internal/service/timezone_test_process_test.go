//go:build unit

package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	appTimezone "github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

const serviceTimezoneEnv = "SUB2API_TEST_SERVICE_TIMEZONE"
const serviceTimezoneCaseEnv = "SUB2API_TEST_SERVICE_TIMEZONE_CASE"

func serviceTimezoneChild(name, zone string) ([]byte, error) {
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^"+regexp.QuoteMeta(name)+"$", "-test.v")
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, serviceTimezoneEnv+"=") && !strings.HasPrefix(env, serviceTimezoneCaseEnv+"=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, serviceTimezoneEnv+"="+zone, serviceTimezoneCaseEnv+"="+name)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return output, fmt.Errorf("timezone child %s: %w", name, ctx.Err())
	}
	if err == nil && !strings.Contains(string(output), "--- PASS: "+name+" (") {
		err = fmt.Errorf("timezone child %s did not execute and pass", name)
	}
	return output, err
}

func runServiceTimezoneTestInSubprocess(t *testing.T, zone string) bool {
	t.Helper()
	if child := os.Getenv(serviceTimezoneCaseEnv); child != "" {
		require.Equal(t, t.Name(), child)
		require.Equal(t, zone, appTimezone.Name())
		return false
	}
	output, err := serviceTimezoneChild(t.Name(), zone)
	t.Logf("timezone child output:\n%s", output)
	require.NoError(t, err)
	return true
}

func TestServiceTimezoneSubprocessPreservesParent(t *testing.T) {
	local, name := time.Local, appTimezone.Name()
	for _, test := range []string{"TestGroupUsageDateUsesConfiguredTimezoneBoundary", "TestGroupUsageParseDateUsesConfiguredTimezone", "TestGroupUsageYesterdayStartHandlesDST"} {
		output, err := serviceTimezoneChild(test, "America/New_York")
		require.NoError(t, err, "%s", output)
	}
	require.Same(t, local, time.Local)
	require.Equal(t, name, appTimezone.Name())
}

func TestServiceTimezoneSubprocessPropagatesFailure(t *testing.T) {
	if os.Getenv(serviceTimezoneCaseEnv) == t.Name() {
		t.Fatal("intentional child failure")
	}
	output, err := serviceTimezoneChild(t.Name(), "America/New_York")
	require.Error(t, err)
	require.Contains(t, string(output), "intentional child failure")
	require.Contains(t, string(output), "--- FAIL: "+t.Name())
}
