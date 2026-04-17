package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	websocketCloseReasonLimit = 120
	websocketWriteTimeout     = 5 * time.Second
)

type responsesWebsocketSession struct {
	groupID           int
	channel           *dbmodel.Channel
	usedKey           dbmodel.ChannelKey
	selectedBaseURL   string
	upstreamConn      *websocket.Conn
	upstreamModel     string
	maxLifetime       time.Duration
	responsesStateful *responsesStatefulRequestContext
	iter              *balancer.Iterator
	span              *balancer.AttemptSpan
}

type responsesWebsocketRelayIOError struct {
	Op  string
	Err error
}

func (e *responsesWebsocketRelayIOError) Error() string {
	if e == nil {
		return ""
	}
	if e.Op == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s websocket frame: %v", e.Op, e.Err)
}

func (e *responsesWebsocketRelayIOError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func HandleResponsesWebsocket(c *gin.Context) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	clientConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer clientConn.Close()

	firstMessageType, firstPayload, internalRequest, err := readResponsesWebsocketCreateFrame(clientConn)
	apiKeyID := c.GetInt("api_key_id")
	requestModel := ""
	if internalRequest != nil {
		requestModel = internalRequest.Model
	}
	metrics := NewRelayMetrics(apiKeyID, requestModel, internalRequest)
	if len(firstPayload) > 0 {
		metrics.SetClientRequestBody(firstPayload)
	}
	metrics.RecordExecutionTrace("ws_accept: endpoint=/v1/responses transport=websocket")

	if err != nil {
		metrics.RecordExecutionTrace(fmt.Sprintf("ws_reject: invalid_first_event error=%s", err.Error()))
		metrics.Save(c.Request.Context(), false, err, nil)
		writeWebsocketClose(clientConn, websocket.CloseProtocolError, err.Error())
		return
	}

	supportedModels := c.GetString("supported_models")
	if supportedModels != "" {
		supportedModelsArray := strings.Split(supportedModels, ",")
		if !containsTrimmedString(supportedModelsArray, requestModel) {
			err := fmt.Errorf("model not supported")
			metrics.RecordExecutionTrace("ws_reject: model_not_supported")
			metrics.Save(c.Request.Context(), false, err, nil)
			writeWebsocketClose(clientConn, websocket.ClosePolicyViolation, err.Error())
			return
		}
	}

	group, err := op.GroupGetEnabledMap(requestModel, c.Request.Context())
	if err != nil {
		err = fmt.Errorf("model not found")
		metrics.RecordExecutionTrace("ws_reject: model_not_found")
		metrics.Save(c.Request.Context(), false, err, nil)
		writeWebsocketClose(clientConn, websocket.ClosePolicyViolation, err.Error())
		return
	}

	metrics.RecordExecutionTrace(fmt.Sprintf("ws_request: model=%s group_mode=%d items=%d", requestModel, group.Mode, len(group.Items)))
	if !group.GetResponsesWebsocketEnabled() {
		err := fmt.Errorf("responses websocket is disabled for this group")
		metrics.RecordExecutionTrace("ws_reject: websocket_disabled")
		metrics.Save(c.Request.Context(), false, err, nil)
		writeWebsocketClose(clientConn, websocket.ClosePolicyViolation, err.Error())
		return
	}

	responsesStateful := buildResponsesStatefulRequestContext(group, apiKeyID, requestModel, internalRequest)
	if responsesStateful != nil {
		metrics.RecordExecutionTrace(fmt.Sprintf("ws_stateful_route: enabled=%t pinned=%t protection=%s", responsesStateful.Enabled(), responsesStateful.HasPinnedRoute(), responsesStateful.ProtectionReason()))
	}

	session, err := establishResponsesWebsocketSession(c.Request.Context(), c, group, requestModel, metrics, responsesStateful)
	if err != nil {
		metrics.RecordExecutionTrace(fmt.Sprintf("ws_connect_failed: %s", err.Error()))
		attempts := []dbmodel.ChannelAttempt(nil)
		if session != nil && session.iter != nil {
			attempts = session.iter.Attempts()
		}
		metrics.Save(c.Request.Context(), false, err, attempts)
		writeWebsocketClose(clientConn, websocket.CloseTryAgainLater, err.Error())
		return
	}
	defer session.upstreamConn.Close()
	metrics.ActualModel = session.upstreamModel

	metrics.RecordExecutionTrace(fmt.Sprintf("ws_connected: channel=%s upstream_model=%s base_url=%s key_id=%d lifetime=%s", session.channel.Name, session.upstreamModel, normalizeAffinityBaseURL(session.selectedBaseURL), session.usedKey.ID, formatRelayTimeout(session.maxLifetime)))

	forwardPayload := rewriteResponsesWebsocketCreatePayload(firstPayload, session.upstreamModel)
	if err := session.upstreamConn.WriteMessage(firstMessageType, forwardPayload); err != nil {
		session.span.End(dbmodel.AttemptFailed, http.StatusSwitchingProtocols, err.Error())
		balancer.RecordFailure(session.channel.ID, session.usedKey.ID, requestModel)
		op.StatsChannelUpdate(session.channel.ID, dbmodel.StatsMetrics{RequestFailed: 1})
		session.usedKey.StatusCode = http.StatusSwitchingProtocols
		session.usedKey.LastUseTimeStamp = time.Now().Unix()
		op.ChannelKeyUpdate(session.usedKey)
		metrics.RecordExecutionTrace(fmt.Sprintf("ws_forward_failed: first_frame error=%s", err.Error()))
		metrics.Save(c.Request.Context(), false, err, session.iter.Attempts())
		writeWebsocketClose(clientConn, websocket.CloseInternalServerErr, err.Error())
		return
	}
	metrics.RecordExecutionTrace("ws_forwarded_first_frame: type=response.create")

	err = proxyResponsesWebsocket(clientConn, session, metrics)
	if err != nil {
		session.span.End(dbmodel.AttemptFailed, http.StatusSwitchingProtocols, err.Error())
		balancer.RecordFailure(session.channel.ID, session.usedKey.ID, requestModel)
		op.StatsChannelUpdate(session.channel.ID, dbmodel.StatsMetrics{RequestFailed: 1})
		session.usedKey.StatusCode = http.StatusSwitchingProtocols
		session.usedKey.LastUseTimeStamp = time.Now().Unix()
		op.ChannelKeyUpdate(session.usedKey)
		metrics.RecordExecutionTrace(fmt.Sprintf("ws_proxy_failed: %s", err.Error()))
		metrics.Save(c.Request.Context(), false, err, session.iter.Attempts())
		return
	}

	session.span.End(dbmodel.AttemptSuccess, http.StatusSwitchingProtocols, "")
	balancer.RecordSuccess(session.channel.ID, session.usedKey.ID, requestModel)
	op.StatsChannelUpdate(session.channel.ID, dbmodel.StatsMetrics{RequestSuccess: 1})
	session.usedKey.StatusCode = http.StatusSwitchingProtocols
	session.usedKey.LastUseTimeStamp = time.Now().Unix()
	op.ChannelKeyUpdate(session.usedKey)
	if session.responsesStateful != nil {
		lookupKeys := session.responsesStateful.AllLookupKeys()
		if len(lookupKeys) > 0 {
			rememberResponsesAffinityRoute(apiKeyID, lookupKeys, affinityRoute{
				ChannelID:    session.channel.ID,
				ChannelKeyID: session.usedKey.ID,
				BaseURL:      session.selectedBaseURL,
			})
			metrics.RecordExecutionTrace(fmt.Sprintf("ws_stateful_bound: lookup_keys=%d channel=%d key_id=%d base_url=%s", len(lookupKeys), session.channel.ID, session.usedKey.ID, normalizeAffinityBaseURL(session.selectedBaseURL)))
		}
	}
	metrics.RecordExecutionTrace("ws_proxy_complete: graceful_close")
	metrics.Save(c.Request.Context(), true, nil, session.iter.Attempts())
}

