package relay

import (
	"context"
	"strings"
	"time"

	"github.com/bluelightgit/octopus/internal/body"
	"github.com/bluelightgit/octopus/internal/conf"
	"github.com/bluelightgit/octopus/internal/model"
	"github.com/bluelightgit/octopus/internal/op"
	"github.com/bluelightgit/octopus/internal/price"
	transformerModel "github.com/bluelightgit/octopus/internal/transformer/model"
	"github.com/bluelightgit/octopus/internal/utils/log"
)

const (
	defaultMaxLoggedClientRequestBytes  = 512 * 1024
	defaultMaxLoggedClientResponseBytes = 1024 * 1024
	maxLoggedUpstreamEventTypes         = 16
	maxLoggedExecutionTraceEntries      = 48
)

var (
	// These legacy limits are consulted only when relay_body_storage.enabled is
	// false. A value of zero means unlimited in that compatibility mode.
	maxLoggedClientRequestBytes  = defaultMaxLoggedClientRequestBytes
	maxLoggedClientResponseBytes = defaultMaxLoggedClientResponseBytes
)

// RelayMetrics 负责最终的日志收集与持久化
type RelayMetrics struct {
	APIKeyID     int
	RequestModel string
	StartTime    time.Time

	// 首 Token 时间
	FirstTokenTime time.Time
	// 流阶段观测点
	UpstreamFirstEventTime time.Time
	ClientFirstWriteTime   time.Time
	UpstreamEventCount     int
	UpstreamEventTypes     []string
	ClientChunkCount       int
	TerminalSeen           bool
	FailureStage           string
	ExecutionTrace         []string

	// 请求和响应内容。capture 只保留受限前缀；超过阈值的完整原文写入
	// body storage，避免把大 body 再复制进日志字符串和 SQLite。
	InternalRequest  *transformerModel.InternalLLMRequest
	InternalResponse *transformerModel.InternalLLMResponse

	requestBodyCapture  *body.Capture
	responseBodyCapture *body.Capture

	// 统计指标
	ActualModel string
	Stats       model.StatsMetrics
}

func NewRelayMetrics(apiKeyID int, requestModel string, req *transformerModel.InternalLLMRequest) *RelayMetrics {
	m := &RelayMetrics{
		APIKeyID:           apiKeyID,
		RequestModel:       requestModel,
		StartTime:          time.Now(),
		InternalRequest:    req,
		UpstreamEventTypes: make([]string, 0, 8),
		ExecutionTrace:     make([]string, 0, 16),
	}
	if req != nil && len(req.RawRequest) > 0 {
		m.SetClientRequestBody(req.RawRequest)
	}
	return m
}

func (m *RelayMetrics) SetFirstTokenTime(t time.Time) {
	if m == nil || !m.FirstTokenTime.IsZero() {
		return
	}
	m.FirstTokenTime = t
}

func (m *RelayMetrics) RecordUpstreamEvent(t time.Time) {
	if m == nil {
		return
	}
	m.UpstreamEventCount++
	if m.UpstreamFirstEventTime.IsZero() {
		m.UpstreamFirstEventTime = t
	}
}

func (m *RelayMetrics) RecordUpstreamEventType(eventType string) {
	if m == nil {
		return
	}
	eventType = normalizeUpstreamEventType(eventType)
	if eventType == "" {
		return
	}
	if len(m.UpstreamEventTypes) >= maxLoggedUpstreamEventTypes {
		copy(m.UpstreamEventTypes, m.UpstreamEventTypes[1:])
		m.UpstreamEventTypes[len(m.UpstreamEventTypes)-1] = eventType
		return
	}
	m.UpstreamEventTypes = append(m.UpstreamEventTypes, eventType)
}

func normalizeUpstreamEventType(eventType string) string {
	return strings.TrimSpace(eventType)
}

