package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/log").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listLog),
		).
		AddRoute(
			router.NewRoute("/clear", http.MethodDelete).
				Handle(clearLog),
		).
		AddRoute(
			router.NewRoute("/stream-token", http.MethodGet).
				Handle(getStreamToken),
		).
		AddRoute(
			router.NewRoute("/:id/body", http.MethodGet).
				Handle(downloadLogBody),
		)

	router.NewGroupRouter("/api/v1/log").
		AddRoute(
			router.NewRoute("/stream", http.MethodGet).
				Handle(streamLog),
		)
}

func listLog(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	startTimeStr := c.Query("start_time")
	endTimeStr := c.Query("end_time")

	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	var startTime, endTime *int
	if startTimeStr != "" && endTimeStr != "" {
		st, err := strconv.Atoi(startTimeStr)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		et, err := strconv.Atoi(endTimeStr)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		startTime = &st
		endTime = &et
	}

	logs, err := op.RelayLogList(c.Request.Context(), startTime, endTime, page, pageSize)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	resp.Success(c, logs)
}

func clearLog(c *gin.Context) {
	if err := op.RelayLogClear(c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func getStreamToken(c *gin.Context) {
	token, err := op.RelayLogStreamTokenCreate()
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, gin.H{"token": token})
}

func downloadLogBody(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, "invalid relay log id")
		return
	}

	kind := strings.ToLower(strings.TrimSpace(c.Query("kind")))
	if kind != "request" && kind != "response" {
		resp.Error(c, http.StatusBadRequest, "body kind must be request or response")
		return
	}

	reader, relayLog, err := op.RelayLogBodyOpen(c.Request.Context(), id, kind)
	if err != nil {
		status := http.StatusInternalServerError
		message := "failed to open relay log body"
		switch {
		case errors.Is(err, op.ErrRelayLogNotFound), errors.Is(err, op.ErrRelayLogBodyNotFound):
			status = http.StatusNotFound
			message = "relay log body not found"
		case errors.Is(err, op.ErrRelayLogBodyKind):
			status = http.StatusBadRequest
			message = err.Error()
		}
		resp.Error(c, status, message)
		return
	}
	defer reader.Close()

	var size int64
	var sha256 string
	var encoding string
	var ref string
	if kind == "request" {
		size = relayLog.RequestBodySize
		sha256 = relayLog.RequestBodySHA256
		encoding = relayLog.RequestBodyEncoding
		ref = relayLog.RequestBodyRef
	} else {
		size = relayLog.ResponseBodySize
		sha256 = relayLog.ResponseBodySHA256
		encoding = relayLog.ResponseBodyEncoding
		ref = relayLog.ResponseBodyRef
	}
	if size <= 0 {
		size = -1
	}

	contentType := "application/octet-stream"
	if ref == "" && encoding == "utf8" {
		contentType = "text/plain; charset=utf-8"
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"octopus-log-%d-%s.body\"", id, kind))
	if sha256 != "" {
		c.Header("X-Content-SHA256", sha256)
	}
	c.DataFromReader(http.StatusOK, size, contentType, reader, nil)
}

func streamLog(c *gin.Context) {
	token := c.Query("token")
	if token == "" || !op.RelayLogStreamTokenVerify(token) {
		resp.Error(c, http.StatusUnauthorized, "invalid stream token")
		return
	}

	op.RelayLogStreamTokenRevoke(token)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	logChan := op.RelayLogSubscribe()
	defer op.RelayLogUnsubscribe(logChan)

	ctx := c.Request.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case log, ok := <-logChan:
			if !ok {
				return
			}
			data, err := json.Marshal(log)
			if err != nil {
				continue
			}
			c.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", data)))
			c.Writer.Flush()
		}
	}
}
