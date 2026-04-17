package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

const websocketCloseReasonLimit = 120

type responsesWebsocketSession struct {
	channel         *dbmodel.Channel
	usedKey         dbmodel.ChannelKey
	selectedBaseURL string
	upstreamConn    *websocket.Conn
	upstreamModel   string
	iter            *balancer.Iterator
	span            *balancer.AttemptSpan
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

	session, err := establishResponsesWebsocketSession(c.Request.Context(), c, group, requestModel, metrics)
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

	metrics.RecordExecutionTrace(fmt.Sprintf("ws_connected: channel=%s upstream_model=%s base_url=%s key_id=%d", session.channel.Name, session.upstreamModel, normalizeAffinityBaseURL(session.selectedBaseURL), session.usedKey.ID))

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

	err = proxyResponsesWebsocket(clientConn, session.upstreamConn, metrics)
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

func establishResponsesWebsocketSession(ctx context.Context, c *gin.Context, group dbmodel.Group, requestModel string, metrics *RelayMetrics) (*responsesWebsocketSession, error) {
	iter := balancer.NewIteratorWithOptions(group, c.GetInt("api_key_id"), requestModel, balancer.IteratorOptions{DisableSessionSticky: true})
	var lastErr error
	var sawResponsesChannel bool

	for iter.Next() {
		item := iter.Item()
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
		if selectedBaseURL == "" {
			iter.Skip(channel.ID, 0, channel.Name, "no available base url")
			continue
		}

		usedKey := channel.GetChannelKey()
		if usedKey.ChannelKey == "" {
			iter.Skip(channel.ID, 0, channel.Name, "no available key")
			continue
		}
		if iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
			continue
		}

		dialer, err := helper.ChannelWebsocketDialer(channel)
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
			channel:         channel,
			usedKey:         usedKey,
			selectedBaseURL: selectedBaseURL,
			upstreamConn:    upstreamConn,
			upstreamModel:   item.ModelName,
			iter:            iter,
			span:            span,
		}, nil
	}

	if !sawResponsesChannel {
		lastErr = fmt.Errorf("no same-protocol channel for responses websocket")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no available channel")
	}
	return &responsesWebsocketSession{iter: iter}, lastErr
}

func proxyResponsesWebsocket(clientConn, upstreamConn *websocket.Conn, metrics *RelayMetrics) error {
	if clientConn == nil || upstreamConn == nil {
		return fmt.Errorf("websocket connection is nil")
	}

	resultCh := make(chan error, 2)
	go func() {
		resultCh <- relayWebsocketMessages(clientConn, upstreamConn, nil)
	}()
	go func() {
		resultCh <- relayWebsocketMessages(upstreamConn, clientConn, func(messageType int, payload []byte) {
			now := time.Now()
			if metrics != nil {
				metrics.RecordUpstreamEvent(now)
				metrics.RecordClientChunk(now, payload)
				if metrics.FirstTokenTime.IsZero() {
					metrics.SetFirstTokenTime(now)
				}
				if messageType == websocket.TextMessage {
					metrics.RecordUpstreamEventType(detectResponsesWebsocketEventType(payload))
				}
			}
		})
	}()

	firstErr := <-resultCh
	_ = clientConn.Close()
	_ = upstreamConn.Close()
	<-resultCh
	if isExpectedWebsocketClose(firstErr) {
		return nil
	}
	return firstErr
}

func relayWebsocketMessages(src, dst *websocket.Conn, beforeWrite func(messageType int, payload []byte)) error {
	for {
		messageType, payload, err := src.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok {
				writeWebsocketClose(dst, closeErr.Code, closeErr.Text)
			}
			return err
		}
		if beforeWrite != nil {
			beforeWrite(messageType, payload)
		}
		if err := dst.WriteMessage(messageType, payload); err != nil {
			return err
		}
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