func (m *RelayMetrics) RecordExecutionTrace(entry string) {
	if m == nil {
		return
	}
	entry = normalizeExecutionTraceEntry(entry)
	if entry == "" {
		return
	}
	if len(m.ExecutionTrace) >= maxLoggedExecutionTraceEntries {
		copy(m.ExecutionTrace, m.ExecutionTrace[1:])
		m.ExecutionTrace[len(m.ExecutionTrace)-1] = entry
		return
	}
	m.ExecutionTrace = append(m.ExecutionTrace, entry)
}

func normalizeExecutionTraceEntry(entry string) string {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return ""
	}
	entry = strings.Join(strings.Fields(entry), " ")
	const maxTraceRunes = 240
	runes := []rune(entry)
	if len(runes) > maxTraceRunes {
		return string(runes[:maxTraceRunes-3]) + "..."
	}
	return entry
}

func (m *RelayMetrics) RecordClientChunk(t time.Time, chunk []byte) {
	if m == nil || len(chunk) == 0 {
		return
	}
	m.ClientChunkCount++
	if m.ClientFirstWriteTime.IsZero() {
		m.ClientFirstWriteTime = t
	}
	m.AppendClientResponseChunk(chunk)
}

func (m *RelayMetrics) MarkTerminalSeen() {
	if m == nil {
		return
	}
	m.TerminalSeen = true
}

func (m *RelayMetrics) SetFailureStage(stage string) {
	if m == nil {
		return
	}
	m.FailureStage = stage
}

func (m *RelayMetrics) SetClientRequestBody(body []byte) {
	if m == nil {
		return
	}
	if m.requestBodyCapture != nil {
		m.requestBodyCapture.Discard()
	}
	capture := newRelayBodyCapture(maxLoggedClientRequestBytes)
	_ = capture.Write(body)
	m.requestBodyCapture = capture
}

func (m *RelayMetrics) SetClientResponseBody(body []byte) {
	if m == nil {
		return
	}
	if m.responseBodyCapture != nil {
		m.responseBodyCapture.Discard()
	}
	capture := newRelayBodyCapture(maxLoggedClientResponseBytes)
	_ = capture.Write(body)
	m.responseBodyCapture = capture
}

func (m *RelayMetrics) AppendClientResponseChunk(chunk []byte) {
	if m == nil {
		return
	}
	if len(chunk) == 0 {
		return
	}
	if m.responseBodyCapture == nil {
		m.responseBodyCapture = newRelayBodyCapture(maxLoggedClientResponseBytes)
	}
	_ = m.responseBodyCapture.Write(chunk)
}

func newRelayBodyCapture(legacyInlineMax int) *body.Capture {
	config := conf.AppConfig.RelayBodyStorage.WithDefaults()
	inlineMax := config.InlineMaxBytes
	previewMax := config.PreviewMaxBytes
	if !config.Enabled {
		// Preserve the old environment-variable behavior when the new external
		// storage feature is explicitly disabled.
		inlineMax = int64(legacyInlineMax)
		previewMax = inlineMax
	}
	return body.NewCapture(body.Config{
		Enabled:         config.Enabled,
		Directory:       config.Directory,
		InlineMaxBytes:  inlineMax,
		PreviewMaxBytes: previewMax,
		Compression:     config.Compression,
	})
}

func finishRelayBodyCapture(capture *body.Capture, kind string) body.Artifact {
	if capture == nil {
		return body.Artifact{}
	}
	artifact, err := capture.Finish()
	if err != nil {
		log.Warnf("failed to store relay %s body externally: %v", kind, err)
	}
	return artifact
}

