package handlers

import (
	"net/http"

	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/chat/completions", http.MethodPost).
				Handle(chat),
		).
		AddRoute(
			router.NewRoute("/completions", http.MethodPost).
				Handle(completions),
		).
		AddRoute(
			router.NewRoute("/responses", http.MethodPost).
				Handle(response),
		).
		AddRoute(
			router.NewRoute("/responses", http.MethodGet).
				Handle(responseWebsocket),
		).
		AddRoute(
			router.NewRoute("/messages", http.MethodPost).
				Handle(message),
		).
		AddRoute(
			router.NewRoute("/embeddings", http.MethodPost).
				Handle(embedding),
		)
}

func chat(c *gin.Context) {
	relay.Handler(inbound.InboundTypeOpenAIChat, c)
}

func completions(c *gin.Context) {
	relay.Handler(inbound.InboundTypeOpenAICompletions, c)
}

func response(c *gin.Context) {
	relay.Handler(inbound.InboundTypeOpenAIResponse, c)
}

func responseWebsocket(c *gin.Context) {
	relay.HandleResponsesWebsocket(c)
}

func message(c *gin.Context) {
	relay.Handler(inbound.InboundTypeAnthropic, c)
}

func embedding(c *gin.Context) {
	relay.Handler(inbound.InboundTypeOpenAIEmbedding, c)
}