func readResponsesWebsocketCreateFrame(conn *websocket.Conn) (int, []byte, *transformerModel.InternalLLMRequest, error) {
	if conn == nil {
		return 0, nil, nil, fmt.Errorf("websocket connection is nil")
	}

	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		return 0, nil, nil, fmt.Errorf("failed to read first websocket message: %w", err)
	}
	if messageType != websocket.TextMessage {
		return 0, payload, nil, fmt.Errorf("first websocket message must be text")
	}

	body, modelName, err := extractResponsesCreateBody(payload)
	if err != nil {
		return 0, payload, nil, err
	}

	stream := true
	internalRequest := &transformerModel.InternalLLMRequest{
		Model:        modelName,
		Stream:       &stream,
		RawOnly:      true,
		RawRequest:   body,
		RawAPIFormat: transformerModel.APIFormatOpenAIResponse,
	}
	return messageType, payload, internalRequest, nil
}

func extractResponsesCreateBody(payload []byte) ([]byte, string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, "", fmt.Errorf("failed to decode response.create event: %w", err)
	}

	var eventType string
	if err := json.Unmarshal(raw["type"], &eventType); err != nil || strings.TrimSpace(eventType) == "" {
		return nil, "", fmt.Errorf("response.create event type is required")
	}
	if strings.TrimSpace(eventType) != "response.create" {
		return nil, "", fmt.Errorf("first websocket event must be response.create")
	}
	delete(raw, "type")

	body, err := json.Marshal(raw)
	if err != nil {
		return nil, "", fmt.Errorf("failed to normalize response.create payload: %w", err)
	}

	var createBody struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &createBody); err != nil {
		return nil, "", fmt.Errorf("failed to decode response.create model: %w", err)
	}
	modelName := strings.TrimSpace(createBody.Model)
	if modelName == "" {
		return nil, "", fmt.Errorf("model is required")
	}
	return body, modelName, nil
}