func (m *RelayMetrics) SetInternalResponse(resp *transformerModel.InternalLLMResponse, actualModel string) {
	m.InternalResponse = resp
	m.ActualModel = actualModel

	if resp == nil || resp.Usage == nil {
		return
	}

	usage := resp.Usage
	m.Stats.InputToken = usage.PromptTokens
	m.Stats.OutputToken = usage.CompletionTokens

	modelPrice := price.GetLLMPrice(actualModel)
	if modelPrice == nil {
		return
	}
	if usage.PromptTokensDetails == nil {
		usage.PromptTokensDetails = &transformerModel.PromptTokensDetails{CachedTokens: 0}
	}
	if usage.AnthropicUsage {
		m.Stats.InputCost = (float64(usage.PromptTokensDetails.CachedTokens)*modelPrice.CacheRead +
			float64(usage.PromptTokens)*modelPrice.Input +
			float64(usage.CacheCreationInputTokens)*modelPrice.CacheWrite) * 1e-6
	} else {
		m.Stats.InputCost = (float64(usage.PromptTokensDetails.CachedTokens)*modelPrice.CacheRead + float64(usage.PromptTokens-usage.PromptTokensDetails.CachedTokens)*modelPrice.Input) * 1e-6
	}
	m.Stats.OutputCost = float64(usage.CompletionTokens) * modelPrice.Output * 1e-6
}

func (m *RelayMetrics) Save(ctx context.Context, success bool, err error, attempts []model.ChannelAttempt) {
	duration := time.Since(m.StartTime)

	globalStats := model.StatsMetrics{
		WaitTime:    duration.Milliseconds(),
		InputToken:  m.Stats.InputToken,
		OutputToken: m.Stats.OutputToken,
		InputCost:   m.Stats.InputCost,
		OutputCost:  m.Stats.OutputCost,
	}
	if success {
		globalStats.RequestSuccess = 1
	} else {
		globalStats.RequestFailed = 1
	}

	channelID, channelName := finalChannel(attempts)
	op.StatsTotalUpdate(globalStats)
	op.StatsHourlyUpdate(globalStats)
	op.StatsDailyUpdate(context.Background(), globalStats)
	op.StatsAPIKeyUpdate(m.APIKeyID, globalStats)
	op.StatsChannelUpdate(channelID, globalStats)

	log.Infof("relay complete: model=%s, channel=%d(%s), success=%t, duration=%dms, input_token=%d, output_token=%d, input_cost=%f, output_cost=%f, total_cost=%f, attempts=%d",
		m.RequestModel, channelID, channelName, success, duration.Milliseconds(),
		m.Stats.InputToken, m.Stats.OutputToken,
		m.Stats.InputCost, m.Stats.OutputCost, m.Stats.InputCost+m.Stats.OutputCost,
		len(attempts))

	m.saveLog(ctx, err, duration, attempts, channelID, channelName)
}

func finalChannel(attempts []model.ChannelAttempt) (int, string) {
	var lastID int
	var lastName string
	for i := len(attempts) - 1; i >= 0; i-- {
		a := attempts[i]
		if a.Status == model.AttemptSuccess {
			return a.ChannelID, a.ChannelName
		}
		if a.Status == model.AttemptFailed && lastID == 0 {
			lastID = a.ChannelID
			lastName = a.ChannelName
		}
	}
	return lastID, lastName
}

