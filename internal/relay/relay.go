package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/gin-gonic/gin"
	"github.com/tmaxmax/go-sse"
)

// Handler 处理入站请求并转发到上游服务
func Handler(inboundType inbound.InboundType, c *gin.Context) {
	internalRequest, inAdapter, err := parseRequest(inboundType, c)
	if err != nil {
		return
	}

	supportedModels := c.GetString("supported_models")
	if supportedModels != "" {
		supportedModelsArray := strings.Split(supportedModels, ",")
		if !slices.Contains(supportedModelsArray, internalRequest.Model) {
			resp.Error(c, http.StatusBadRequest, "model not supported")
			return
		}
	}

	requestModel := internalRequest.Model
	apiKeyID := c.GetInt("api_key_id")

	// 获取通道分组
	group, err := op.GroupGetEnabledMap(requestModel, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusNotFound, "model not found")
		return
	}

	rawOnlyRequiredType, rawOnlyHasRequired := rawOnlyRequiredOutboundType(internalRequest)
	rawOnlySawCompatible := false
	if internalRequest.RawOnly && !rawOnlyHasRequired {
		resp.Error(c, http.StatusBadRequest, "raw-only request format not supported")
		return
	}

	protocolPlan, err := buildProtocolRoutePlan(group, internalRequest)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	log.Infof("protocol routing for model %s: %s", requestModel, protocolRoutePlanSummary(protocolPlan))

	routeAffinityMode := group.GetRouteAffinityMode()
	responsesStateful := buildResponsesStatefulRequestContext(group, apiKeyID, requestModel, internalRequest)
	conversationAffinity := buildConversationAffinityRequestContext(group, apiKeyID, requestModel, internalRequest)

	iterOpts := balancer.IteratorOptions{}
	if routeAffinityMode != dbmodel.GroupRouteAffinityModeOff || responsesStateful != nil || conversationAffinity != nil {
		iterOpts.DisableSessionSticky = true
	}
	if responsesStateful != nil && responsesStateful.HasPinnedRoute() {
		iterOpts.PreferChannelID = responsesStateful.PinnedRoute.ChannelID
	} else if conversationAffinity != nil && conversationAffinity.HasPreferredRoute() {
		iterOpts.PreferChannelID = conversationAffinity.PreferredRoute.ChannelID
	}

	primaryGroup := cloneGroupWithItems(group, protocolPlan.Primary)
	primaryIter := balancer.NewIteratorWithOptions(primaryGroup, apiKeyID, requestModel, iterOpts)
	var fallbackIter *balancer.Iterator
	if len(protocolPlan.Fallback) > 0 {
		fallbackGroup := cloneGroupWithItems(group, protocolPlan.Fallback)
		fallbackIter = balancer.NewIteratorWithOptions(fallbackGroup, apiKeyID, requestModel, iterOpts)
	}

	activeIter := primaryIter
	if activeIter.Len() == 0 {
		if fallbackIter == nil || fallbackIter.Len() == 0 {
			resp.Error(c, http.StatusServiceUnavailable, "no available channel")
			return
		}
		activeIter = fallbackIter
		fallbackIter = nil
	}

	metrics := NewRelayMetrics(apiKeyID, requestModel, internalRequest)
	metrics.RecordExecutionTrace(fmt.Sprintf("request: raw_api=%s stream=%t raw_only=%t protocol_plan=%s", internalRequest.RawAPIFormat, internalRequest.Stream != nil && *internalRequest.Stream, internalRequest.RawOnly, protocolRoutePlanSummary(protocolPlan)))
	if responsesStateful != nil {
		metrics.RecordExecutionTrace(fmt.Sprintf("responses_stateful: mode=%s continuation=%t pinned=%t reason=%s", responsesStateful.Mode, responsesStateful.HasContinuationState, responsesStateful.HasPinnedRoute(), responsesStateful.ProtectionReason()))
	}
	if conversationAffinity != nil {
		metrics.RecordExecutionTrace(fmt.Sprintf("conversation_affinity: mode=%s keys=%d preferred=%t selected=%s", conversationAffinity.Mode, len(conversationAffinity.LookupKeys), conversationAffinity.HasPreferredRoute(), conversationAffinity.SelectedLookupKey))
	}
	req := &relayRequest{
		c:                    c,
		inAdapter:            inAdapter,
		internalRequest:      internalRequest,
		metrics:              metrics,
		apiKeyID:             apiKeyID,
		groupID:              group.ID,
		requestModel:         requestModel,
		routeAffinityMode:    routeAffinityMode,
		iter:                 activeIter,
		responsesStateful:    responsesStateful,
		conversationAffinity: conversationAffinity,
	}

	var lastErr error
	var firstCompatibilityErr error
	attemptStarted := false
	stopRetries := false

	for {
		matchedPinnedRoute := false
		for activeIter.Next() {
			select {
			case <-c.Request.Context().Done():
				log.Infof("request context canceled, stopping retry")
				metrics.Save(c.Request.Context(), false, context.Canceled, mergeAttempts(primaryIter, fallbackIter, activeIter))
				return
			default:
			}

			item := activeIter.Item()
			if req.responsesStateful != nil && req.responsesStateful.HasPinnedRoute() && item.ChannelID == req.responsesStateful.PinnedRoute.ChannelID {
				matchedPinnedRoute = true
			}
			if req.conversationAffinity != nil && req.conversationAffinity.HasPreferredRoute() && item.ChannelID == req.conversationAffinity.PreferredRoute.ChannelID {
				matchedPinnedRoute = true
			}
			channel, err := op.ChannelGet(item.ChannelID, c.Request.Context())
			if err != nil {
				log.Warnf("failed to get channel %d: %v", item.ChannelID, err)
				activeIter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("channel not found: %v", err))
				lastErr = err
				continue
			}
			if !channel.Enabled {
				activeIter.Skip(channel.ID, 0, channel.Name, "channel disabled")
				continue
			}

			if internalRequest.RawOnly {
				if channel.Type != rawOnlyRequiredType {
					activeIter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("raw-only request requires channel type %d", rawOnlyRequiredType))
					continue
				}
				rawOnlySawCompatible = true
			}

			selectedBaseURL := channel.GetBaseUrl()
			if selectedBaseURL == "" {
				activeIter.Skip(channel.ID, 0, channel.Name, "no available base url")
				continue
			}

			var usedKey dbmodel.ChannelKey
			if req.responsesStateful != nil && req.responsesStateful.HasPinnedRoute() {
				pinnedRoute := req.responsesStateful.PinnedRoute
				if channel.ID != pinnedRoute.ChannelID {
					activeIter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("responses stateful route pinned to channel %d", pinnedRoute.ChannelID))
					continue
				}
				pinnedBaseURL, ok := findPinnedBaseURL(channel, pinnedRoute.BaseURL)
				if !ok {
					err = fmt.Errorf("responses stateful route pinned to base url %s but it is unavailable", pinnedRoute.BaseURL)
					activeIter.Skip(channel.ID, pinnedRoute.ChannelKeyID, channel.Name, err.Error())
					lastErr = err
					if req.responsesStateful.BlocksCrossRouteFailover() {
						stopRetries = true
						break
					}
					continue
				}
				selectedBaseURL = pinnedBaseURL
				pinnedKey, ok := findPinnedChannelKey(channel, pinnedRoute.ChannelKeyID)
				if !ok {
					err = fmt.Errorf("responses stateful route pinned to channel key %d but it is unavailable", pinnedRoute.ChannelKeyID)
					activeIter.Skip(channel.ID, pinnedRoute.ChannelKeyID, channel.Name, err.Error())
					lastErr = err
					if req.responsesStateful.BlocksCrossRouteFailover() {
						stopRetries = true
						break
					}
					continue
				}
				usedKey = pinnedKey
			} else {
				if req.conversationAffinity != nil && req.conversationAffinity.HasPreferredRoute() && channel.ID == req.conversationAffinity.PreferredRoute.ChannelID {
					preferredRoute := req.conversationAffinity.PreferredRoute
					if preferredBaseURL, ok := findPinnedBaseURL(channel, preferredRoute.BaseURL); ok {
						selectedBaseURL = preferredBaseURL
					}
					if preferredKey, ok := findPinnedChannelKey(channel, preferredRoute.ChannelKeyID); ok {
						usedKey = preferredKey
					}
				}
				if usedKey.ChannelKey == "" {
					usedKey = channel.GetChannelKey()
				}
				if usedKey.ChannelKey == "" {
					activeIter.Skip(channel.ID, 0, channel.Name, "no available key")
					continue
				}
			}
			if activeIter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
				if req.responsesStateful != nil && req.responsesStateful.HasPinnedRoute() && req.responsesStateful.BlocksCrossRouteFailover() {
					lastErr = fmt.Errorf("responses stateful route is unavailable because the pinned route is circuit broken")
					stopRetries = true
					break
				}
				if req.conversationAffinity != nil && req.conversationAffinity.HasPreferredRoute() && req.conversationAffinity.BlocksCrossRouteFailover() && channel.ID == req.conversationAffinity.PreferredRoute.ChannelID {
					lastErr = fmt.Errorf("conversation affinity route is unavailable because the pinned route is circuit broken")
					stopRetries = true
					break
				}
				continue
			}

			outAdapter := outbound.Get(channel.Type)
			if outAdapter == nil {
				activeIter.Skip(channel.ID, usedKey.ID, channel.Name, fmt.Sprintf("unsupported channel type: %d", channel.Type))
				continue
			}

			if internalRequest.IsEmbeddingRequest() && !outbound.IsEmbeddingChannelType(channel.Type) {
				activeIter.Skip(channel.ID, usedKey.ID, channel.Name, "channel type not compatible with embedding request")
				continue
			}
			if internalRequest.IsChatRequest() && !outbound.IsChatChannelType(channel.Type) {
				activeIter.Skip(channel.ID, usedKey.ID, channel.Name, "channel type not compatible with chat request")
				continue
			}
			if compatErr := outbound.ValidateRequestCompatibility(channel.Type, internalRequest); compatErr != nil {
				if firstCompatibilityErr == nil {
					firstCompatibilityErr = compatErr
				}
				activeIter.Skip(channel.ID, usedKey.ID, channel.Name, compatErr.Error())
				continue
			}

			internalRequest.Model = item.ModelName
			log.Infof("request model %s, mode: %d, forwarding to channel: %s model: %s base_url: %s (attempt %d/%d, sticky=%t, responses_stateful=%t)",
				requestModel, group.Mode, channel.Name, item.ModelName, normalizeAffinityBaseURL(selectedBaseURL),
				activeIter.Index()+1, activeIter.Len(), activeIter.IsSticky(), req.responsesStateful != nil)
			metrics.RecordExecutionTrace(fmt.Sprintf("attempt_start: channel=%s outbound_type=%d upstream_model=%s base_url=%s key_id=%d sticky=%t responses_stateful=%t", channel.Name, channel.Type, item.ModelName, normalizeAffinityBaseURL(selectedBaseURL), usedKey.ID, activeIter.IsSticky(), req.responsesStateful != nil))

			req.iter = activeIter
			ra := &relayAttempt{
				relayRequest:         req,
				outAdapter:           outAdapter,
				channel:              channel,
				usedKey:              usedKey,
				selectedBaseURL:      selectedBaseURL,
				firstTokenTimeOutSec: group.FirstTokenTimeOut,
			}

			attemptStarted = true
			result := ra.attempt()
			if result.Success {
				metrics.Save(c.Request.Context(), true, nil, mergeAttempts(primaryIter, fallbackIter, activeIter))
				return
			}
			if result.Written {
				metrics.Save(c.Request.Context(), false, result.Err, mergeAttempts(primaryIter, fallbackIter, activeIter))
				return
			}
			lastErr = result.Err
			if result.NonRetryable {
				stopRetries = true
				break
			}
		}
		if !stopRetries && req.responsesStateful != nil && req.responsesStateful.HasPinnedRoute() && req.responsesStateful.BlocksCrossRouteFailover() && !matchedPinnedRoute {
			lastErr = fmt.Errorf("responses stateful route pinned to channel %d but no matching route candidate is available", req.responsesStateful.PinnedRoute.ChannelID)
			stopRetries = true
		}
		if !stopRetries && req.conversationAffinity != nil && req.conversationAffinity.HasPreferredRoute() && req.conversationAffinity.BlocksCrossRouteFailover() && !matchedPinnedRoute {
			lastErr = fmt.Errorf("conversation affinity route pinned to channel %d but no matching route candidate is available", req.conversationAffinity.PreferredRoute.ChannelID)
			stopRetries = true
		}
		if stopRetries {
			break
		}

		if fallbackIter == nil || activeIter == fallbackIter {
			break
		}
		activeIter = fallbackIter
		req.iter = activeIter
		fallbackIter = nil
		metrics.RecordExecutionTrace("route_switch: moving to fallback candidates")
	}

	var ue upstreamHTTPError
	if internalRequest.RawOnly && rawOnlyHasRequired && !rawOnlySawCompatible {
		err := errors.New("no same-protocol channel for raw-only request")
		writeRelayError(c, metrics, http.StatusBadRequest, "no same-protocol channel for raw-only request")
		metrics.RecordExecutionTrace("request_failed: no same-protocol channel for raw-only request")
		metrics.Save(c.Request.Context(), false, err, mergeAttempts(primaryIter, fallbackIter, activeIter))
		return
	}

	if !attemptStarted && firstCompatibilityErr != nil {
		writeRelayError(c, metrics, http.StatusBadRequest, firstCompatibilityErr.Error())
		metrics.RecordExecutionTrace(fmt.Sprintf("request_failed: compatibility_error=%s", firstCompatibilityErr.Error()))
		metrics.Save(c.Request.Context(), false, firstCompatibilityErr, mergeAttempts(primaryIter, fallbackIter, activeIter))
		return
	}

	if stopRetries && lastErr != nil {
		metrics.RecordExecutionTrace(fmt.Sprintf("request_failed: responses_stateful_stop error=%s", lastErr.Error()))
	}

	if lastErr != nil && errors.As(lastErr, &ue) {
		if metrics != nil {
			metrics.SetClientResponseBody(ue.Body)
		}
		metrics.RecordExecutionTrace(fmt.Sprintf("request_failed: upstream_http_error status=%d content_type=%s", ue.StatusCode, strings.TrimSpace(ue.ContentType)))
		metrics.Save(c.Request.Context(), false, lastErr, mergeAttempts(primaryIter, fallbackIter, activeIter))
		ct := strings.TrimSpace(ue.ContentType)
		if ct == "" {
			ct = "application/json"
		}
		code := ue.StatusCode
		if code <= 0 {
			code = http.StatusBadGateway
		}
		c.Data(code, ct, ue.Body)
		return
	}

	writeRelayError(c, metrics, http.StatusBadGateway, "all channels failed")
	if lastErr != nil {
		metrics.RecordExecutionTrace(fmt.Sprintf("request_failed: %s", lastErr.Error()))
	} else {
		metrics.RecordExecutionTrace("request_failed: all channels failed")
	}
	metrics.Save(c.Request.Context(), false, lastErr, mergeAttempts(primaryIter, fallbackIter, activeIter))
}

