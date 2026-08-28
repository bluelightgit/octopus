package model

type GroupMode int

type GroupProtocolFamily string

type GroupProtocolRoutingMode string

type GroupRouteAffinityMode string

type GroupResponsesStatefulRoutingMode = GroupRouteAffinityMode

const (
	GroupModeRoundRobin GroupMode = 1 // 轮询：依次循环选择渠道
	GroupModeRandom     GroupMode = 2 // 随机：每次随机选择一个渠道
	GroupModeFailover   GroupMode = 3 // 故障转移：按优先级选择，失败时降级到下一个
	GroupModeWeighted   GroupMode = 4 // 加权分配：按优权重分配流量
)

const (
	GroupProtocolFamilyAuto              GroupProtocolFamily = "auto"
	GroupProtocolFamilyOpenAIChat        GroupProtocolFamily = "openai_chat"
	GroupProtocolFamilyOpenAIResponses   GroupProtocolFamily = "openai_responses"
	GroupProtocolFamilyAnthropicMessages GroupProtocolFamily = "anthropic_messages"
	GroupProtocolFamilyGeminiContents    GroupProtocolFamily = "gemini_contents"
)

const (
	GroupProtocolRoutingModePreferSameProtocol GroupProtocolRoutingMode = "prefer_same_protocol"
	GroupProtocolRoutingModeSameProtocolOnly   GroupProtocolRoutingMode = "same_protocol_only"
	GroupProtocolRoutingModeAllowCrossProtocol GroupProtocolRoutingMode = "allow_cross_protocol"
)

const (
	GroupRouteAffinityModeOff    GroupRouteAffinityMode = "off"
	GroupRouteAffinityModeAuto   GroupRouteAffinityMode = "auto"
	GroupRouteAffinityModeStrict GroupRouteAffinityMode = "strict"
)

const (
	GroupResponsesStatefulRoutingModeOff    = GroupRouteAffinityModeOff
	GroupResponsesStatefulRoutingModeAuto   = GroupRouteAffinityModeAuto
	GroupResponsesStatefulRoutingModeStrict = GroupRouteAffinityModeStrict
)

type Group struct {
	ID                        int                               `json:"id" gorm:"primaryKey"`
	Name                      string                            `json:"name" gorm:"unique;not null"`
	Mode                      GroupMode                         `json:"mode" gorm:"not null"`
	MatchRegex                string                            `json:"match_regex"`
	FirstTokenTimeOut         int                               `json:"first_token_time_out"`
	SessionKeepTime           int                               `json:"session_keep_time"`
	PreferredProtocolFamily   GroupProtocolFamily               `json:"preferred_protocol_family" gorm:"default:auto"`
	ProtocolRoutingMode       GroupProtocolRoutingMode          `json:"protocol_routing_mode" gorm:"default:prefer_same_protocol"`
	RouteAffinityMode         GroupRouteAffinityMode            `json:"route_affinity_mode" gorm:"column:responses_stateful_routing;default:auto"`
	ResponsesStatefulRouting  GroupResponsesStatefulRoutingMode `json:"responses_stateful_routing,omitempty" gorm:"-"`
	ResponsesWebsocketEnabled bool                              `json:"responses_websocket_enabled" gorm:"default:false"`
	Items                     []GroupItem                       `json:"items,omitempty" gorm:"foreignKey:GroupID"`
}

type GroupItem struct {
	ID        int    `json:"id" gorm:"primaryKey"`
	GroupID   int    `json:"group_id" gorm:"not null;index:idx_group_channel_model,unique"`
	ChannelID int    `json:"channel_id" gorm:"not null;index:idx_group_channel_model,unique"`
	ModelName string `json:"model_name" gorm:"not null;index:idx_group_channel_model,unique"`
	Priority  int    `json:"priority"`
	Weight    int    `json:"weight"`
}