func rewriteResponsesWebsocketCreatePayload(payload []byte, upstreamModel string) []byte {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return payload
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return payload
	}
	raw["model"] = upstreamModel
	body, err := json.Marshal(raw)
	if err != nil {
		return payload
	}
	return body
}

func establishResponsesWebsocketSession(ctx context.Context, c *gin.Context, group dbmodel.Group, requestModel string, metrics *RelayMetrics, responsesStateful *responsesStatefulRequestContext) (*responsesWebsocketSession, error) {
	return establishResponsesWebsocketSessionWithDialer(ctx, c, group, requestModel, metrics, responsesStateful, helper.ChannelWebsocketDialer)
}

func establishResponsesWebsocketSessionWithDialer(ctx context.Context, c *gin.Context, group dbmodel.Group, requestModel string, metrics *RelayMetrics, responsesStateful *responsesStatefulRequestContext, dialerFactory func(*dbmodel.Channel) (*websocket.Dialer, error)) (*responsesWebsocketSession, error) {
	iter := balancer.NewIteratorWithOptions(group, c.GetInt("api_key_id"), requestModel, balancer.IteratorOptions{DisableSessionSticky: true})
	var lastErr error
	var sawResponsesChannel bool
	var matchedPinnedRoute bool

	for iter.Next() {
		item := iter.Item()
		if responsesStateful != nil && responsesStateful.HasPinnedRoute() && item.ChannelID == responsesStateful.PinnedRoute.ChannelID {
			matchedPinnedRoute = true
		}
		if responsesStateful != nil && responsesStateful.HasPinnedRoute() && item.ChannelID != responsesStateful.PinnedRoute.ChannelID {
			iter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("responses stateful route pinned to channel %d", responsesStateful.PinnedRoute.ChannelID))
			continue
		}
		channel, err := op.ChannelGet(item.ChannelID, c.Request.Context())
		if err != nil {
			iter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("channel not found: %v", err))
			lastErr = err
			continue
		}
		if !channel.Enabled {
			iter.Skip(channel.ID, 0, channel.Name, "channel disabled")
			continue
		}
		if channel.Type != outbound.OutboundTypeOpenAIResponse {
			iter.Skip(channel.ID, 0, channel.Name, "responses websocket requires same-protocol channel")
			continue
		}
		sawResponsesChannel = true

		selectedBaseURL := channel.GetBaseUrl()
		if responsesStateful != nil && responsesStateful.HasPinnedRoute() {
			pinnedBaseURL, ok := findPinnedBaseURL(channel, responsesStateful.PinnedRoute.BaseURL)
			if !ok {
				err = fmt.Errorf("responses stateful route pinned to base url %s but it is unavailable", responsesStateful.PinnedRoute.BaseURL)
				iter.Skip(channel.ID, responsesStateful.PinnedRoute.ChannelKeyID, channel.Name, err.Error())
				lastErr = err
				if responsesStateful.BlocksCrossRouteFailover() {
					break
				}
				continue
			}
			selectedBaseURL = pinnedBaseURL
		}
		if selectedBaseURL == "" {
			iter.Skip(channel.ID, 0, channel.Name, "no available base url")
			continue
		}

		usedKey := dbmodel.ChannelKey{}
		if responsesStateful != nil && responsesStateful.HasPinnedRoute() {
			pinnedKey, ok := findPinnedChannelKey(channel, responsesStateful.PinnedRoute.ChannelKeyID)
			if !ok {
				err = fmt.Errorf("responses stateful route pinned to channel key %d but it is unavailable", responsesStateful.PinnedRoute.ChannelKeyID)
				iter.Skip(channel.ID, responsesStateful.PinnedRoute.ChannelKeyID, channel.Name, err.Error())
				lastErr = err
				if responsesStateful.BlocksCrossRouteFailover() {
					break
				}
				continue
			}
			usedKey = pinnedKey
		}
		if usedKey.ChannelKey == "" {
			usedKey = channel.GetChannelKey()
		}
		if usedKey.ChannelKey == "" {
			iter.Skip(channel.ID, 0, channel.Name, "no available key")
			continue
		}
		if iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
			if responsesStateful != nil && responsesStateful.HasPinnedRoute() && responsesStateful.BlocksCrossRouteFailover() {
				lastErr = fmt.Errorf("responses stateful route is unavailable because the pinned route is circuit broken")
				break
			}
			continue
		}

		dialer, err := dialerFactory(channel)
		if err != nil {
			iter.Skip(channel.ID, usedKey.ID, channel.Name, err.Error())
			lastErr = err
			continue
		}

		upstreamURL, err := buildResponsesWebsocketURL(selectedBaseURL, c.Request.URL.RawQuery)
		if err != nil {
			iter.Skip(channel.ID, usedKey.ID, channel.Name, err.Error())
			lastErr = err
			continue
		}

		headers := buildResponsesWebsocketHeaders(c.Request.Header, usedKey.ChannelKey, channel.CustomHeader)
		span := iter.StartAttempt(channel.ID, usedKey.ID, channel.Name)
		metrics.RecordExecutionTrace(fmt.Sprintf("ws_connect_attempt: channel=%s upstream_model=%s base_url=%s key_id=%d", channel.Name, item.ModelName, normalizeAffinityBaseURL(selectedBaseURL), usedKey.ID))

		upstreamConn, response, err := dialer.DialContext(ctx, upstreamURL, headers)
		if err != nil {
			statusCode := 0
			message := err.Error()
			if response != nil {
				statusCode = response.StatusCode
				if response.Body != nil {
					body, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
					_ = response.Body.Close()
					if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
						message = fmt.Sprintf("%s; body=%s", message, trimmed)
					}
				}
			}
			span.End(dbmodel.AttemptFailed, statusCode, message)
			balancer.RecordFailure(channel.ID, usedKey.ID, requestModel)
			op.StatsChannelUpdate(channel.ID, dbmodel.StatsMetrics{RequestFailed: 1})
			usedKey.StatusCode = statusCode
			usedKey.LastUseTimeStamp = time.Now().Unix()
			op.ChannelKeyUpdate(usedKey)
			lastErr = fmt.Errorf("channel %s failed: %s", channel.Name, message)
			continue
		}

		return &responsesWebsocketSession{
			groupID:           group.ID,
			channel:           channel,
			usedKey:           usedKey,
			selectedBaseURL:   selectedBaseURL,
			upstreamConn:      upstreamConn,
			upstreamModel:     item.ModelName,
			maxLifetime:       channel.GetResponsesWebsocketMaxLifetime(),
			responsesStateful: responsesStateful,
			iter:              iter,
			span:              span,
		}, nil
	}

	if responsesStateful != nil && responsesStateful.HasPinnedRoute() && responsesStateful.BlocksCrossRouteFailover() && !matchedPinnedRoute {
		lastErr = fmt.Errorf("responses stateful route pinned to channel %d but no matching route candidate is available", responsesStateful.PinnedRoute.ChannelID)
	} else if !sawResponsesChannel {
		lastErr = fmt.Errorf("no same-protocol channel for responses websocket")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no available channel")
	}
	return &responsesWebsocketSession{iter: iter}, lastErr
}

