package relay

import (
	"strings"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
)

type affinityRoute struct {
	ChannelID    int
	ChannelKeyID int
	BaseURL      string
	Timestamp    time.Time
}

func normalizeAffinityBaseURL(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return ""
	}
	return strings.TrimRight(trimmed, "/")
}

func findPinnedBaseURL(channel *dbmodel.Channel, expected string) (string, bool) {
	if channel == nil {
		return "", false
	}
	expected = normalizeAffinityBaseURL(expected)
	if expected == "" {
		baseURL := strings.TrimSpace(channel.GetBaseUrl())
		return baseURL, baseURL != ""
	}

	for _, item := range channel.BaseUrls {
		if normalizeAffinityBaseURL(item.URL) == expected {
			return item.URL, true
		}
	}
	return "", false
}

func findPinnedChannelKey(channel *dbmodel.Channel, keyID int) (dbmodel.ChannelKey, bool) {
	if channel == nil {
		return dbmodel.ChannelKey{}, false
	}
	if keyID == 0 {
		key := channel.GetChannelKey()
		if key.ChannelKey == "" {
			return dbmodel.ChannelKey{}, false
		}
		return key, true
	}

	nowSec := time.Now().Unix()
	for _, key := range channel.Keys {
		if key.ID != keyID || !key.Enabled || key.ChannelKey == "" {
			continue
		}
		if key.StatusCode == 429 && key.LastUseTimeStamp > 0 && nowSec-key.LastUseTimeStamp < int64(5*time.Minute/time.Second) {
			return dbmodel.ChannelKey{}, false
		}
		return key, true
	}
	return dbmodel.ChannelKey{}, false
}