func (m *RelayMetrics) saveLog(ctx context.Context, err error, duration time.Duration, attempts []model.ChannelAttempt, channelID int, channelName string) {
	actualModel := m.ActualModel
	if actualModel == "" {
		actualModel = m.RequestModel
	}
	isStreamRequest := m.InternalRequest != nil && m.InternalRequest.Stream != nil && *m.InternalRequest.Stream

	relayLog := model.RelayLog{
		Time:             m.StartTime.Unix(),
		RequestModelName: m.RequestModel,
		ChannelName:      channelName,
		ChannelId:        channelID,
		ActualModelName:  actualModel,
		UseTime:          int(duration.Milliseconds()),
		Attempts:         attempts,
		TotalAttempts:    len(attempts),
	}

	if apiKey, getErr := op.APIKeyGet(m.APIKeyID, ctx); getErr == nil {
		relayLog.RequestAPIKeyName = apiKey.Name
	}

	// 首字时间
	if !m.FirstTokenTime.IsZero() {
		relayLog.Ftut = int(m.FirstTokenTime.Sub(m.StartTime).Milliseconds())
	}
	if isStreamRequest {
		if !m.UpstreamFirstEventTime.IsZero() {
			upstreamFirstEventMs := int(m.UpstreamFirstEventTime.Sub(m.StartTime).Milliseconds())
			relayLog.UpstreamFirstEventMs = &upstreamFirstEventMs
		}
		if !m.ClientFirstWriteTime.IsZero() {
			clientFirstWriteMs := int(m.ClientFirstWriteTime.Sub(m.StartTime).Milliseconds())
			relayLog.ClientFirstWriteMs = &clientFirstWriteMs
		}
		upstreamEventCount := m.UpstreamEventCount
		clientChunkCount := m.ClientChunkCount
		terminalSeen := m.TerminalSeen
		relayLog.UpstreamEventCount = &upstreamEventCount
		if len(m.UpstreamEventTypes) > 0 {
			relayLog.UpstreamEventTypes = append([]string(nil), m.UpstreamEventTypes...)
		}
		relayLog.ClientChunkCount = &clientChunkCount
		relayLog.TerminalSeen = &terminalSeen
		if err != nil && m.FailureStage != "" {
			failureStage := m.FailureStage
			relayLog.FailureStage = &failureStage
		}
	}
	if len(m.ExecutionTrace) > 0 {
		relayLog.ExecutionTrace = append([]string(nil), m.ExecutionTrace...)
	}

	// Usage
	if m.InternalResponse != nil && m.InternalResponse.Usage != nil {
		relayLog.InputTokens = int(m.InternalResponse.Usage.PromptTokens)
		relayLog.OutputTokens = int(m.InternalResponse.Usage.CompletionTokens)
		relayLog.Cost = m.Stats.InputCost + m.Stats.OutputCost
	}

	// 请求内容（优先 client 原始内容）。大 body 的完整内容由 capture
	// 外置，SQLite 中只保留精确前缀和可下载引用。
	requestArtifact := finishRelayBodyCapture(m.requestBodyCapture, "request")
	if requestArtifact.Size > 0 {
		relayLog.RequestContent, relayLog.RequestBodyEncoding = body.EncodeInline(requestArtifact.Inline)
		relayLog.RequestContentTruncated = requestArtifact.Truncated
		relayLog.RequestBodyRef = requestArtifact.Ref
		relayLog.RequestBodySize = requestArtifact.Size
		relayLog.RequestBodySHA256 = requestArtifact.SHA256
		if requestArtifact.StorageError != "" {
			relayLog.RequestBodyStorageError = requestArtifact.StorageError
		}
	}

	// 响应内容（优先 client 原始内容）。这里保存的是最终客户端可见的
	// 原始响应，不保存协议转换过程中的中间副本。
	responseArtifact := finishRelayBodyCapture(m.responseBodyCapture, "response")
	if responseArtifact.Size > 0 {
		relayLog.ResponseContent, relayLog.ResponseBodyEncoding = body.EncodeInline(responseArtifact.Inline)
		relayLog.ResponseContentTruncated = responseArtifact.Truncated
		relayLog.ResponseBodyRef = responseArtifact.Ref
		relayLog.ResponseBodySize = responseArtifact.Size
		relayLog.ResponseBodySHA256 = responseArtifact.SHA256
		if responseArtifact.StorageError != "" {
			relayLog.ResponseBodyStorageError = responseArtifact.StorageError
		}
	}

	// 错误信息
	if err != nil {
		relayLog.Error = err.Error()
	}

	if logErr := op.RelayLogAdd(ctx, relayLog); logErr != nil {
		log.Warnf("failed to save relay log: %v", logErr)
	}
}