func proxyResponsesWebsocket(clientConn *websocket.Conn, session *responsesWebsocketSession, metrics *RelayMetrics) error {
	if clientConn == nil || session == nil || session.upstreamConn == nil {
		return fmt.Errorf("websocket connection is nil")
	}
	upstreamConn := session.upstreamConn

	applyResponsesWebsocketReadDeadline(clientConn)
	applyResponsesWebsocketReadDeadline(upstreamConn)
	clientConn.SetPongHandler(func(string) error {
		applyResponsesWebsocketReadDeadline(clientConn)
		return nil
	})
	upstreamConn.SetPongHandler(func(string) error {
		applyResponsesWebsocketReadDeadline(upstreamConn)
		return nil
	})

	type websocketRelayResult struct {
		source string
		err    error
	}

	resultCh := make(chan websocketRelayResult, 2)
	go func() {
		resultCh <- websocketRelayResult{source: "client_to_upstream", err: relayWebsocketMessages(clientConn, upstreamConn, nil)}
	}()
	go func() {
		resultCh <- websocketRelayResult{source: "upstream_to_client", err: relayWebsocketMessages(upstreamConn, clientConn, func(messageType int, payload []byte) {
			now := time.Now()
			if metrics != nil {
				metrics.RecordUpstreamEvent(now)
				metrics.RecordClientChunk(now, payload)
				if metrics.FirstTokenTime.IsZero() {
					metrics.SetFirstTokenTime(now)
				}
				if messageType == websocket.TextMessage {
					eventType := detectResponsesWebsocketEventType(payload)
					metrics.RecordUpstreamEventType(eventType)
					if session.responsesStateful != nil {
						observeResponsesAffinityFromPayload(session.groupID, metrics.RequestModel, session.responsesStateful, payload)
					}
					if isResponsesWebsocketTerminalEvent(eventType) {
						metrics.MarkTerminalSeen()
					}
				}
			}
		})}
	}()

	var stopPing chan struct{}
	var pingWG sync.WaitGroup
	if relayStreamIdleTimeout > 0 {
		stopPing = make(chan struct{})
		pingWG.Add(2)
		go func() {
			defer pingWG.Done()
			_ = sendResponsesWebsocketPingLoop(clientConn, stopPing)
		}()
		go func() {
			defer pingWG.Done()
			_ = sendResponsesWebsocketPingLoop(upstreamConn, stopPing)
		}()
	}

	var lifetimeReached atomic.Bool
	var lifetimeTimer *time.Timer
	if session.maxLifetime > 0 {
		lifetimeTimer = time.AfterFunc(session.maxLifetime, func() {
			lifetimeReached.Store(true)
			reason := fmt.Sprintf("responses websocket max lifetime reached after %s", formatRelayTimeout(session.maxLifetime))
			writeWebsocketClose(clientConn, websocket.CloseGoingAway, reason)
			writeWebsocketClose(upstreamConn, websocket.CloseGoingAway, reason)
			_ = clientConn.Close()
			_ = upstreamConn.Close()
		})
	}

	firstResult := <-resultCh
	if lifetimeTimer != nil {
		lifetimeTimer.Stop()
	}
	if stopPing != nil {
		close(stopPing)
	}
	_ = clientConn.Close()
	_ = upstreamConn.Close()
	<-resultCh
	if stopPing != nil {
		pingWG.Wait()
	}
	return evaluateResponsesWebsocketProxyResult(firstResult.source, firstResult.err, metrics, lifetimeReached.Load())
}