func mergeAttempts(primaryIter, fallbackIter, activeIter *balancer.Iterator) []dbmodel.ChannelAttempt {
	attempts := make([]dbmodel.ChannelAttempt, 0)
	seen := map[*balancer.Iterator]struct{}{}
	appendAttempts := func(it *balancer.Iterator) {
		if it == nil {
			return
		}
		if _, ok := seen[it]; ok {
			return
		}
		seen[it] = struct{}{}
		attempts = append(attempts, it.Attempts()...)
	}
	appendAttempts(primaryIter)
	appendAttempts(fallbackIter)
	appendAttempts(activeIter)
	return attempts
}

type streamFailureEmitter interface {
	EmitStreamFailure(ctx context.Context, cause error) ([]byte, error)
}

// attempt 统一管理一次通道尝试的完整生命周期
func (ra *relayAttempt) attempt() attemptResult {
	span := ra.iter.StartAttempt(ra.channel.ID, ra.usedKey.ID, ra.channel.Name)
	ra.tracef("attempt_forward: channel=%s stream=%t", ra.channel.Name, ra.isStreamRequest())

	// 转发请求
	statusCode, fwdErr := ra.forward()

	// 更新 channel key 状态
	ra.usedKey.StatusCode = statusCode
	ra.usedKey.LastUseTimeStamp = time.Now().Unix()

	if fwdErr == nil {
		// ====== 成功 ======
		ra.collectResponse()
		ra.usedKey.TotalCost += ra.metrics.Stats.InputCost + ra.metrics.Stats.OutputCost
		op.ChannelKeyUpdate(ra.usedKey)

		span.End(dbmodel.AttemptSuccess, statusCode, "")

		// Channel 维度统计
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:       span.Duration().Milliseconds(),
			RequestSuccess: 1,
		})

		// 熔断器：记录成功
		balancer.RecordSuccess(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		// 仅在未启用统一路由亲和时，保留旧的模型级 sticky。
		if ra.routeAffinityMode != dbmodel.GroupRouteAffinityModeOff && ra.responsesStateful == nil && ra.conversationAffinity == nil {
			balancer.SetSticky(ra.apiKeyID, ra.requestModel, ra.channel.ID, ra.usedKey.ID)
		}
		if ra.responsesStateful != nil {
			lookupKeys := ra.responsesStateful.AllLookupKeys()
			if len(lookupKeys) > 0 {
				rememberResponsesAffinityRoute(ra.apiKeyID, lookupKeys, affinityRoute{
					ChannelID:    ra.channel.ID,
					ChannelKeyID: ra.usedKey.ID,
					BaseURL:      ra.selectedBaseURL,
				})
				ra.tracef("responses_stateful_bound: lookup_keys=%d channel=%d key_id=%d base_url=%s", len(lookupKeys), ra.channel.ID, ra.usedKey.ID, normalizeAffinityBaseURL(ra.selectedBaseURL))
			}
		}
		if ra.conversationAffinity != nil && len(ra.conversationAffinity.LookupKeys) > 0 {
			rememberConversationAffinityRoute(ra.apiKeyID, ra.conversationAffinity.LookupKeys, affinityRoute{
				ChannelID:    ra.channel.ID,
				ChannelKeyID: ra.usedKey.ID,
				BaseURL:      ra.selectedBaseURL,
			})
			ra.tracef("conversation_affinity_bound: lookup_keys=%d channel=%d key_id=%d base_url=%s", len(ra.conversationAffinity.LookupKeys), ra.channel.ID, ra.usedKey.ID, normalizeAffinityBaseURL(ra.selectedBaseURL))
		}

		ra.tracef("attempt_result: channel=%s success status=%d", ra.channel.Name, statusCode)
		return attemptResult{Success: true}
	}

	// ====== 失败 ======
	op.ChannelKeyUpdate(ra.usedKey)
	span.End(dbmodel.AttemptFailed, statusCode, fwdErr.Error())

	// Channel 维度统计
	op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
		WaitTime:      span.Duration().Milliseconds(),
		RequestFailed: 1,
	})

	// 熔断器：记录失败
	balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)

	written := ra.c.Writer.Written()
	ra.tracef("attempt_result: channel=%s failed status=%d written=%t error=%s", ra.channel.Name, statusCode, written, fwdErr.Error())
	if written {
		ra.emitStreamFailureTrailer(ra.c.Request.Context(), fwdErr)
		ra.collectResponse()
	}
	nonRetryable := ra.shouldStopCrossRouteRetry(fwdErr)
	return attemptResult{
		Success:      false,
		Written:      written,
		NonRetryable: nonRetryable,
		Err:          fmt.Errorf("channel %s failed: %w", ra.channel.Name, fwdErr),
	}
}

