package repository

import (
	"context"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	adaptiveHistoryKey   = "gateway:adaptive:samples:v1"
	adaptiveHistoryLimit = 8192
)

type adaptiveHistoryStore struct {
	redis *redis.Client
}

func ProvideAdaptiveHistoryStore(rdb *redis.Client) service.AdaptiveHistoryStore {
	return &adaptiveHistoryStore{redis: rdb.WithTimeout(350 * time.Millisecond)}
}

func (s *adaptiveHistoryStore) Read(ctx context.Context, after, through time.Time) ([]service.AdaptiveHistoryRecord, error) {
	rows, err := s.redis.ZRevRangeByScoreWithScores(ctx, adaptiveHistoryKey, &redis.ZRangeBy{
		Min: "(" + adaptiveHistoryTime(after), Max: adaptiveHistoryTime(through), Count: adaptiveHistoryLimit,
	}).Result()
	if err != nil {
		return nil, err
	}
	records := make([]service.AdaptiveHistoryRecord, 0, len(rows))
	for _, row := range rows {
		if payload, ok := row.Member.(string); ok && len(payload) <= service.AdaptiveHistoryMaxMember {
			records = append(records, service.AdaptiveHistoryRecord{At: time.UnixMicro(int64(row.Score)), Payload: payload})
		}
	}
	return records, nil
}

func adaptiveHistoryTime(at time.Time) string {
	return strconv.FormatInt(at.UnixMicro(), 10)
}

// The same serialized sample is the same member on retries. Atomic pruning
// keeps the newest records across all writers sharing the key.
const adaptiveHistoryMerge = `
local key = KEYS[1]
local cutoff = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
for i = 4, #ARGV, 2 do
  if tonumber(ARGV[i]) > cutoff then
    redis.call('ZADD', key, ARGV[i], ARGV[i+1])
  end
end
redis.call('ZREMRANGEBYSCORE', key, '-inf', ARGV[1])
local excess = redis.call('ZCARD', key) - limit
if excess > 0 then
  redis.call('ZREMRANGEBYRANK', key, 0, excess - 1)
end
redis.call('EXPIRE', key, ARGV[3])
return 1`

func (s *adaptiveHistoryStore) Merge(ctx context.Context, cutoff time.Time, records []service.AdaptiveHistoryRecord) error {
	args := make([]any, 0, 3+2*len(records))
	args = append(args, adaptiveHistoryTime(cutoff), adaptiveHistoryLimit, int(service.AdaptiveHistoryRetention/time.Second))
	for _, record := range records {
		if len(record.Payload) <= service.AdaptiveHistoryMaxMember {
			args = append(args, adaptiveHistoryTime(record.At), record.Payload)
		}
	}
	if len(args) == 3 {
		return nil
	}
	return s.redis.Eval(ctx, adaptiveHistoryMerge, []string{adaptiveHistoryKey}, args...).Err()
}
