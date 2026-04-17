package handlers

import (
	"net/http"
	"strconv"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/dlclark/regexp2"
	"github.com/gin-gonic/gin"
)

func isValidGroupProtocolFamily(v model.GroupProtocolFamily) bool {
	switch v {
	case "", model.GroupProtocolFamilyAuto, model.GroupProtocolFamilyOpenAIChat, model.GroupProtocolFamilyOpenAIResponses, model.GroupProtocolFamilyAnthropicMessages, model.GroupProtocolFamilyGeminiContents:
		return true
	default:
		return false
	}
}

func isValidGroupProtocolRoutingMode(v model.GroupProtocolRoutingMode) bool {
	switch v {
	case "", model.GroupProtocolRoutingModePreferSameProtocol, model.GroupProtocolRoutingModeSameProtocolOnly, model.GroupProtocolRoutingModeAllowCrossProtocol:
		return true
	default:
		return false
	}
}

func isValidGroupRouteAffinityMode(v model.GroupRouteAffinityMode) bool {
	switch v {
	case "", model.GroupRouteAffinityModeOff, model.GroupRouteAffinityModeAuto, model.GroupRouteAffinityModeStrict:
		return true
	default:
		return false
	}
}

func normalizeResponsesWebsocketEnabled(group *model.Group) {
	if group == nil {
		return
	}
	if group.GetPreferredProtocolFamily() != model.GroupProtocolFamilyOpenAIResponses {
		group.ResponsesWebsocketEnabled = false
	}
}

func normalizeResponsesWebsocketEnabledUpdate(req *model.GroupUpdateRequest, current model.Group) {
	if req == nil {
		return
	}
	nextPreferredProtocolFamily := current.GetPreferredProtocolFamily()
	if req.PreferredProtocolFamily != nil {
		nextPreferredProtocolFamily = *req.PreferredProtocolFamily
	}
	if nextPreferredProtocolFamily != model.GroupProtocolFamilyOpenAIResponses {
		disabled := false
		req.ResponsesWebsocketEnabled = &disabled
	}
}

func init() {
	router.NewGroupRouter("/api/v1/group").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(getGroupList),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createGroup),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateGroup),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteGroup),
		)
	// AddRoute(
	// 	router.NewRoute("/auto-add-item", http.MethodPost).
	// 		Handle(autoAddGroupItem),
	// )
}

func getGroupList(c *gin.Context) {
	groups, err := op.GroupList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, groups)
}

func createGroup(c *gin.Context) {
	var group model.Group
	if err := c.ShouldBindJSON(&group); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if !isValidGroupProtocolFamily(group.PreferredProtocolFamily) {
		resp.Error(c, http.StatusBadRequest, "invalid preferred_protocol_family")
		return
	}
	if !isValidGroupProtocolRoutingMode(group.ProtocolRoutingMode) {
		resp.Error(c, http.StatusBadRequest, "invalid protocol_routing_mode")
		return
	}
	if !isValidGroupRouteAffinityMode(model.ResolveGroupRouteAffinityMode(group.RouteAffinityMode, group.ResponsesStatefulRouting)) {
		resp.Error(c, http.StatusBadRequest, "invalid route_affinity_mode")
		return
	}
	normalizeResponsesWebsocketEnabled(&group)
	if group.MatchRegex != "" {
		_, err := regexp2.Compile(group.MatchRegex, regexp2.ECMAScript)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := op.GroupCreate(&group, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, group)
}

func updateGroup(c *gin.Context) {
	var req model.GroupUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.PreferredProtocolFamily != nil && !isValidGroupProtocolFamily(*req.PreferredProtocolFamily) {
		resp.Error(c, http.StatusBadRequest, "invalid preferred_protocol_family")
		return
	}
	if req.ProtocolRoutingMode != nil && !isValidGroupProtocolRoutingMode(*req.ProtocolRoutingMode) {
		resp.Error(c, http.StatusBadRequest, "invalid protocol_routing_mode")
		return
	}
	if req.RouteAffinityMode != nil && !isValidGroupRouteAffinityMode(*req.RouteAffinityMode) {
		resp.Error(c, http.StatusBadRequest, "invalid route_affinity_mode")
		return
	}
	if req.ResponsesStatefulRouting != nil && !isValidGroupRouteAffinityMode(model.GroupRouteAffinityMode(*req.ResponsesStatefulRouting)) {
		resp.Error(c, http.StatusBadRequest, "invalid responses_stateful_routing")
		return
	}
	if req.MatchRegex != nil {
		_, err := regexp2.Compile(*req.MatchRegex, regexp2.ECMAScript)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	currentGroup, err := op.GroupGet(req.ID, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	normalizeResponsesWebsocketEnabledUpdate(&req, *currentGroup)
	group, err := op.GroupUpdate(&req, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, group)
}

func deleteGroup(c *gin.Context) {
	id := c.Param("id")
	idNum, err := strconv.Atoi(id)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.GroupDel(idNum, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, "group deleted successfully")
}

// func autoAddGroupItem(c *gin.Context) {
// 	var req struct {
// 		ID int `json:"id"`
// 	}
// 	if err := c.ShouldBindJSON(&req); err != nil {
// 		resp.Error(c, http.StatusBadRequest, err.Error())
// 		return
// 	}
// 	if req.ID <= 0 {
// 		resp.Error(c, http.StatusBadRequest, "invalid id")
// 		return
// 	}
// 	err := worker.AutoAddGroupItem(req.ID, c.Request.Context())
// 	if err != nil {
// 		resp.Error(c, http.StatusInternalServerError, err.Error())
// 		return
// 	}
// 	resp.Success(c, nil)
// }