func relayWebsocketMessages(src, dst *websocket.Conn, beforeWrite func(messageType int, payload []byte)) error {
	for {
		messageType, payload, err := src.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok {
				writeWebsocketClose(dst, closeErr.Code, closeErr.Text)
			}
			return &responsesWebsocketRelayIOError{Op: "read", Err: normalizeResponsesWebsocketReadError(err)}
		}
		applyResponsesWebsocketReadDeadline(src)
		if beforeWrite != nil {
			beforeWrite(messageType, payload)
		}
		if err := writeResponsesWebsocketMessage(dst, messageType, payload); err != nil {
			return &responsesWebsocketRelayIOError{Op: "write", Err: err}
		}
	}
}

func writeResponsesWebsocketMessage(conn *websocket.Conn, messageType int, payload []byte) error {
	if conn == nil {
		return fmt.Errorf("websocket connection is nil")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(websocketWriteTimeout)); err != nil {
		return err
	}
	if err := conn.WriteMessage(messageType, payload); err != nil {
		return err
	}
	return conn.SetWriteDeadline(time.Time{})
}

func sendResponsesWebsocketPingLoop(conn *websocket.Conn, stop <-chan struct{}) error {
	if conn == nil || relayStreamIdleTimeout <= 0 {
		return nil
	}
	interval := relayStreamIdleTimeout / 2
	if interval <= 0 {
		interval = time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			deadline := time.Now().Add(websocketWriteTimeout)
			if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				return err
			}
		}
	}
}