func (ra *relayAttempt) shouldStopCrossRouteRetry(cause error) bool {
	if ra == nil || cause == nil {
		return false
	}
	if ra.responsesStateful != nil && ra.responsesStateful.BlocksCrossRouteFailover() {
		if isInvalidEncryptedContentError(cause) {
			ra.tracef("responses_stateful_stop_retry: reason=invalid_encrypted_content")
			return true
		}
		if ra.responsesStateful.HasPinnedRoute() {
			ra.tracef("responses_stateful_stop_retry: reason=pinned_route_failed")
			return true
		}
	}
	if ra.conversationAffinity != nil && ra.conversationAffinity.BlocksCrossRouteFailover() && ra.conversationAffinity.HasPreferredRoute() && ra.channel != nil && ra.channel.ID == ra.conversationAffinity.PreferredRoute.ChannelID {
		ra.tracef("conversation_affinity_stop_retry: reason=pinned_route_failed")
		return true
	}
	return false
}

func isInvalidEncryptedContentError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "invalid_encrypted_content") || strings.Contains(message, "encrypted content") && strings.Contains(message, "could not be verified") {
		return true
	}
	var ue upstreamHTTPError
	if errors.As(err, &ue) {
		body := strings.ToLower(string(ue.Body))
		return strings.Contains(body, "invalid_encrypted_content") || strings.Contains(body, "encrypted content") && strings.Contains(body, "could not be verified")
	}
	return false
}

