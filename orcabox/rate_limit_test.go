package orcabox

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rateTestClient(t *testing.T) (*redis.Client, string, int64) {
	t.Helper()
	address := os.Getenv("ORCABOX_TEST_RATE_REDIS_ADDR")
	if address == "" {
		address = miniredis.RunT(t).Addr()
	}
	client := redis.NewClient(&redis.Options{Addr: address})
	key := "orcabox:test:rate:" + uuid.NewString()
	t.Cleanup(func() { client.Del(context.Background(), key, key+":active"); client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())
	return client, key, time.Now().UnixMilli()
}

func TestRateUsageReleasesUnusedEstimateWithoutCountingCacheTwice(t *testing.T) {
	client, key, now := rateTestClient(t)
	ctx := context.Background()
	require.NoError(t, client.ZAdd(ctx, key, &redis.Z{Score: float64(now), Member: "first:0"}).Err())
	accepted, err := client.Eval(ctx, tokenLimitScript, []string{key}, now, "first", 120000).Int()
	require.NoError(t, err)
	require.Equal(t, 1, accepted)
	require.NoError(t, client.ZAdd(ctx, key, &redis.Z{Score: float64(now), Member: "second:0"}).Err())
	accepted, err = client.Eval(ctx, tokenLimitScript, []string{key}, now, "second", 100000).Int()
	require.NoError(t, err)
	require.Equal(t, 0, accepted, "in-flight estimates must share one TPM budget")

	require.NoError(t, reconcileRateUsage(ctx, client, key, "first", 120000, &Usage{Input: 100, Output: 20, Cached: 80}, true, now+10))
	assert.Equal(t, float64(now), client.ZScore(ctx, key, "first:120").Val())
	accepted, err = client.Eval(ctx, tokenLimitScript, []string{key}, now+20, "second", 100000).Int()
	require.NoError(t, err)
	assert.Equal(t, 1, accepted, "settled small requests must not retain worst-case TPM")
	require.NoError(t, reconcileRateUsage(ctx, client, key, "first", 120000, &Usage{Input: 100, Output: 20}, true, now+30))
	assert.Equal(t, int64(2), client.ZCard(ctx, key).Val(), "replayed reconciliation must not add another entry")
}

func TestUnknownUsageKeepsReservationAndUnsentFailureOnlyReleasesTokens(t *testing.T) {
	client, key, now := rateTestClient(t)
	ctx := context.Background()
	require.NoError(t, client.ZAdd(ctx, key, &redis.Z{Score: float64(now), Member: "unknown:90000"}).Err())
	require.NoError(t, reconcileRateUsage(ctx, client, key, "unknown", 90000, nil, true, now+1))
	assert.Equal(t, float64(now), client.ZScore(ctx, key, "unknown:90000").Val())
	require.NoError(t, reconcileRateUsage(ctx, client, key, "unknown", 90000, nil, false, now+2))
	assert.Equal(t, float64(now), client.ZScore(ctx, key, "unknown:0").Val())
	assert.Equal(t, int64(1), client.ZCard(ctx, key).Val(), "RPM still counts an unsent failed attempt")
}

func TestLateAndLostWindowReconciliationCannotRecreateRateEntries(t *testing.T) {
	client, key, now := rateTestClient(t)
	ctx := context.Background()
	require.NoError(t, client.ZAdd(ctx, key, &redis.Z{Score: float64(now), Member: "old:90000"}).Err())
	require.NoError(t, reconcileRateUsage(ctx, client, key, "old", 90000, &Usage{Input: 50}, true, now+61000))
	assert.Equal(t, int64(1), client.ZCard(ctx, key).Val())
	assert.Equal(t, float64(now), client.ZScore(ctx, key, "old:90000").Val())
	require.NoError(t, client.Del(ctx, key).Err())
	require.NoError(t, reconcileRateUsage(ctx, client, key, "old", 90000, &Usage{Input: 50}, true, now+61000))
	assert.Equal(t, int64(0), client.Exists(ctx, key).Val())
}

func TestActualRateUsageStillEnforcesRPMAndConcurrency(t *testing.T) {
	client, key, now := rateTestClient(t)
	ctx := context.Background()
	for n := range 20 {
		id := fmt.Sprint(n)
		accepted, err := client.Eval(ctx, limitScript, []string{key, key + ":active"}, now, id).Int()
		require.NoError(t, err)
		require.Equal(t, 1, accepted)
		require.NoError(t, client.ZRem(ctx, key+":active", id).Err())
	}
	accepted, err := client.Eval(ctx, limitScript, []string{key, key + ":active"}, now, "twenty-first").Int()
	require.NoError(t, err)
	assert.Equal(t, -1, accepted)
	require.NoError(t, client.Del(ctx, key).Err())
	for n := range 2 {
		accepted, err = client.Eval(ctx, limitScript, []string{key, key + ":active"}, now, fmt.Sprint(n)).Int()
		require.NoError(t, err)
		require.Equal(t, 1, accepted)
	}
	accepted, err = client.Eval(ctx, limitScript, []string{key, key + ":active"}, now, "third").Int()
	require.NoError(t, err)
	assert.Equal(t, -2, accepted)
}

func TestInvalidRateUsageRetainsEstimate(t *testing.T) {
	for _, usage := range []*Usage{{Input: -1}, {Input: 10, Output: -1}, {Input: 10, Cached: 11}, {Input: 10, Cached: 8, CacheWrite: 3}, {Input: 9223372036854775807, Output: 1}} {
		client, key, now := rateTestClient(t)
		ctx := context.Background()
		require.NoError(t, client.ZAdd(ctx, key, &redis.Z{Score: float64(now), Member: "invalid:90000"}).Err())
		require.NoError(t, reconcileRateUsage(ctx, client, key, "invalid", 90000, usage, true, now+1))
		assert.Equal(t, float64(now), client.ZScore(ctx, key, "invalid:90000").Val())
	}
}

func TestRateLimitResponseNamesTheLocalBoundary(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	rateLimitResponse(c, 1, "account_tpm", "本站 Token 速率已达上限", 60)
	assert.Equal(t, 429, w.Code)
	assert.Equal(t, "account_tpm", w.Header().Get("X-Orcabox-Failure-Class"))
	assert.Equal(t, "60", w.Header().Get("Retry-After"))
	assert.Contains(t, w.Body.String(), `"code":"account_tpm"`)
}

func TestConfiguredLimitsRemainBoundedAndCanIncreasePilotCapacity(t *testing.T) {
	t.Setenv("ORCABOX_RPM_LIMIT", "60")
	t.Setenv("ORCABOX_TPM_LIMIT", "1000000")
	limits, err := loadRateLimits()
	require.NoError(t, err)
	assert.Equal(t, rateLimits{RPM: 60, TPM: 1000000}, limits)
	for _, values := range [][2]string{{"0", "1000000"}, {"601", "1000000"}, {"60", "0"}, {"60", "10000001"}, {"bad", "1000000"}} {
		t.Setenv("ORCABOX_RPM_LIMIT", values[0])
		t.Setenv("ORCABOX_TPM_LIMIT", values[1])
		_, err = loadRateLimits()
		assert.Error(t, err)
	}
}

func TestConfiguredRPMAndTPMAreEnforcedByRedis(t *testing.T) {
	client, key, now := rateTestClient(t)
	ctx := context.Background()
	for n := range 60 {
		id := fmt.Sprint(n)
		accepted, err := client.Eval(ctx, limitScript, []string{key, key + ":active"}, now, id, 60).Int()
		require.NoError(t, err)
		require.Equal(t, 1, accepted)
		require.NoError(t, client.ZRem(ctx, key+":active", id).Err())
	}
	accepted, err := client.Eval(ctx, limitScript, []string{key, key + ":active"}, now, "sixty-first", 60).Int()
	require.NoError(t, err)
	assert.Equal(t, -1, accepted)
	require.NoError(t, client.Del(ctx, key).Err())
	for _, request := range []struct {
		id       string
		tokens   int64
		expected int
	}{{"large", 600000, 1}, {"next", 500000, 0}} {
		require.NoError(t, client.ZAdd(ctx, key, &redis.Z{Score: float64(now), Member: request.id + ":0"}).Err())
		accepted, err = client.Eval(ctx, tokenLimitScript, []string{key}, now, request.id, request.tokens, 1000000).Int()
		require.NoError(t, err)
		assert.Equal(t, request.expected, accepted)
	}
}
