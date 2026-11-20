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
	adaptiveHealthIndex  = "gateway:adaptive:health:index:v1"
	adaptiveHealthData   = "gateway:adaptive:health:data:v1"
	adaptiveHealthLimit  = 4096
	adaptiveHealthTTL    = 7 * 24 * time.Hour
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
	states, err := s.redis.Eval(ctx, adaptiveHealthRead, []string{adaptiveHealthIndex, adaptiveHealthData},
		adaptiveHistoryTime(through.Add(-adaptiveHealthTTL)), adaptiveHistoryTime(through), adaptiveHealthLimit).StringSlice()
	if err != nil {
		return nil, err
	}
	for i := 0; i+2 < len(states); i += 3 {
		score, err := strconv.ParseInt(states[i+1], 10, 64)
		if err == nil && len(states[i+2]) <= service.AdaptiveHistoryMaxMember {
			records = append(records, service.AdaptiveHistoryRecord{
				At: time.UnixMicro(score), Payload: states[i+2], StateKey: states[i],
			})
		}
	}
	return records, nil
}

const adaptiveHealthRead = `
local out = {}
local rows = redis.call('ZREVRANGEBYSCORE', KEYS[1], ARGV[2], '(' .. ARGV[1], 'WITHSCORES', 'LIMIT', 0, ARGV[3])
for i = 1, #rows, 2 do
  local payload = redis.call('HGET', KEYS[2], rows[i])
  if payload then
    out[#out + 1] = rows[i]
    out[#out + 1] = rows[i + 1]
    out[#out + 1] = payload
  end
end
return out`

func adaptiveHistoryTime(at time.Time) string {
	return strconv.FormatInt(at.UnixMicro(), 10)
}

// The same serialized sample is the same member on retries. Atomic pruning
// keeps the newest records across all writers sharing the key.
const adaptiveHistoryMerge = `
local key = KEYS[1]
local cutoff = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local healthCutoff = tonumber(ARGV[4])
local healthLimit = tonumber(ARGV[5])
local healthChanged = false
for i = 7, #ARGV, 3 do
  local score = tonumber(ARGV[i])
  local stateID = ARGV[i+2]
  if stateID == '' then
    if score > cutoff then
      redis.call('ZADD', key, ARGV[i], ARGV[i+1])
    end
  elseif score > healthCutoff then
    local old = redis.call('ZSCORE', KEYS[2], stateID)
    if not old or score > tonumber(old) then
      redis.call('ZADD', KEYS[2], ARGV[i], stateID)
      redis.call('HSET', KEYS[3], stateID, ARGV[i+1])
      healthChanged = true
    end
  end
end
local expired = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', ARGV[4])
for _, id in ipairs(expired) do
  redis.call('HDEL', KEYS[3], id)
  redis.call('ZREM', KEYS[2], id)
end
local stateExcess = redis.call('ZCARD', KEYS[2]) - healthLimit
if stateExcess > 0 then
  local removed = redis.call('ZRANGE', KEYS[2], 0, stateExcess - 1)
  for _, id in ipairs(removed) do
    redis.call('HDEL', KEYS[3], id)
    redis.call('ZREM', KEYS[2], id)
  end
end
if healthChanged then
  redis.call('EXPIRE', KEYS[2], ARGV[6])
  redis.call('EXPIRE', KEYS[3], ARGV[6])
end
redis.call('ZREMRANGEBYSCORE', key, '-inf', ARGV[1])
local excess = redis.call('ZCARD', key) - limit
if excess > 0 then
  redis.call('ZREMRANGEBYRANK', key, 0, excess - 1)
end
redis.call('EXPIRE', key, ARGV[3])
return 1`

func (s *adaptiveHistoryStore) Merge(ctx context.Context, cutoff time.Time, records []service.AdaptiveHistoryRecord) error {
	args := make([]any, 0, 6+3*len(records))
	healthCutoff := cutoff.Add(service.AdaptiveHistoryRetention - adaptiveHealthTTL)
	args = append(args, adaptiveHistoryTime(cutoff), adaptiveHistoryLimit, int(service.AdaptiveHistoryRetention/time.Second),
		adaptiveHistoryTime(healthCutoff), adaptiveHealthLimit, int(adaptiveHealthTTL/time.Second))
	for _, record := range records {
		if len(record.Payload) <= service.AdaptiveHistoryMaxMember {
			args = append(args, adaptiveHistoryTime(record.At), record.Payload, record.StateKey)
		}
	}
	if len(args) == 6 {
		return nil
	}
	return s.redis.Eval(ctx, adaptiveHistoryMerge, []string{adaptiveHistoryKey, adaptiveHealthIndex, adaptiveHealthData}, args...).Err()
}