func (ra *relayAttempt) emitStreamFailureTrailer(ctx context.Context, cause error) {
	if ra == nil || cause == nil || !ra.isStreamRequest() || ra.inAdapter == nil {
		return
	}
	if ra.metrics != nil {
		if ra.metrics.ClientChunkCount == 0 || ra.metrics.TerminalSeen {
			return
		}
	}

	emitter, ok := ra.inAdapter.(streamFailureEmitter)
	if !ok {
		return
	}

	chunk, err := emitter.EmitStreamFailure(ctx, cause)
	if err != nil {
		log.Warnf("failed to emit stream failure trailer: %v", err)
		ra.tracef("emit_failure_trailer_error: %v", err)
		return
	}
	if len(chunk) == 0 {
		ra.tracef("emit_failure_trailer_skipped: empty chunk")
		return
	}
	ra.tracef("emit_failure_trailer: bytes=%d cause=%s", len(chunk), cause.Error())
	if ra.metrics != nil {
		ra.metrics.RecordClientChunk(time.Now(), chunk)
	}
	_, _ = ra.c.Writer.Write(chunk)
	ra.c.Writer.Flush()
}

// parseRequest 解析并验证入站请求
func parseRequest(inboundType inbound.InboundType, c *gin.Context) (*model.InternalLLMRequest, model.Inbound, error) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return nil, nil, err
	}

	inAdapter := inbound.Get(inboundType)
	internalRequest, err := inAdapter.TransformRequest(c.Request.Context(), body)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return nil, nil, err
	}

	// Pass through the original query parameters
	internalRequest.Query = c.Request.URL.Query()

	if err := internalRequest.Validate(); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return nil, nil, err
	}

	return internalRequest, inAdapter, nil
}

func formatRelayTimeout(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return d.String()
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	type timeout interface{ Timeout() bool }
	var te timeout
	return errors.As(err, &te) && te.Timeout()
}

func upstreamNonStreamTimeoutError() error {
	return fmt.Errorf("upstream non-stream timeout after %s", formatRelayTimeout(relayNonStreamTimeout))
}

func upstreamStreamIdleTimeoutError() error {
	return fmt.Errorf("upstream stream idle timeout after %s", formatRelayTimeout(relayStreamIdleTimeout))
}

func upstreamStreamBeforeFirstEventError() error {
	return fmt.Errorf("upstream stream ended before first event")
}

func upstreamStreamUnexpectedEOFError() error {
	return fmt.Errorf("upstream stream ended unexpectedly before terminal event")
}

func upstreamStreamReadError(err error) error {
	return fmt.Errorf("upstream stream read error: %w", err)
}

func writeRelayError(c *gin.Context, metrics *RelayMetrics, code int, message string) {
	payload := resp.ResponseStruct{Code: code, Message: message}
	if metrics != nil {
		if body, err := json.Marshal(payload); err == nil {
			metrics.SetClientResponseBody(body)
		}
		metrics.RecordExecutionTrace(fmt.Sprintf("local_error_response: status=%d message=%s", code, message))
	}
	c.AbortWithStatusJSON(code, payload)
}

func upstreamResponseReadError(err error) error {
	return fmt.Errorf("upstream response read error: %w", err)
}

func firstMeaningfulOutputTimeoutError(sec int) error {
	timeout := time.Duration(sec) * time.Second
	if timeout > 0 {
		return firstMeaningfulOutputTimeoutDurationError(timeout)
	}
	return fmt.Errorf("first meaningful output timeout")
}

func firstMeaningfulOutputTimeoutDurationError(timeout time.Duration) error {
	if timeout > 0 && timeout%time.Second == 0 {
		return fmt.Errorf("first meaningful output timeout (%ds)", int(timeout/time.Second))
	}
	return fmt.Errorf("first meaningful output timeout after %s", formatRelayTimeout(timeout))
}

func (ra *relayAttempt) isStreamRequest() bool {
	return ra != nil && ra.internalRequest != nil && ra.internalRequest.Stream != nil && *ra.internalRequest.Stream
}

func (ra *relayAttempt) tracef(format string, args ...any) {
	if ra == nil || ra.metrics == nil {
		return
	}
	ra.metrics.RecordExecutionTrace(fmt.Sprintf(format, args...))
}

func (ra *relayAttempt) shouldBufferPreludeUntilMeaningfulOutput() bool {
	return ra != nil && ra.internalRequest != nil && ra.internalRequest.RawAPIFormat == model.APIFormatOpenAIResponse
}

func (ra *relayAttempt) effectiveFirstMeaningfulOutputTimeout() time.Duration {
	if ra == nil {
		return 0
	}
	if ra.firstTokenTimeOutSec > 0 {
		return time.Duration(ra.firstTokenTimeOutSec) * time.Second
	}
	if ra.shouldBufferPreludeUntilMeaningfulOutput() && ra.shouldPassthroughSSE() && relayResponsesPreludeTimeout > 0 {
		return relayResponsesPreludeTimeout
	}
	return 0
}