func applyResponsesWebsocketReadDeadline(conn *websocket.Conn) {
	if conn == nil || relayStreamIdleTimeout <= 0 {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(relayStreamIdleTimeout))
}

func normalizeResponsesWebsocketReadError(err error) error {
	if err == nil {
		return nil
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("websocket idle timeout after %s", formatRelayTimeout(relayStreamIdleTimeout))
	}
	return err
}

func isExpectedWebsocketIdleTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "websocket idle timeout")
}

func evaluateResponsesWebsocketProxyResult(source string, err error, metrics *RelayMetrics, lifetimeReached bool) error {
	if lifetimeReached {
		return nil
	}
	if err == nil {
		return nil
	}

	var ioErr *responsesWebsocketRelayIOError
	if errors.As(err, &ioErr) {
		switch {
		case source == "client_to_upstream" && ioErr.Op == "read":
			if isResponsesWebsocketClientDisconnect(ioErr.Err) {
				return nil
			}
		case source == "upstream_to_client" && ioErr.Op == "write":
			if isResponsesWebsocketClientDisconnect(ioErr.Err) {
				return nil
			}
		case source == "upstream_to_client" && ioErr.Op == "read":
			if responsesWebsocketSawTerminal(metrics) && (isExpectedWebsocketClose(ioErr.Err) || isUnexpectedResponsesWebsocketClose(ioErr.Err)) {
				return nil
			}
			if !responsesWebsocketSawTerminal(metrics) && isUnexpectedResponsesWebsocketClose(ioErr.Err) {
				return fmt.Errorf("upstream websocket closed before terminal event")
			}
		}
	}

	return err
}

