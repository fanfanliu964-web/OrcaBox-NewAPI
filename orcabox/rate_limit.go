package orcabox

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
)

type rateLimits struct {
	RPM int64 `json:"rpm"`
	TPM int64 `json:"tpm"`
}

var activeRateLimits = rateLimits{RPM: 20, TPM: 200000}

func loadRateLimits() (rateLimits, error) {
	limits := rateLimits{RPM: 20, TPM: 200000}
	for _, setting := range []struct {
		name             string
		value            *int64
		minimum, maximum int64
	}{
		{"ORCABOX_RPM_LIMIT", &limits.RPM, 1, 600},
		{"ORCABOX_TPM_LIMIT", &limits.TPM, 8192, 10000000},
	} {
		if raw := os.Getenv(setting.name); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < setting.minimum || value > setting.maximum {
				return limits, fmt.Errorf("invalid managed rate limit: %s", setting.name)
			}
			*setting.value = value
		}
	}
	return limits, nil
}

// Keep the original admission timestamp and one RPM event. Only replace the
// request's provisional TPM amount; never recreate expired or lost windows.
const settleTokenLimitScript = `
local score=redis.call('ZSCORE',KEYS[1],ARGV[1])
if not score or tonumber(score)<=tonumber(ARGV[4])-60000 then return 0 end
redis.call('ZREM',KEYS[1],ARGV[1])
redis.call('ZADD',KEYS[1],score,ARGV[2]..':'..ARGV[3])
return 1`

func rateLimitResponse(c *gin.Context, accountID int, reason, message string, retrySeconds int) {
	c.Header("X-Orcabox-Failure-Class", reason)
	c.Header("Retry-After", strconv.Itoa(retrySeconds))
	common.SysLog(fmt.Sprintf("orcabox admission rejected reason=%s account_id=%d", reason, accountID))
	c.AbortWithStatusJSON(429, gin.H{"error": gin.H{"type": "orcabox_rate_limit", "code": reason, "message": message}})
}

func reconcileRateUsage(ctx context.Context, client *redis.Client, key, id string, reserved int64, usage *Usage, forwarded bool, now int64) error {
	actual := int64(0)
	if forwarded {
		// An unknown supplier outcome must retain its estimate for the rest of
		// the original minute. Cached/reasoning tokens are already included.
		if usage == nil || usage.Input < 0 || usage.Output < 0 || usage.Cached < 0 || usage.CacheWrite < 0 ||
			usage.Cached > usage.Input || usage.CacheWrite > usage.Input-usage.Cached || usage.Input > math.MaxInt64-usage.Output {
			return nil
		}
		actual = usage.Input + usage.Output
	}
	member := id + ":" + strconv.FormatInt(reserved, 10)
	return client.Eval(ctx, settleTokenLimitScript, []string{key}, member, id, actual, now).Err()
}
