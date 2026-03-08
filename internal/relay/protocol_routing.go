package relay

import (
	"fmt"
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

type protocolRoutePlan struct {
	Primary              []dbmodel.GroupItem
	Fallback             []dbmodel.GroupItem
	ResolvedClientFamily dbmodel.GroupProtocolFamily
	ResolvedGroupFamily  dbmodel.GroupProtocolFamily
}

func buildProtocolRoutePlan(group dbmodel.Group, request *model.InternalLLMRequest) (protocolRoutePlan, error) {
	plan := protocolRoutePlan{}
	if len(group.Items) == 0 {
		return plan, nil
	}

	clientFamily, ok := requestProtocolFamily(request)
	if !ok || request.IsEmbeddingRequest() {
		plan.Primary = append(plan.Primary, group.Items...)
		return plan, nil
	}
	plan.ResolvedClientFamily = clientFamily

	groupFamily := resolveGroupProtocolFamily(group)
	plan.ResolvedGroupFamily = groupFamily
	mode := group.GetProtocolRoutingMode()

	if groupFamily != dbmodel.GroupProtocolFamilyAuto && groupFamily != clientFamily {
		if mode == dbmodel.GroupProtocolRoutingModeSameProtocolOnly {
			return plan, fmt.Errorf("group requires same-protocol routing, but client protocol does not match group protocol")
		}
		plan.Primary = append(plan.Primary, group.Items...)
		return plan, nil
	}

	sameProtocolItems := make([]dbmodel.GroupItem, 0, len(group.Items))
	crossProtocolItems := make([]dbmodel.GroupItem, 0, len(group.Items))
	for _, item := range group.Items {
		channel, err := op.ChannelGet(item.ChannelID, nil)
		if err != nil || channel == nil {
			crossProtocolItems = append(crossProtocolItems, item)
			continue
		}
		if outboundProtocolFamily(channel.Type) == clientFamily {
			sameProtocolItems = append(sameProtocolItems, item)
		} else {
			crossProtocolItems = append(crossProtocolItems, item)
		}
	}

	switch mode {
	case dbmodel.GroupProtocolRoutingModeSameProtocolOnly:
		plan.Primary = sameProtocolItems
		if len(plan.Primary) == 0 {
			return plan, fmt.Errorf("group requires same-protocol routing, but no matching channel is configured")
		}
	case dbmodel.GroupProtocolRoutingModeAllowCrossProtocol:
		plan.Primary = append(plan.Primary, group.Items...)
	default:
		if len(sameProtocolItems) > 0 {
			plan.Primary = sameProtocolItems
			plan.Fallback = crossProtocolItems
		} else {
			plan.Primary = append(plan.Primary, group.Items...)
		}
	}

	return plan, nil
}

func requestProtocolFamily(req *model.InternalLLMRequest) (dbmodel.GroupProtocolFamily, bool) {
	if req == nil {
		return "", false
	}
	if req.IsEmbeddingRequest() {
		return "", false
	}
	switch req.RawAPIFormat {
	case model.APIFormatOpenAIChatCompletion:
		return dbmodel.GroupProtocolFamilyOpenAIChat, true
	case model.APIFormatOpenAIResponse:
		return dbmodel.GroupProtocolFamilyOpenAIResponses, true
	case model.APIFormatAnthropicMessage:
		return dbmodel.GroupProtocolFamilyAnthropicMessages, true
	case model.APIFormatGeminiContents:
		return dbmodel.GroupProtocolFamilyGeminiContents, true
	default:
		return "", false
	}
}

func outboundProtocolFamily(channelType outbound.OutboundType) dbmodel.GroupProtocolFamily {
	switch channelType {
	case outbound.OutboundTypeOpenAIChat:
		return dbmodel.GroupProtocolFamilyOpenAIChat
	case outbound.OutboundTypeOpenAIResponse:
		return dbmodel.GroupProtocolFamilyOpenAIResponses
	case outbound.OutboundTypeAnthropic:
		return dbmodel.GroupProtocolFamilyAnthropicMessages
	case outbound.OutboundTypeGemini:
		return dbmodel.GroupProtocolFamilyGeminiContents
	default:
		return ""
	}
}

func resolveGroupProtocolFamily(group dbmodel.Group) dbmodel.GroupProtocolFamily {
	if preferred := group.GetPreferredProtocolFamily(); preferred != dbmodel.GroupProtocolFamilyAuto {
		return preferred
	}
	families := map[dbmodel.GroupProtocolFamily]struct{}{}
	for _, item := range group.Items {
		channel, err := op.ChannelGet(item.ChannelID, nil)
		if err != nil || channel == nil {
			continue
		}
		family := outboundProtocolFamily(channel.Type)
		if family == "" {
			continue
		}
		families[family] = struct{}{}
		if len(families) > 1 {
			return dbmodel.GroupProtocolFamilyAuto
		}
	}
	for family := range families {
		return family
	}
	return dbmodel.GroupProtocolFamilyAuto
}

func cloneGroupWithItems(group dbmodel.Group, items []dbmodel.GroupItem) dbmodel.Group {
	cloned := group
	cloned.Items = make([]dbmodel.GroupItem, len(items))
	copy(cloned.Items, items)
	return cloned
}

func protocolRoutePlanSummary(plan protocolRoutePlan) string {
	parts := make([]string, 0, 4)
	if plan.ResolvedClientFamily != "" {
		parts = append(parts, "client="+string(plan.ResolvedClientFamily))
	}
	if plan.ResolvedGroupFamily != "" {
		parts = append(parts, "group="+string(plan.ResolvedGroupFamily))
	}
	parts = append(parts, fmt.Sprintf("primary=%d", len(plan.Primary)))
	if len(plan.Fallback) > 0 {
		parts = append(parts, fmt.Sprintf("fallback=%d", len(plan.Fallback)))
	}
	return strings.Join(parts, ", ")
}