type GroupUpdateRequest struct {
	ID                        int                                `json:"id" binding:"required"`
	Name                      *string                            `json:"name,omitempty"`
	Mode                      *GroupMode                         `json:"mode,omitempty"`
	MatchRegex                *string                            `json:"match_regex,omitempty"`
	FirstTokenTimeOut         *int                               `json:"first_token_time_out,omitempty"`
	SessionKeepTime           *int                               `json:"session_keep_time,omitempty"`
	PreferredProtocolFamily   *GroupProtocolFamily               `json:"preferred_protocol_family,omitempty"`
	ProtocolRoutingMode       *GroupProtocolRoutingMode          `json:"protocol_routing_mode,omitempty"`
	RouteAffinityMode         *GroupRouteAffinityMode            `json:"route_affinity_mode,omitempty"`
	ResponsesStatefulRouting  *GroupResponsesStatefulRoutingMode `json:"responses_stateful_routing,omitempty"`
	ResponsesWebsocketEnabled *bool                              `json:"responses_websocket_enabled,omitempty"`
	ItemsToAdd                []GroupItemAddRequest              `json:"items_to_add,omitempty"`
	ItemsToUpdate             []GroupItemUpdateRequest           `json:"items_to_update,omitempty"`
	ItemsToDelete             []int                              `json:"items_to_delete,omitempty"`
}

type GroupItemAddRequest struct {
	ChannelID int    `json:"channel_id" binding:"required"`
	ModelName string `json:"model_name" binding:"required"`
	Priority  int    `json:"priority,omitempty"`
	Weight    int    `json:"weight,omitempty"`
}

type GroupItemUpdateRequest struct {
	ID       int `json:"id" binding:"required"`
	Priority int `json:"priority,omitempty"`
	Weight   int `json:"weight,omitempty"`
}

type GroupIDAndLLMName struct {
	ChannelID int
	ModelName string
}

func (g Group) GetPreferredProtocolFamily() GroupProtocolFamily {
	switch g.PreferredProtocolFamily {
	case GroupProtocolFamilyOpenAIChat, GroupProtocolFamilyOpenAIResponses, GroupProtocolFamilyAnthropicMessages, GroupProtocolFamilyGeminiContents:
		return g.PreferredProtocolFamily
	default:
		return GroupProtocolFamilyAuto
	}
}

func (g Group) GetProtocolRoutingMode() GroupProtocolRoutingMode {
	switch g.ProtocolRoutingMode {
	case GroupProtocolRoutingModeSameProtocolOnly, GroupProtocolRoutingModeAllowCrossProtocol:
		return g.ProtocolRoutingMode
	default:
		return GroupProtocolRoutingModePreferSameProtocol
	}
}

func NormalizeGroupRouteAffinityMode(v GroupRouteAffinityMode) GroupRouteAffinityMode {
	switch v {
	case GroupRouteAffinityModeOff, GroupRouteAffinityModeStrict:
		return v
	default:
		return GroupRouteAffinityModeAuto
	}
}

func ResolveGroupRouteAffinityMode(primary GroupRouteAffinityMode, legacy GroupResponsesStatefulRoutingMode) GroupRouteAffinityMode {
	if primary != "" {
		return NormalizeGroupRouteAffinityMode(primary)
	}
	return NormalizeGroupRouteAffinityMode(GroupRouteAffinityMode(legacy))
}

func (g Group) GetRouteAffinityMode() GroupRouteAffinityMode {
	return ResolveGroupRouteAffinityMode(g.RouteAffinityMode, g.ResponsesStatefulRouting)
}

func (g Group) GetResponsesStatefulRoutingMode() GroupResponsesStatefulRoutingMode {
	return g.GetRouteAffinityMode()
}

func (g Group) GetResponsesWebsocketEnabled() bool {
	return g.GetPreferredProtocolFamily() == GroupProtocolFamilyOpenAIResponses && g.ResponsesWebsocketEnabled
}
