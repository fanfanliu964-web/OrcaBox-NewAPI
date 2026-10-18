package orcabox

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "golang.org/x/image/webp"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const Managed = "orcabox_managed"

var largePayloadSlots = make(chan struct{}, 1)
var imageDecodeSlots = make(chan struct{}, 1)
var errImageDecodeBusy = errors.New("图片解析繁忙，请稍后重试")

const limitScript = `
redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',ARGV[1]-60000)
redis.call('ZREMRANGEBYSCORE',KEYS[2],'-inf',ARGV[1])
if redis.call('ZCARD',KEYS[1])>=20 or redis.call('ZCARD',KEYS[2])>=2 then return 0 end
redis.call('ZADD',KEYS[1],ARGV[1],ARGV[2]..':0')
redis.call('ZADD',KEYS[2],ARGV[1]+230000,ARGV[2])
redis.call('EXPIRE',KEYS[1],70)
redis.call('EXPIRE',KEYS[2],240)
return 1`
const tokenLimitScript = `
redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',ARGV[1]-60000)
local total=0
for _,v in ipairs(redis.call('ZRANGE',KEYS[1],0,-1)) do total=total+tonumber(string.match(v,':(%d+)$')) end
if total+tonumber(ARGV[3])>200000 then return 0 end
redis.call('ZREM',KEYS[1],ARGV[2]..':0')
redis.call('ZADD',KEYS[1],ARGV[1],ARGV[2]..':'..ARGV[3])
redis.call('EXPIRE',KEYS[1],70)
return 1`

