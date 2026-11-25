package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const adaptiveStreamHistoryKey = "gateway:adaptive:stream-health:samples:v2"

var _ service.AdaptiveStreamHistoryStore = (*adaptiveHistoryStore)(nil)

func (s *adaptiveHistoryStore) ReadStreamHealth(ctx context.Context, after, through time.Time) ([]service.AdaptiveHistoryRecord, error) {
	return s.readSamples(ctx, adaptiveStreamHistoryKey, after, through)
}

const adaptiveStreamHistoryMerge = `
local key = KEYS[1]
local cutoff = tonumber(ARGV[1])
for i = 4, #ARGV, 2 do
  if tonumber(ARGV[i]) > cutoff then
    redis.call('ZADD', key, ARGV[i], ARGV[i+1])
  end
end
redis.call('ZREMRANGEBYSCORE', key, '-inf', ARGV[1])
local excess = redis.call('ZCARD', key) - tonumber(ARGV[2])
if excess > 0 then
  redis.call('ZREMRANGEBYRANK', key, 0, excess - 1)
end
redis.call('EXPIRE', key, ARGV[3])
return math.max(excess, 0)`

func (s *adaptiveHistoryStore) MergeStreamHealth(ctx context.Context, cutoff time.Time, records []service.AdaptiveHistoryRecord) (int64, error) {
	args := make([]any, 0, 3+2*len(records))
	args = append(args, adaptiveHistoryTime(cutoff), adaptiveHistoryLimit, int((15*time.Minute)/time.Second))
	for _, record := range records {
		if record.StateKey == "" && len(record.Payload) <= service.AdaptiveHistoryMaxMember {
			args = append(args, adaptiveHistoryTime(record.At), record.Payload)
		}
	}
	if len(args) == 3 {
		return 0, nil
	}
	return s.redis.Eval(ctx, adaptiveStreamHistoryMerge, []string{adaptiveStreamHistoryKey}, args...).Int64()
}
