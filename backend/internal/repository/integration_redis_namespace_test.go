package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type prefixHook struct {
	prefix string
}

func (h prefixHook) DialHook(next redisclient.DialHook) redisclient.DialHook { return next }

func (h prefixHook) ProcessHook(next redisclient.ProcessHook) redisclient.ProcessHook {
	return func(ctx context.Context, cmd redisclient.Cmder) error {
		h.prefixCmd(cmd)
		return next(ctx, cmd)
	}
}

func (h prefixHook) ProcessPipelineHook(next redisclient.ProcessPipelineHook) redisclient.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redisclient.Cmder) error {
		for _, cmd := range cmds {
			h.prefixCmd(cmd)
		}
		return next(ctx, cmds)
	}
}

func (h prefixHook) prefixCmd(cmd redisclient.Cmder) {
	args := cmd.Args()
	if len(args) < 2 {
		return
	}

	prefixOne := func(i int) {
		if i < 0 || i >= len(args) {
			return
		}

		switch v := args[i].(type) {
		case string:
			if v != "" && !strings.HasPrefix(v, h.prefix) {
				args[i] = h.prefix + v
			}
		case []byte:
			s := string(v)
			if s != "" && !strings.HasPrefix(s, h.prefix) {
				args[i] = []byte(h.prefix + s)
			}
		}
	}

	switch strings.ToLower(cmd.Name()) {
	case "get", "set", "setnx", "setex", "psetex", "incr", "decr", "incrby", "expire", "pexpire", "ttl", "pttl",
		"hgetall", "hget", "hset", "hdel", "hlen", "hexists", "hincrbyfloat", "exists",
		"zadd", "zcard", "zrange", "zrangebyscore", "zrem", "zremrangebyscore", "zrevrange", "zrevrangebyscore", "zscore":
		prefixOne(1)
	case "mget":
		for i := 1; i < len(args); i++ {
			prefixOne(i)
		}
	case "del", "unlink":
		for i := 1; i < len(args); i++ {
			prefixOne(i)
		}
	case "eval", "evalsha", "eval_ro", "evalsha_ro":
		if len(args) < 3 {
			return
		}
		numKeys, err := strconv.Atoi(fmt.Sprint(args[2]))
		if err != nil || numKeys <= 0 {
			return
		}
		for i := 0; i < numKeys && 3+i < len(args); i++ {
			prefixOne(3 + i)
		}
	case "scan":
		for i := 2; i+1 < len(args); i++ {
			if strings.EqualFold(fmt.Sprint(args[i]), "match") {
				prefixOne(i + 1)
				break
			}
		}
	}
}

func TestIntegrationRedisPrefixHookCommandRouting(t *testing.T) {
	ctx := context.Background()
	hook := prefixHook{prefix: "ns:"}
	for _, name := range []string{"hlen", "hexists", "hset", "hget", "hdel", "hgetall", "hincrbyfloat", "zadd", "zcard", "zrange", "zrangebyscore", "zrem", "zremrangebyscore", "zrevrange", "zrevrangebyscore", "zscore", "ttl", "pttl", "expire", "pexpire"} {
		t.Run(name, func(t *testing.T) {
			for _, key := range []any{"key", []byte("key"), "ns:key", []byte("ns:key")} {
				cmd := redisclient.NewCmd(ctx, name, key, "field", "value")
				called := 0
				sentinel := errors.New("next failure")
				err := hook.ProcessHook(func(gotCtx context.Context, got redisclient.Cmder) error {
					called++
					require.Same(t, cmd, got)
					require.Equal(t, ctx, gotCtx)
					return sentinel
				})(ctx, cmd)
				require.ErrorIs(t, err, sentinel)
				require.Equal(t, 1, called)
				want := any("ns:key")
				if _, ok := key.([]byte); ok {
					want = []byte("ns:key")
				}
				require.Equal(t, []any{name, want, "field", "value"}, cmd.Args())
				hook.prefixCmd(cmd)
				require.Equal(t, want, cmd.Args()[1])
			}
		})
	}
	for _, name := range []string{"eval", "evalsha", "eval_ro", "evalsha_ro"} {
		cmd := redisclient.NewCmd(ctx, name, "script", 2, "key", []byte("other"), "key", []byte("other"))
		hook.prefixCmd(cmd)
		require.Equal(t, []any{name, "script", 2, "ns:key", []byte("ns:other"), "key", []byte("other")}, cmd.Args())
	}
	for _, args := range [][]any{{"hlen"}, {"hexists"}, {"eval", "script"}, {"eval", "script", "invalid", "key"}, {"eval", "script", 0, "key"}, {"hget", "", "field"}, {"hget", 123, "field"}, {"ping"}} {
		cmd := redisclient.NewCmd(ctx, args...)
		before := append([]any(nil), args...)
		hook.prefixCmd(cmd)
		require.Equal(t, before, cmd.Args())
	}
	for _, transaction := range []bool{false, true} {
		cmds := []redisclient.Cmder{redisclient.NewCmd(ctx, "hlen", "key"), redisclient.NewCmd(ctx, "hexists", "key", "field")}
		if transaction {
			cmds = append([]redisclient.Cmder{redisclient.NewCmd(ctx, "multi")}, append(cmds, redisclient.NewCmd(ctx, "exec"))...)
		}
		sentinel := errors.New("pipeline failure")
		called := 0
		err := hook.ProcessPipelineHook(func(gotCtx context.Context, got []redisclient.Cmder) error {
			called++
			require.Equal(t, ctx, gotCtx)
			require.Equal(t, cmds, got)
			return sentinel
		})(ctx, cmds)
		require.ErrorIs(t, err, sentinel)
		require.Equal(t, 1, called)
		for _, cmd := range cmds {
			switch cmd.Name() {
			case "multi", "exec":
				require.Len(t, cmd.Args(), 1)
			default:
				require.Equal(t, "ns:key", cmd.Args()[1])
			}
		}
	}
	nextDial := redisclient.DialHook(nil)
	require.Nil(t, hook.DialHook(nextDial))
}