func errorResponse(c *gin.Context, code int, msg string) {
	c.AbortWithStatusJSON(code, gin.H{"error": gin.H{"message": msg, "type": "orcabox_error"}})
}
func Install(engine *gin.Engine) error {
	if os.Getenv("ORCABOX_MANAGED_GATEWAY") != "true" {
		return nil
	}
	if _, e := testBudgetLimit(); e != nil {
		return e
	}
	if len(os.Getenv("ORCABOX_INTERNAL_TOKEN")) < 48 || common.RDB == nil {
		return errors.New("managed gateway requires internal authentication and Redis")
	}
	prices, e := LoadPrices()
	if e != nil {
		return errors.New("managed price book unavailable")
	}
	model.DB.Logger = logger.Default.LogMode(logger.Silent)
	l := &Ledger{DB: model.DB}
	if e = l.Migrate(); e != nil {
		return e
	}
	if e = l.PublishPrices(prices); e != nil {
		return e
	}
	go l.monitorPrices(prices)
	engine.Use(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/orcabox/internal/") {
			internal(c, l, prices)
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			gateway(c, l, prices)
			return
		}
		c.Next()
	})
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for range tick.C {
			if e := l.Recover(); e != nil {
				common.SysLog("orcabox settlement recovery pending")
			}
		}
	}()
	return nil
}
func audit(l *Ledger, external, action string) error {
	return l.DB.Create(&Audit{ExternalID: external, Action: action, CreatedAt: l.now()}).Error
}
func internal(c *gin.Context, l *Ledger, p PriceBook) {
	c.Header("Cache-Control", "no-store")
	expected := "Bearer " + os.Getenv("ORCABOX_INTERNAL_TOKEN")
	if subtle.ConstantTimeCompare([]byte(expected), []byte(c.GetHeader("Authorization"))) != 1 {
		errorResponse(c, 401, "invalid service credential")
		return
	}
	var b struct {
		ExternalID      string `json:"external_id"`
		OrderNo         string `json:"order_no"`
		Credits         int64  `json:"credits"`
		Days            int    `json:"days"`
		Disabled        bool   `json:"disabled"`
		Page            int    `json:"page"`
		Model           string `json:"model"`
		ReviewReference string `json:"review_reference"`
	}
	if c.Request.Method != "POST" {
		errorResponse(c, 405, "POST required")
		return
	}
	d := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&b) != nil {
		errorResponse(c, 400, "invalid request")
		return
	}
	action := strings.TrimPrefix(c.Request.URL.Path, "/orcabox/internal/")
	var result any
	var e error
	switch action {
	case "health":
		if !resourcesReady() || common.RDB.Ping(c.Request.Context()).Err() != nil || l.DB.Exec("SELECT 1").Error != nil {
			e = errors.New("unavailable")
		} else {
			for model := range p.Models {
				if !l.modelReady(model, p.Version) {
					e = errors.New("model price gate unavailable")
					break
				}
			}
			result = gin.H{"ready": true, "price_version": p.Version}
		}
	case "grant":
		result, e = l.Grant(b.OrderNo, b.ExternalID, b.Credits, b.Days)
	case "account":
		result, e = l.Account(b.ExternalID)
	case "prices":
		result = p
	case "reopen-model":
		e = l.reopenModel(p, b.Model, b.ReviewReference)
		result = gin.H{"ok": e == nil}
	case "key":
		var key string
		if e = audit(l, b.ExternalID, "key.reveal"); e == nil {
			key, e = l.Key(b.ExternalID)
		}
		result = gin.H{"key": key}
	case "rotate":
		if e = audit(l, b.ExternalID, "key.rotate"); e == nil {
			e = l.Rotate(b.ExternalID)
		}
		result = gin.H{"ok": e == nil}
	case "disable":
		if e = audit(l, b.ExternalID, "key.disable"); e == nil {
			e = l.Disable(b.ExternalID, b.Disabled)
		}
		result = gin.H{"ok": e == nil}
	case "revoke":
		if e = audit(l, b.ExternalID, "entitlement.revoke"); e == nil {
			e = l.Revoke(b.OrderNo, b.ExternalID)
		}
		result = gin.H{"ok": e == nil}
	case "requests":
		var a Account
		a, e = l.Account(b.ExternalID)
		if e == nil {
			rows := []Request{}
			page := b.Page
			if page < 1 {
				page = 1
			}
			if page > 10000 {
				page = 10000
			}
			e = l.DB.Where("account_id = ?", a.ID).Order("created_at DESC, id DESC").Offset((page - 1) * 25).Limit(25).Find(&rows).Error
			result = rows
		}
	default:
		errorResponse(c, 404, "unknown operation")
		return
	}
	if e != nil {
		if errors.Is(e, gorm.ErrRecordNotFound) {
			errorResponse(c, 404, "not found")
		} else if errors.Is(e, ErrConflict) {
			errorResponse(c, 409, "conflicting order facts")
		} else {
			errorResponse(c, 503, "operation unavailable")
		}
		return
	}
	c.AbortWithStatusJSON(200, result)
}
func authenticate(c *gin.Context, l *Ledger) (Account, error) {
	var a Account
	header := c.GetHeader("Authorization")
	if !strings.HasPrefix(header, "Bearer sk-") {
		return a, errors.New("invalid key")
	}
	key := strings.TrimPrefix(header, "Bearer sk-")
	if len(key) != 64 {
		return a, errors.New("invalid key")
	}
	// Fetch key ownership and its generation from the same database snapshot.
	e := l.DB.WithContext(c.Request.Context()).Table("orcabox_accounts AS a").Select("a.*").
		Joins("JOIN tokens AS t ON t.id = a.token_id AND t.user_id = a.user_id").
		Where("t.key = ? AND t.status = ? AND t.deleted_at IS NULL", key, common.TokenStatusEnabled).Take(&a).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return a, ErrKeyChanged
	}
	if e != nil {
		return a, ErrAuthUnavailable
	}
	if a.Disabled || a.AdminFrozen {
		return a, ErrKeyChanged
	}
	return a, nil
}
func gateway(c *gin.Context, l *Ledger, p PriceBook) {
	c.Header("Cache-Control", "no-store")
	a, e := authenticate(c, l)
	if e != nil {
		if errors.Is(e, ErrAuthUnavailable) {
			errorResponse(c, 503, "身份验证服务暂不可用，请稍后重试")
			return
		}
		errorResponse(c, 401, "Key 无效、已停用或需要更换为本站 Key")
		return
	}
	path := c.Request.URL.Path
	if path == "/v1/usage" && c.Request.Method == "GET" {
		l.expire(&a, l.now())
		c.AbortWithStatusJSON(200, gin.H{"mode": "quota_limited", "unit": "积分", "quota": gin.H{"scope": "key", "limit": float64(a.Available+a.Frozen+a.Used+a.Expired) / float64(Unit), "remaining": float64(a.Available) / float64(Unit)}, "available_units": a.Available, "frozen_units": a.Frozen, "used_units": a.Used, "unit_scale": Unit, "expires_at": a.ExpiresAt})
		return
	}
	if !a.ExpiresAt.After(l.now()) {
		errorResponse(c, 403, "套餐已到期")
		return
	}
	if path == "/v1/models" && c.Request.Method == "GET" {
		list := []gin.H{}
		for _, id := range []string{"gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6-astra"} {
			if _, ok := p.Models[id]; ok && l.modelReady(id, p.Version) {
				list = append(list, gin.H{"id": id, "object": "model", "owned_by": "orcabox"})
			}
		}
		c.AbortWithStatusJSON(200, gin.H{"object": "list", "data": list})
		return
	}
	if c.Request.Method != "POST" || (path != "/v1/responses" && path != "/v1/chat/completions") {
		errorResponse(c, 404, "接口未开放")
		return
	}
	if !resourcesReady() {
		c.Header("X-Orcabox-Failure-Class", "system_busy")
		errorResponse(c, 503, "服务器繁忙，请稍后重试")
		return
	}
	// Acquire account limits before reading or decoding a potentially large
	// body. A leaked key must not bypass concurrency by flooding JSON uploads.
	id := uuid.NewString()
	rk := fmt.Sprintf("orcabox:rate:%d", a.ID)
	ck := fmt.Sprintf("orcabox:active:%d", a.ID)
	leaseCtx, leaseCancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	limit, e := common.RDB.Eval(leaseCtx, limitScript, []string{rk, ck}, time.Now().UnixMilli(), id).Int()
	leaseCancel()
	if e != nil {
		errorResponse(c, 503, "限流服务暂不可用")
		return
	}
	if limit != 1 {
		errorResponse(c, 429, "请求过于频繁或并发已满")
		return
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = common.RDB.ZRem(releaseCtx, ck, id).Err()
	}()
	if c.Request.ContentLength < 1 || c.Request.ContentLength > 1<<20 {
		select {
		case largePayloadSlots <- struct{}{}:
			defer func() { <-largePayloadSlots }()
		default:
			errorResponse(c, 429, "图片与大文件请求繁忙，请稍后重试")
			return
		}
	}
	raw, e := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 56<<20))
	if e != nil {
		errorResponse(c, 413, "请求过大")
		return
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		errorResponse(c, 400, "invalid JSON")
		return
	}
	modelName, _ := body["model"].(string)
	rate, ok := p.Models[modelName]
	if !ok {
		errorResponse(c, 403, "模型未开放")
		return
	}
	if !l.modelReady(modelName, p.Version) {
		errorResponse(c, 503, "模型费率正在核验，请稍后重试")
		return
	}
	tokens, maxOut, e := validateRequest(body, path, func(item map[string]any) (int64, error) {
		return l.opaqueCeiling(a.ID, item)
	})
	if e != nil {
		if errors.Is(e, errImageDecodeBusy) {
			errorResponse(c, 429, e.Error())
			return
		}
		errorResponse(c, 400, e.Error())
		return
	}
	// Supplier-side system/context tokens are billed even for a tiny prompt.
	// This reserve is conservative and remains subject to the overrun breaker;
	// it is not a supplier guarantee about future hidden-context size.
	tokens += p.InputOverhead
	if tokens > 128000 {
		errorResponse(c, 400, "内容超过当前输入上限，请缩小图片或文件")
		return
	}
	if path == "/v1/chat/completions" {
		if stream, _ := body["stream"].(bool); stream {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	raw, _ = json.Marshal(body)
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	c.Request.ContentLength = int64(len(raw))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 180*time.Second)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	limit, e = common.RDB.Eval(ctx, tokenLimitScript, []string{rk}, time.Now().UnixMilli(), id, tokens+maxOut).Int()
	if e != nil {
		errorResponse(c, 503, "限流服务暂不可用")
		return
	}
	if limit != 1 {
		errorResponse(c, 429, "请求过于频繁或并发已满")
		return
	}
	// Reserve at the most expensive input category; cached input can only cost less.
	reserveRate := rate
	for _, v := range []string{rate.CacheWrite, rate.CacheRead} {
		if priceGreater(v, reserveRate.Input) {
			reserveRate.Input = v
		}
	}
	units, e := reserveRate.Cost(Usage{Input: tokens, Output: maxOut})
	if e != nil {
		errorResponse(c, 503, "费率不可用")
		return
	}
	req, e := l.ReserveAuthenticated(a.ExternalID, modelName, p.Version, units, a.KeyGeneration, rate)
	if e != nil {
		if errors.Is(e, ErrKeyChanged) {
			errorResponse(c, 401, "Key 已更换，请使用新 Key")
		} else if errors.Is(e, ErrTestBudget) {
			errorResponse(c, 403, "真实联调预算已用完，请联系管理员")
		} else if errors.Is(e, ErrFunds) {
			errorResponse(c, 403, "积分不足或套餐已到期")
		} else if errors.Is(e, ErrConcurrency) {
			errorResponse(c, 429, "并发已满，未完成请求正在恢复")
		} else {
			errorResponse(c, 503, "账务服务暂不可用")
		}
		return
	}
	c.Header("X-Orcabox-Request-Id", req.ID)
	c.Set(Managed, true)
	capture := &usageCapture{}
	capture.onUsage = func(u *Usage) {
		if e := l.DurableSettle(req.ID, *u, rate, capture.metadata()); e != nil {
			common.SysLog("orcabox durable settlement pending")
		}
	}
	c.Set("orcabox_capture", capture)
	original := c.Writer
	c.Writer = &disconnectWriter{ResponseWriter: original, ctx: ctx}
	defer func() {
		c.Writer = original
		if e := l.Settle(req.ID, capture.usage, rate, !c.GetBool("orcabox_forwarded"), capture.metadata()); e != nil {
			common.SysLog("orcabox settlement pending")
		}
	}()
	c.Next()
}
func priceGreater(a, b string) bool {
	x, _ := strconv.ParseFloat(a, 64)
	y, _ := strconv.ParseFloat(b, 64)
	return x > y
}