func responsesWebsocketSawTerminal(metrics *RelayMetrics) bool {
	return metrics != nil && metrics.TerminalSeen
}

func isUnexpectedResponsesWebsocketClose(err error) bool {
	if err == nil {
		return false
	}
	if isExpectedWebsocketIdleTimeout(err) {
		return false
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
		return true
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "unexpected eof") || strings.Contains(msg, "abnormal closure")
}

func isResponsesWebsocketClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if isExpectedWebsocketIdleTimeout(err) || isUnexpectedResponsesWebsocketClose(err) || isExpectedWebsocketClose(err) {
		return true
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "forcibly closed")
}

func isResponsesWebsocketTerminalEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	default:
		return false
	}
}

func detectResponsesWebsocketEventType(payload []byte) string {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return "text"
	}
	if strings.TrimSpace(event.Type) == "" {
		return "text"
	}
	return strings.TrimSpace(event.Type)
}

func buildResponsesWebsocketURL(baseURL, rawQuery string) (string, error) {
	parsedURL, err := httpToWebsocketURL(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	parsedURL.Path = strings.TrimRight(parsedURL.Path, "/") + "/responses"
	parsedURL.RawQuery = rawQuery
	return parsedURL.String(), nil
}

func httpToWebsocketURL(rawURL string) (*url.URL, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse base url: %w", err)
	}
	switch parsedURL.Scheme {
	case "http":
		parsedURL.Scheme = "ws"
	case "https":
		parsedURL.Scheme = "wss"
	case "ws", "wss":
	default:
		return nil, fmt.Errorf("unsupported websocket scheme: %s", parsedURL.Scheme)
	}
	return parsedURL, nil
}

func buildResponsesWebsocketHeaders(source http.Header, key string, customHeaders []dbmodel.CustomHeader) http.Header {
	headers := http.Header{}
	for headerKey, values := range source {
		if shouldSkipWebsocketHeader(headerKey) {
			continue
		}
		for _, value := range values {
			headers.Add(headerKey, value)
		}
	}
	headers.Set("Authorization", "Bearer "+key)
	for _, header := range customHeaders {
		headers.Set(header.HeaderKey, header.HeaderValue)
	}
	return headers
}

func shouldSkipWebsocketHeader(headerKey string) bool {
	lower := strings.ToLower(strings.TrimSpace(headerKey))
	if lower == "" {
		return true
	}
	if hopByHopHeaders[lower] {
		return true
	}
	if lower == "authorization" || lower == "x-api-key" || lower == "sec-websocket-key" || lower == "sec-websocket-version" || lower == "sec-websocket-extensions" || lower == "sec-websocket-accept" || lower == "sec-websocket-protocol" {
		return true
	}
	return strings.HasPrefix(lower, "sec-websocket-")
}

func containsTrimmedString(values []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}

func isExpectedWebsocketClose(err error) bool {
	if err == nil {
		return true
	}
	return websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway)
}

func writeWebsocketClose(conn *websocket.Conn, code int, message string) {
	if conn == nil {
		return
	}
	deadline := time.Now().Add(time.Second)
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, truncateWebsocketCloseReason(message)), deadline)
}

func truncateWebsocketCloseReason(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= websocketCloseReasonLimit {
		return message
	}
	return message[:websocketCloseReasonLimit]
}