func TestIntegrationRedisNamespaceHashCommands(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	newClient := func(prefix string) *redisclient.Client {
		client := redisclient.NewClient(&redisclient.Options{Addr: mr.Addr()})
		if prefix != "" {
			client.AddHook(prefixHook{prefix: prefix})
		}
		t.Cleanup(func() { require.NoError(t, client.Close()) })
		return client
	}
	raw, a, b := newClient(""), newClient("a:"), newClient("b:")
	const key = "logical-hash"
	require.NoError(t, raw.HSet(ctx, key, "sentinel", "untouched").Err())
	require.NoError(t, a.HSet(ctx, key, "a-field", "a-value").Err())
	require.NoError(t, b.HSet(ctx, key, "b-field", "b-value").Err())
	check := func(client *redisclient.Client, own, foreign, value string, count int64) {
		n, err := client.HLen(ctx, key).Result()
		require.NoError(t, err)
		require.Equal(t, count, n)
		exists, err := client.HExists(ctx, key, own).Result()
		require.NoError(t, err)
		require.True(t, exists)
		exists, err = client.HExists(ctx, key, foreign).Result()
		require.NoError(t, err)
		require.False(t, exists)
		got, err := client.HGet(ctx, key, own).Result()
		require.NoError(t, err)
		require.Equal(t, value, got)
	}
	check(a, "a-field", "b-field", "a-value", 1)
	check(b, "b-field", "a-field", "b-value", 1)
	for _, transaction := range []bool{false, true} {
		for _, item := range []struct {
			client       *redisclient.Client
			own, foreign string
		}{{a, "a-field", "b-field"}, {b, "b-field", "a-field"}} {
			var pipe redisclient.Pipeliner
			if transaction {
				pipe = item.client.TxPipeline()
			} else {
				pipe = item.client.Pipeline()
			}
			set := pipe.HSet(ctx, key, "pipeline-field", "pipeline-value")
			length := pipe.HLen(ctx, key)
			exists := pipe.HExists(ctx, key, item.own)
			absent := pipe.HExists(ctx, key, item.foreign)
			get := pipe.HGet(ctx, key, "pipeline-field")
			_, err := pipe.Exec(ctx)
			require.NoError(t, err)
			require.NoError(t, set.Err())
			n, err := length.Result()
			require.NoError(t, err)
			require.Equal(t, int64(2), n)
			present, err := exists.Result()
			require.NoError(t, err)
			require.True(t, present)
			present, err = absent.Result()
			require.NoError(t, err)
			require.False(t, present)
			v, err := get.Result()
			require.NoError(t, err)
			require.Equal(t, "pipeline-value", v)
		}
	}
	check(a, "a-field", "b-field", "a-value", 2)
	check(b, "b-field", "a-field", "b-value", 2)
	values, err := raw.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"sentinel": "untouched"}, values)
}
