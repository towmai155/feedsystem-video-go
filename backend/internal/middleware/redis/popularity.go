package redis

import (
	"context"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// ErrPopularityWindowExpired reports an event that arrived after its one-minute
// window left the feed's readable horizon. The counter in MySQL stays
// authoritative, so the event is acknowledged instead of being retried forever.
var ErrPopularityWindowExpired = errors.New("popularity window already expired")

// Lua execution is atomic, but Redis does not roll back a script's earlier
// commands if a later command errors. Validate key types before any writes and
// put the dedup marker after the increment and expiration have succeeded.
// Returns 1 when the score was applied, 2 when the window had already expired,
// and 0 when the event was processed before.
var applyPopularityScript = goredis.NewScript(`
if redis.call("EXISTS", KEYS[1]) == 1 then return 0 end
local kind = redis.call("TYPE", KEYS[2]).ok
if kind ~= "none" and kind ~= "zset" then return redis.error_reply("invalid popularity key type") end
local now = tonumber(redis.call("TIME")[1])
local expires = tonumber(ARGV[3])
redis.call("DEL", KEYS[3])
if expires > now then
  redis.call("ZINCRBY", KEYS[2], ARGV[2], ARGV[1])
  redis.call("EXPIREAT", KEYS[2], expires)
  redis.call("SET", KEYS[1], "1", "EXAT", math.max(expires + 3600, now + 3600))
  return 1
end
redis.call("SET", KEYS[1], "1", "EXAT", math.max(expires + 3600, now + 3600))
return 2
`)

// The marker outlives its two-hour score window. Old replays do not recreate an
// expired window; a retry after an ambiguous network error cannot increment twice.
func (c *Client) ApplyPopularityEvent(ctx context.Context, eventID, windowKey, member, detailKey string, delta int64, expiresAt time.Time) error {
	if c == nil || c.rdb == nil {
		return errors.New("Redis is unavailable")
	}
	if eventID == "" || delta == 0 {
		return errors.New("invalid popularity event")
	}
	result, err := applyPopularityScript.Run(ctx, c.rdb, []string{c.Key("processed:popularity:%s", eventID), windowKey, detailKey},
		member, delta, expiresAt.Unix()).Int()
	if err != nil {
		return err
	}
	if result == 2 {
		return ErrPopularityWindowExpired
	}
	return nil
}