// Count text by UTF-8 bytes as an upper bound and reserve conservative image patches.
// Remote URLs and opaque history are refused because they hide unbounded input.
func validateRequest(b map[string]any, path string, opaqueResolvers ...func(map[string]any) (int64, error)) (int64, int64, error) {
	if n, ok := b["n"]; ok && n != float64(1) {
		return 0, 0, errors.New("每次请求仅支持生成一个回答")
	}
	if v, ok := b["modalities"]; ok {
		modes, valid := v.([]any)
		if !valid || len(modes) != 1 || modes[0] != "text" {
			return 0, 0, errors.New("仅支持文字输出")
		}
	}
	for _, k := range []string{"previous_response_id", "conversation", "background", "input_audio", "audio", "web_search_options"} {
		if v, ok := b[k]; ok && v != nil && v != false {
			return 0, 0, fmt.Errorf("不支持 %s", k)
		}
	}
	if t, ok := b["service_tier"]; ok && t != "auto" && t != "default" {
		return 0, 0, errors.New("快速模式未开放")
	}
	b["store"] = false
	if tools, ok := b["tools"].([]any); ok {
		for _, v := range tools {
			m, ok := v.(map[string]any)
			if !ok || m["type"] != "function" {
				return 0, 0, errors.New("仅支持本地 function 工具")
			}
		}
	}
	field := "max_output_tokens"
	if path == "/v1/chat/completions" {
		field = "max_completion_tokens"
	}
	maximum := int64(8192)
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if v, ok := b[k]; ok {
			x, ok := v.(float64)
			if !ok || x < 1 || x > 8192 || x != float64(int64(x)) {
				return 0, 0, errors.New("输出上限为 8192 Token")
			}
			if int64(x) < maximum {
				maximum = int64(x)
			}
		}
	}
	// Send exactly one output limit. Leaving a higher alias in the payload
	// could let the supplier generate more than the amount we reserved.
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		delete(b, k)
	}
	b[field] = maximum
	images := int64(0)
	opaqueTokens := int64(0)
	textBytes := int64(512)
	var walk func(any) error
	walk = func(v any) error {
		switch x := v.(type) {
		case map[string]any:
			textBytes += 8
			if x["type"] == "input_file" || x["type"] == "file" || x["file_id"] != nil {
				return errors.New("文件需先在客户端提取内容")
			}
			if x["type"] == "item_reference" || (x["id"] != nil && len(x) == 1) {
				return errors.New("不支持远程内容引用")
			}
			if x["type"] == "input_audio" || x["type"] == "audio" || x["type"] == "input_video" || x["type"] == "video_url" {
				return errors.New("当前不支持音频或视频")
			}
			if x["encrypted_content"] != nil {
				if x["type"] != "reasoning" || len(opaqueResolvers) != 1 {
					return errors.New("思考记录无法核验，请新建会话")
				}
				ceiling, e := opaqueResolvers[0](x)
				if e != nil {
					return e
				}
				if ceiling < 1 || ceiling > 8192 {
					return errors.New("思考记录超过限制")
				}
				opaqueTokens += ceiling
			}
			for k, v := range x {
				textBytes += int64(len(k)) + 8
				if k == "image_url" && (x["type"] == "input_image" || x["type"] == "image_url") {
					// Verified model/detail sizing: <=2500 patches * 1.2.
					// https://developers.openai.com/api/docs/guides/images-vision
					if b["model"] != "gpt-5.6-sol" && b["model"] != "gpt-6-astra" {
						return errors.New("图片请选用 5.6 Sol 或 6 Astra，其他模型的图片成本仍在核验")
					}
					url, ok := v.(string)
					if obj, yes := v.(map[string]any); yes {
						url, ok = obj["url"].(string)
						obj["detail"] = "high"
					} else {
						x["detail"] = "high"
					}
					if !ok {
						return errors.New("图片格式无效")
					}
					if e := validateImage(url); e != nil {
						return e
					}
					images++
				} else if e := walk(v); e != nil {
					return e
				}
			}
		case []any:
			textBytes += 8
			for _, v := range x {
				if e := walk(v); e != nil {
					return e
				}
			}
		case string:
			textBytes += int64(len(x)) + 8
		default:
			textBytes += 32
		}
		if textBytes+opaqueTokens > 128000 {
			return errors.New("内容超过当前输入上限，请拆分文件或新建会话")
		}
		return nil
	}
	if e := walk(b); e != nil {
		return 0, 0, e
	}
	if images > 4 {
		return 0, 0, errors.New("每次最多 4 张图片")
	}
	tokens := textBytes + images*3000 + opaqueTokens
	if tokens > 128000 {
		return 0, 0, errors.New("内容超过当前输入上限，请缩小图片或文件")
	}
	return tokens, maximum, nil
}
func validateImage(s string) error {
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return errors.New("只支持直接上传图片")
	}
	switch parts[0] {
	case "data:image/jpeg;base64", "data:image/png;base64", "data:image/webp;base64":
	default:
		return errors.New("图片仅支持 JPEG、PNG、WebP")
	}
	if len(parts[1]) > 14<<20 {
		return errors.New("每张图片最多 10 MB")
	}
	decodedBytes, e := io.Copy(io.Discard, io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(parts[1])), (10<<20)+1))
	if e != nil || decodedBytes > 10<<20 {
		return errors.New("图片无效或过大")
	}
	config, format, e := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(parts[1])))
	if e != nil || config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > 16000000 {
		return errors.New("图片尺寸超过限制")
	}
	expected := strings.TrimPrefix(strings.TrimSuffix(parts[0], ";base64"), "data:image/")
	if format != expected {
		return errors.New("图片类型不匹配")
	}
	// Compressed tiny files can still decode into 64 MB pixel buffers. Bound
	// decoding across accounts as well as bounding the uploaded byte count.
	select {
	case imageDecodeSlots <- struct{}{}:
		defer func() { <-imageDecodeSlots }()
	default:
		return errImageDecodeBusy
	}
	decoded, _, e := image.Decode(base64.NewDecoder(base64.StdEncoding, strings.NewReader(parts[1])))
	if e != nil || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return errors.New("图片无法完整解码")
	}
	return nil
}

type disconnectWriter struct {
	gin.ResponseWriter
	ctx context.Context
}

func (w *disconnectWriter) Write(b []byte) (int, error) {
	if w.ResponseWriter.Status() >= 400 {
		b = []byte(`{"error":{"type":"gateway_error","message":"调用未完成，请稍后重试或联系支持"}}`)
	}
	_, e := w.ResponseWriter.Write(b)
	if e != nil && w.ctx.Err() != nil {
		return 0, w.ctx.Err()
	}
	return len(b), nil
}
func (w *disconnectWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *disconnectWriter) CloseNotify() <-chan bool {
	ch := make(chan bool)
	go func() { <-w.ctx.Done(); close(ch) }()
	return ch
}