func (ra *relayAttempt) isPassthroughTerminalEvent(eventType, data string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "[DONE]" {
		return true
	}

	if ra == nil || ra.internalRequest == nil {
		return false
	}

	switch ra.internalRequest.RawAPIFormat {
	case model.APIFormatOpenAIResponse:
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err == nil {
			switch payload.Type {
			case "response.completed", "response.incomplete", "response.failed", "error":
				return true
			}
		}
	case model.APIFormatOpenAIChatCompletion:
		var payload struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason,omitempty"`
			} `json:"choices,omitempty"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err == nil {
			for _, choice := range payload.Choices {
				if choice.FinishReason != nil {
					return true
				}
			}
		}
	case model.APIFormatAnthropicMessage:
		return strings.EqualFold(strings.TrimSpace(eventType), "message_stop")
	}

	return false
}

func isTerminalInternalStream(stream *model.InternalLLMResponse) bool {
	if stream == nil {
		return false
	}
	if stream.Object == "[DONE]" {
		return true
	}
	for _, choice := range stream.Choices {
		if choice.FinishReason != nil {
			return true
		}
	}
	return false
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, d time.Duration) {
	if timer == nil {
		return
	}
	stopTimer(timer)
	timer.Reset(d)
}

type responsesEventProgress struct {
	EventType  string
	Meaningful bool
	Terminal   bool
	KeepAlive  bool
}

func inspectResponsesEvent(data string) responsesEventProgress {
	payload := strings.TrimSpace(data)
	if payload == "" {
		return responsesEventProgress{}
	}
	if payload == "[DONE]" {
		return responsesEventProgress{EventType: "[DONE]", Terminal: true, KeepAlive: true}
	}

	var meta struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(payload), &meta); err != nil {
		log.Warnf("failed to inspect responses event payload: %v", err)
		// Unknown non-JSON payload should not unlock client writes or keep the stream alive forever.
		return responsesEventProgress{EventType: "(non_json_payload)"}
	}

	eventType := strings.TrimSpace(meta.Type)
	if eventType == "" {
		return responsesEventProgress{EventType: "(missing_type)"}
	}

	switch eventType {
	case "response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.reasoning_summary_part.added", "response.output_item.done", "response.content_part.done", "response.reasoning_summary_part.done":
		return responsesEventProgress{EventType: eventType, KeepAlive: true}
	case "response.output_text.delta", "response.output_text.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.refusal.delta", "response.refusal.done":
		return responsesEventProgress{EventType: eventType, Meaningful: true, KeepAlive: true}
	case "response.completed", "response.incomplete", "response.failed", "error":
		return responsesEventProgress{EventType: eventType, Terminal: true, KeepAlive: true}
	default:
		// Treat unknown Responses events conservatively so a new prelude-only event family
		// does not leak to the client and permanently disable retry.
		return responsesEventProgress{EventType: eventType}
	}
}

// forward 转发请求到上游服务
func (ra *relayAttempt) forward() (int, error) {
	ctx := ra.c.Request.Context()
	var cancel context.CancelFunc
	if !ra.isStreamRequest() && relayNonStreamTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, relayNonStreamTimeout)
		defer cancel()
	}

	if beta := strings.TrimSpace(ra.c.Request.Header.Get("Anthropic-Beta")); beta != "" {
		ctx = context.WithValue(ctx, "anthropic_beta_header", beta)
	}

	// 构建出站请求
	baseURL := ra.selectedBaseURL
	if baseURL == "" {
		baseURL = ra.channel.GetBaseUrl()
	}
	outboundRequest, err := ra.outAdapter.TransformRequest(
		ctx,
		ra.internalRequest,
		baseURL,
		ra.usedKey.ChannelKey,
	)
	if err != nil {
		log.Warnf("failed to create request: %v", err)
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	// 复制请求头
	ra.copyHeaders(outboundRequest)

	// 发送请求
	response, err := ra.sendRequest(outboundRequest)
	if err != nil {
		if !ra.isStreamRequest() && relayNonStreamTimeout > 0 && isTimeoutError(ctx.Err()) {
			return 0, upstreamNonStreamTimeoutError()
		}
		return 0, fmt.Errorf("failed to send request: %w", err)
	}
	defer response.Body.Close()

	// 处理响应（包含非 2xx 错误透传/转换）
	if ra.internalRequest.Stream != nil && *ra.internalRequest.Stream {
		if err := ra.handleStreamResponse(ctx, response); err != nil {
			return response.StatusCode, err
		}
		return response.StatusCode, nil
	}
	if err := ra.handleResponse(ctx, response); err != nil {
		return response.StatusCode, err
	}
	return response.StatusCode, nil
}

// copyHeaders 复制请求头，过滤 hop-by-hop 头
func (ra *relayAttempt) copyHeaders(outboundRequest *http.Request) {
	for key, values := range ra.c.Request.Header {
		if hopByHopHeaders[strings.ToLower(key)] {
			continue
		}
		if strings.EqualFold(key, "accept") || strings.EqualFold(key, "authorization") || strings.EqualFold(key, "x-api-key") {
			continue
		}
		if outboundRequest.Header.Get(key) != "" {
			continue
		}
		for _, value := range values {
			outboundRequest.Header.Set(key, value)
		}
	}
	if len(ra.channel.CustomHeader) > 0 {
		for _, header := range ra.channel.CustomHeader {
			outboundRequest.Header.Set(header.HeaderKey, header.HeaderValue)
		}
	}
}

// sendRequest 发送 HTTP 请求
func (ra *relayAttempt) sendRequest(req *http.Request) (*http.Response, error) {
	httpClient, err := helper.ChannelHttpClient(ra.channel)
	if err != nil {
		log.Warnf("failed to get http client: %v", err)
		return nil, err
	}

	response, err := httpClient.Do(req)
	if err != nil {
		log.Warnf("failed to send request: %v", err)
		return nil, err
	}

	return response, nil
}

// handleStreamResponse 处理流式响应
func (ra *relayAttempt) handleStreamResponse(ctx context.Context, response *http.Response) error {
	if response == nil {
		return fmt.Errorf("response is nil")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 256*1024))
		return upstreamHTTPError{
			StatusCode:  response.StatusCode,
			ContentType: response.Header.Get("Content-Type"),
			Body:        body,
		}
	}
	if ct := response.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
		return fmt.Errorf("upstream returned non-SSE content-type %q for stream request: %s", ct, string(body))
	}

	passthroughSSE := ra.shouldPassthroughSSE()
	responsesPassthrough := passthroughSSE && ra.internalRequest != nil && ra.internalRequest.RawAPIFormat == model.APIFormatOpenAIResponse
	if passthroughSSE {
		log.Infof("SSE passthrough enabled for channel %s", ra.channel.Name)
	}

	// 设置 SSE 响应头
	ra.c.Header("Content-Type", "text/event-stream")
	ra.c.Header("Cache-Control", "no-cache")
	ra.c.Header("Connection", "keep-alive")
	ra.c.Header("X-Accel-Buffering", "no")

	firstToken := true
	sawAnyEvent := false
	sawTerminalEvent := false
	bufferPrelude := responsesPassthrough && ra.shouldBufferPreludeUntilMeaningfulOutput()
	type pendingSSEChunk struct {
		eventType string
		data      string
		encoded   []byte
	}
	pendingPrelude := make([]pendingSSEChunk, 0, 4)

	type sseReadResult struct {
		eventType string
		data      string
		err       error
	}

	stop := make(chan struct{})
	defer close(stop)

	results := make(chan sseReadResult, 16)
	go func() {
		defer close(results)
		readCfg := &sse.ReadConfig{MaxEventSize: maxSSEEventSize}
		for ev, err := range sse.Read(response.Body, readCfg) {
			if err != nil {
				select {
				case results <- sseReadResult{err: err}:
				case <-stop:
				}
				return
			}
			select {
			case results <- sseReadResult{eventType: ev.Type, data: ev.Data}:
			case <-stop:
				return
			}
		}
	}()

	firstMeaningfulOutputTimeout := ra.effectiveFirstMeaningfulOutputTimeout()
	var firstTokenTimer *time.Timer
	var firstTokenC <-chan time.Time
	if firstToken && firstMeaningfulOutputTimeout > 0 {
		firstTokenTimer = time.NewTimer(firstMeaningfulOutputTimeout)
		firstTokenC = firstTokenTimer.C
		defer func() {
			stopTimer(firstTokenTimer)
		}()
	}

	var idleTimer *time.Timer
	var idleC <-chan time.Time
	if relayStreamIdleTimeout > 0 {
		idleTimer = time.NewTimer(relayStreamIdleTimeout)
		idleC = idleTimer.C
		defer func() {
			stopTimer(idleTimer)
		}()
	}

	ra.tracef("stream_open: status=%d content_type=%s passthrough=%t responses_passthrough=%t buffer_prelude=%t first_meaningful_timeout=%s idle_timeout=%s", response.StatusCode, strings.TrimSpace(response.Header.Get("Content-Type")), passthroughSSE, responsesPassthrough, bufferPrelude, formatRelayTimeout(firstMeaningfulOutputTimeout), formatRelayTimeout(relayStreamIdleTimeout))

	writeChunk := func(chunk []byte) {
		if len(chunk) == 0 {
			return
		}
		if ra.metrics != nil {
			ra.metrics.RecordClientChunk(time.Now(), chunk)
		}
		ra.c.Writer.Write(chunk)
		ra.c.Writer.Flush()
	}

	writePassthroughChunk := func(eventType, rawData string, chunk []byte) {
		if len(chunk) == 0 {
			return
		}
		if rawData != "" {
			ra.tapStreamForMetrics(ctx, rawData)
		}
		writeChunk(chunk)
	}

	flushPendingPrelude := func() {
		if len(pendingPrelude) > 0 {
			ra.tracef("stream_prelude_flush: buffered_chunks=%d", len(pendingPrelude))
		}
		for _, chunk := range pendingPrelude {
			writePassthroughChunk(chunk.eventType, chunk.data, chunk.encoded)
		}
		pendingPrelude = pendingPrelude[:0]
	}

	streamEventIndex := 0

	for {
		select {
		case <-ctx.Done():
			ra.tracef("stream_context_done: client_chunks=%d saw_any_event=%t terminal_seen=%t", ra.metrics.ClientChunkCount, sawAnyEvent, sawTerminalEvent)
			log.Infof("client disconnected, stopping stream")
			_ = response.Body.Close()
			return nil
		case <-idleC:
			ra.tracef("stream_timeout: idle after_write=%t saw_any_event=%t buffered_prelude=%d", ra.metrics.ClientChunkCount > 0, sawAnyEvent, len(pendingPrelude))
			log.Warnf("upstream stream idle timeout after %s", formatRelayTimeout(relayStreamIdleTimeout))
			_ = response.Body.Close()
			if ra.metrics != nil {
				ra.metrics.SetFailureStage("stream_idle_timeout")
			}
			return upstreamStreamIdleTimeoutError()
		case <-firstTokenC:
			ra.tracef("stream_timeout: first_meaningful_output buffered_prelude=%d upstream_events=%d", len(pendingPrelude), ra.metrics.UpstreamEventCount)
			log.Warnf("first meaningful output timeout after %s, switching channel", formatRelayTimeout(firstMeaningfulOutputTimeout))
			_ = response.Body.Close()
			if ra.metrics != nil {
				ra.metrics.SetFailureStage("first_meaningful_output_timeout")
			}
			return firstMeaningfulOutputTimeoutDurationError(firstMeaningfulOutputTimeout)
		case r, ok := <-results:
			if !ok {
				ra.tracef("stream_reader_closed: saw_any_event=%t terminal_seen=%t", sawAnyEvent, sawTerminalEvent)
				if !sawAnyEvent {
					log.Warnf("upstream stream ended before first event")
					if ra.metrics != nil {
						ra.metrics.SetFailureStage("stream_ended_before_first_event")
					}
					return upstreamStreamBeforeFirstEventError()
				}
				if !sawTerminalEvent {
					log.Warnf("upstream stream ended unexpectedly before terminal event")
					if ra.metrics != nil {
						ra.metrics.SetFailureStage("stream_unexpected_eof_before_terminal")
					}
					return upstreamStreamUnexpectedEOFError()
				}
				log.Infof("stream end after terminal event")
				return nil
			}
			if r.err != nil {
				ra.tracef("stream_read_error: %v", r.err)
				log.Warnf("failed to read event: %v", r.err)
				_ = response.Body.Close()
				if ra.metrics != nil {
					ra.metrics.SetFailureStage("stream_read_error")
				}
				return upstreamStreamReadError(r.err)
			}

			sawAnyEvent = true
			if ra.metrics != nil {
				ra.metrics.RecordUpstreamEvent(time.Now())
			}
			streamEventIndex++

			var responsesProgress responsesEventProgress
			if bufferPrelude || responsesPassthrough {
				responsesProgress = inspectResponsesEvent(r.data)
			}
			if responsesPassthrough && ra.metrics != nil {
				ra.metrics.RecordUpstreamEventType(responsesProgress.EventType)
				ra.tracef("stream_event#%d: type=%s meaningful=%t keepalive=%t terminal=%t", streamEventIndex, responsesProgress.EventType, responsesProgress.Meaningful, responsesProgress.KeepAlive, responsesProgress.Terminal)
			} else if ra.internalRequest != nil && ra.internalRequest.RawAPIFormat == model.APIFormatOpenAIResponse {
				eventType := strings.TrimSpace(r.eventType)
				if eventType == "" {
					eventType = "data"
				}
				ra.tracef("stream_event#%d: upstream_event=%s passthrough=false data_bytes=%d", streamEventIndex, eventType, len(r.data))
			}

			var (
				data     []byte
				err      error
				terminal bool
			)

			if passthroughSSE {
				terminal = ra.isPassthroughTerminalEvent(r.eventType, r.data)
				if responsesPassthrough {
					if responsesProgress.Terminal {
						terminal = true
					}
					if !responsesProgress.KeepAlive && !responsesProgress.Meaningful && !responsesProgress.Terminal {
						ra.tracef("stream_event#%d action=drop_non_progress_event", streamEventIndex)
						continue
					}
				}
				if r.data == "" {
					if terminal {
						ra.tracef("stream_event#%d action=terminal_empty_event", streamEventIndex)
						sawTerminalEvent = true
						if ra.metrics != nil {
							ra.metrics.MarkTerminalSeen()
						}
						stopTimer(idleTimer)
						idleTimer = nil
						idleC = nil
					}
					continue
				}
				switch ra.internalRequest.RawAPIFormat {
				case model.APIFormatAnthropicMessage:
					data = formatSSEEventString(r.eventType, r.data)
				default:
					data = formatSSEDataString(r.data)
				}

				if idleTimer != nil && !sawTerminalEvent && (!responsesPassthrough || responsesProgress.KeepAlive) {
					resetTimer(idleTimer, relayStreamIdleTimeout)
				}

				// Stop early once the upstream signals completion.
				if strings.TrimSpace(r.data) == "[DONE]" {
					sawTerminalEvent = true
					if ra.metrics != nil {
						ra.metrics.MarkTerminalSeen()
					}
					if firstToken {
						ra.metrics.SetFirstTokenTime(time.Now())
						firstToken = false
					}
					flushPendingPrelude()
					writePassthroughChunk(r.eventType, r.data, data)
					_ = response.Body.Close()
					return nil
				}
			} else {
				data, terminal, err = ra.transformStreamData(ctx, r.data)
				if ra.internalRequest != nil && ra.internalRequest.RawAPIFormat == model.APIFormatOpenAIResponse {
					ra.tracef("stream_event#%d transformed_bytes=%d terminal=%t transform_err=%t", streamEventIndex, len(data), terminal, err != nil)
				}
				if err != nil || len(data) == 0 {
					if terminal {
						sawTerminalEvent = true
						if ra.metrics != nil {
							ra.metrics.MarkTerminalSeen()
						}
						stopTimer(idleTimer)
						idleTimer = nil
						idleC = nil
					}
					continue
				}
			}

			if terminal {
				ra.tracef("stream_terminal_seen: event#%d", streamEventIndex)
				sawTerminalEvent = true
				if ra.metrics != nil {
					ra.metrics.MarkTerminalSeen()
				}
				stopTimer(idleTimer)
				idleTimer = nil
				idleC = nil
			}

			if bufferPrelude && len(data) > 0 && firstToken {
				if responsesProgress.Terminal {
					terminal = true
				}
				if !responsesProgress.Meaningful && !terminal {
					pendingPrelude = append(pendingPrelude, pendingSSEChunk{eventType: r.eventType, data: r.data, encoded: data})
					ra.tracef("stream_event#%d action=buffer_prelude buffered_chunks=%d", streamEventIndex, len(pendingPrelude))
					continue
				}
			}

			if idleTimer != nil && !sawTerminalEvent {
				resetTimer(idleTimer, relayStreamIdleTimeout)
			}

			if firstToken {
				ra.tracef("first_client_write: event#%d chunk_bytes=%d", streamEventIndex, len(data))
				ra.metrics.SetFirstTokenTime(time.Now())
				firstToken = false
				if firstTokenTimer != nil {
					stopTimer(firstTokenTimer)
					firstTokenTimer = nil
					firstTokenC = nil
				}
			}

			flushPendingPrelude()
			if passthroughSSE {
				writePassthroughChunk(r.eventType, r.data, data)
			} else {
				writeChunk(data)
			}
		}
	}
}

func (ra *relayAttempt) shouldPassthroughSSE() bool {
	if ra == nil || ra.internalRequest == nil || ra.channel == nil {
		return false
	}
	if ra.internalRequest.Stream == nil || !*ra.internalRequest.Stream {
		return false
	}
	// We only passthrough streaming responses when the upstream response stream
	// is already in the client protocol format.
	switch ra.internalRequest.RawAPIFormat {
	case model.APIFormatOpenAIChatCompletion:
		return ra.channel.Type == outbound.OutboundTypeOpenAIChat
	case model.APIFormatOpenAIResponse:
		return ra.channel.Type == outbound.OutboundTypeOpenAIResponse
	case model.APIFormatAnthropicMessage:
		return ra.channel.Type == outbound.OutboundTypeAnthropic
	default:
		return false
	}
}

func (ra *relayAttempt) tapStreamForMetrics(ctx context.Context, data string) {
	if ra == nil || ra.internalRequest == nil {
		return
	}
	if ra.internalRequest.Stream == nil || !*ra.internalRequest.Stream {
		return
	}
	if ra.outAdapter == nil || ra.inAdapter == nil {
		return
	}

	ra.observeResponsesAffinityPayload([]byte(data))
	internalStream, err := ra.outAdapter.TransformStream(ctx, []byte(data))
	if err != nil {
		log.Warnf("failed to tap outbound stream for metrics: %v", err)
		goto fallback
	}
	if internalStream != nil {
		// Feed the internal chunk into inbound adapter so GetInternalResponse() can
		// later aggregate a complete response for metrics/logging.
		if _, err := ra.inAdapter.TransformStream(ctx, internalStream); err != nil {
			log.Warnf("failed to tap inbound stream for metrics: %v", err)
		}

		// Ensure usage is recorded even if aggregation fails later.
		if internalStream.Usage != nil && (ra.metrics == nil || ra.metrics.InternalResponse == nil || ra.metrics.InternalResponse.Usage == nil) {
			if ra.metrics != nil {
				ra.metrics.SetInternalResponse(&model.InternalLLMResponse{Usage: internalStream.Usage}, ra.internalRequest.Model)
			}
		}
		return
	}

fallback:
	// Fallback: try to parse usage directly from Responses stream events.
	if ra.metrics == nil || ra.internalRequest.RawAPIFormat != model.APIFormatOpenAIResponse {
		return
	}
	if ra.metrics.InternalResponse != nil && ra.metrics.InternalResponse.Usage != nil {
		return
	}
	// Avoid JSON parsing on non-usage events.
	if !strings.Contains(data, `"usage"`) {
		return
	}

	var raw struct {
		Response struct {
			Usage *struct {
				InputTokens        int64 `json:"input_tokens"`
				OutputTokens       int64 `json:"output_tokens"`
				TotalTokens        int64 `json:"total_tokens"`
				InputTokensDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
				OutputTokensDetails struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil || raw.Response.Usage == nil {
		return
	}
	usage := &model.Usage{
		PromptTokens:     raw.Response.Usage.InputTokens,
		CompletionTokens: raw.Response.Usage.OutputTokens,
		TotalTokens:      raw.Response.Usage.TotalTokens,
	}
	usage.PromptTokensDetails = &model.PromptTokensDetails{CachedTokens: raw.Response.Usage.InputTokensDetails.CachedTokens}
	usage.CompletionTokensDetails = &model.CompletionTokensDetails{ReasoningTokens: raw.Response.Usage.OutputTokensDetails.ReasoningTokens}
	ra.metrics.SetInternalResponse(&model.InternalLLMResponse{Usage: usage}, ra.internalRequest.Model)
}

func formatSSEDataString(data string) []byte {
	lines := strings.Split(data, "\n")
	var sb strings.Builder
	for _, line := range lines {
		sb.WriteString("data: ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	return []byte(sb.String())
}

func formatSSEEventString(eventType string, data string) []byte {
	var sb strings.Builder
	if strings.TrimSpace(eventType) != "" {
		sb.WriteString("event: ")
		sb.WriteString(eventType)
		sb.WriteString("\n")
	}
	sb.Write(formatSSEDataString(data))
	return []byte(sb.String())
}

func (ra *relayAttempt) observeResponsesAffinityPayload(payload []byte) {
	if ra == nil || ra.responsesStateful == nil || len(payload) == 0 {
		return
	}
	observeResponsesAffinityFromPayload(ra.groupID, ra.requestModel, ra.responsesStateful, payload)
}

// transformStreamData 转换流式数据
func (ra *relayAttempt) transformStreamData(ctx context.Context, data string) ([]byte, bool, error) {
	ra.observeResponsesAffinityPayload([]byte(data))
	internalStream, err := ra.outAdapter.TransformStream(ctx, []byte(data))
	if err != nil {
		log.Warnf("failed to transform stream: %v", err)
		return nil, false, err
	}
	if internalStream == nil {
		return nil, false, nil
	}
	terminal := isTerminalInternalStream(internalStream)

	inStream, err := ra.inAdapter.TransformStream(ctx, internalStream)
	if err != nil {
		log.Warnf("failed to transform stream: %v", err)
		return nil, terminal, err
	}

	return inStream, terminal, nil
}

// handleResponse 处理非流式响应
func (ra *relayAttempt) handleResponse(ctx context.Context, response *http.Response) error {
	if response == nil {
		return fmt.Errorf("response is nil")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		bodyBytes, err := io.ReadAll(response.Body)
		if err != nil {
			log.Warnf("failed to read error response body: %v", err)
			if relayNonStreamTimeout > 0 && isTimeoutError(ctx.Err()) {
				return upstreamNonStreamTimeoutError()
			}
			return upstreamResponseReadError(err)
		}

		// Best-effort: still populate internal response for metrics/logging.
		if ra.outAdapter != nil && ra.inAdapter != nil {
			clone := *response
			clone.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			if internalResponse, err := ra.outAdapter.TransformResponse(ctx, &clone); err != nil {
				log.Warnf("failed to tap error response for metrics: %v", err)
			} else if internalResponse != nil {
				_, _ = ra.inAdapter.TransformResponse(ctx, internalResponse)
			}
		}

		return upstreamHTTPError{
			StatusCode:  response.StatusCode,
			ContentType: response.Header.Get("Content-Type"),
			Body:        bodyBytes,
		}
	}

	if ra.shouldPassthroughNonStream() {
		bodyBytes, err := io.ReadAll(response.Body)
		if err != nil {
			log.Warnf("failed to read response body: %v", err)
			if relayNonStreamTimeout > 0 && isTimeoutError(ctx.Err()) {
				return upstreamNonStreamTimeoutError()
			}
			return upstreamResponseReadError(err)
		}
		if len(bodyBytes) == 0 {
			return fmt.Errorf("upstream response body is empty")
		}

		// Best-effort: still populate internal response for metrics/logging without
		// altering the client-facing passthrough response.
		if ra.outAdapter != nil && ra.inAdapter != nil {
			clone := *response
			clone.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			if internalResponse, err := ra.outAdapter.TransformResponse(ctx, &clone); err != nil {
				log.Warnf("failed to tap passthrough response for metrics: %v", err)
			} else if internalResponse != nil {
				_, _ = ra.inAdapter.TransformResponse(ctx, internalResponse)
			}
		}

		contentType := strings.TrimSpace(response.Header.Get("Content-Type"))
		if contentType == "" {
			contentType = "application/json"
		}
		ra.observeResponsesAffinityPayload(bodyBytes)
		if ra.metrics != nil {
			ra.metrics.SetClientResponseBody(bodyBytes)
		}
		ra.c.Data(http.StatusOK, contentType, bodyBytes)
		return nil
	}

	internalResponse, err := ra.outAdapter.TransformResponse(ctx, response)
	if err != nil {
		log.Warnf("failed to transform response: %v", err)
		if relayNonStreamTimeout > 0 && isTimeoutError(ctx.Err()) {
			return upstreamNonStreamTimeoutError()
		}
		return fmt.Errorf("failed to transform outbound response: %w", err)
	}

	if internalResponse != nil {
		observeResponsesAffinityFromInternalResponse(ra.groupID, ra.requestModel, ra.responsesStateful, internalResponse)
	}
	inResponse, err := ra.inAdapter.TransformResponse(ctx, internalResponse)
	if err != nil {
		log.Warnf("failed to transform response: %v", err)
		return fmt.Errorf("failed to transform inbound response: %w", err)
	}

	if ra.metrics != nil {
		ra.metrics.SetClientResponseBody(inResponse)
	}
	ra.c.Data(http.StatusOK, "application/json", inResponse)
	return nil
}

func (ra *relayAttempt) shouldPassthroughNonStream() bool {
	if ra == nil || ra.internalRequest == nil || ra.channel == nil {
		return false
	}
	if ra.internalRequest.Stream != nil && *ra.internalRequest.Stream {
		return false
	}

	switch ra.internalRequest.RawAPIFormat {
	case model.APIFormatOpenAIChatCompletion:
		return ra.channel.Type == outbound.OutboundTypeOpenAIChat
	case model.APIFormatOpenAIResponse:
		return ra.channel.Type == outbound.OutboundTypeOpenAIResponse
	case model.APIFormatAnthropicMessage:
		return ra.channel.Type == outbound.OutboundTypeAnthropic
	default:
		return false
	}
}

// collectResponse 收集响应信息
func (ra *relayAttempt) collectResponse() {
	internalResponse, err := ra.inAdapter.GetInternalResponse(ra.c.Request.Context())
	if err != nil {
		ra.tracef("collect_response_error: %v", err)
		return
	}
	if internalResponse == nil {
		ra.tracef("collect_response: empty")
		return
	}

	ra.tracef("collect_response: usage_present=%t choices=%d", internalResponse.Usage != nil, len(internalResponse.Choices))
	ra.metrics.SetInternalResponse(internalResponse, ra.internalRequest.Model)
}

func rawOnlyRequiredOutboundType(req *model.InternalLLMRequest) (outbound.OutboundType, bool) {
	if req == nil || !req.RawOnly {
		return 0, false
	}
	switch req.RawAPIFormat {
	case model.APIFormatOpenAIChatCompletion:
		return outbound.OutboundTypeOpenAIChat, true
	case model.APIFormatOpenAIResponse:
		return outbound.OutboundTypeOpenAIResponse, true
	case model.APIFormatAnthropicMessage:
		return outbound.OutboundTypeAnthropic, true
	default:
		return 0, false
	}
}
