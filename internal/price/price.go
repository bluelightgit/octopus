package price

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bluelightgit/octopus/internal/client"
	"github.com/bluelightgit/octopus/internal/model"
	"github.com/bluelightgit/octopus/internal/op"
	"github.com/bluelightgit/octopus/internal/utils/log"
)

const llmPriceURL = "https://models.dev/api.json"

const llmPriceUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"

var getHTTPClientSystemProxy = client.GetHTTPClientSystemProxy

var Provider = []string{
	"openai",     // GPT 系列
	"anthropic",  // Claude 系列
	"google",     // Gemini 系列
	"deepseek",   // DeepSeek 系列
	"xai",        // Grok 系列
	"alibaba",    // Qwen 系列
	"zhipuai",    // GLM 系列
	"minimax",    // MiniMax 系列
	"moonshotai", // Kimi/Moonshot
	"v0",         // v0 系列
	"xiaomi",     // MiMo 系列
}

// developerFamilies limits automatic prices to first-party text models. The
// models.dev catalog also contains hosted third-party and embedding models,
// which must not be used to price arbitrary relay requests.
var developerFamilies = map[string][]string{
	"openai":     {"gpt", "o"},
	"anthropic":  {"claude"},
	"google":     {"gemini", "gemma", "lyria", "veo"},
	"deepseek":   {"deepseek"},
	"xai":        {"grok"},
	"alibaba":    {"qwen", "qvq"},
	"zhipuai":    {"glm"},
	"minimax":    {"minimax"},
	"moonshotai": {"kimi"},
	"v0":         {"v0"},
	"xiaomi":     {"mimo"},
}

var lastUpdateTime time.Time

func UpdateLLMPrice(ctx context.Context) error {
	log.Debugf("update LLM price task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("update LLM price task finished, update time: %s", time.Since(startTime))
	}()
	body, err := fetchLLMPriceBody(ctx)
	if err != nil {
		return err
	}
	var rawPrice map[string]struct {
		Models map[string]struct {
			ID         string `json:"id"`
			Family     string `json:"family"`
			Modalities struct {
				Output []string `json:"output"`
			} `json:"modalities"`
			Cost model.LLMPrice `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &rawPrice); err != nil {
		return fmt.Errorf("failed to parse LLM info: %w", err)
	}
	updatedPrices := make(map[string]model.LLMPrice)
	for provider, familyPrefixes := range developerFamilies {
		for _, priceModel := range rawPrice[provider].Models {
			modelID := strings.ToLower(strings.TrimSpace(priceModel.ID))
			modelFamily := strings.ToLower(strings.TrimSpace(priceModel.Family))
			if modelID == "" || !slices.ContainsFunc(priceModel.Modalities.Output, func(output string) bool {
				return strings.EqualFold(strings.TrimSpace(output), "text")
			}) || strings.Contains(modelID, "embed") || strings.Contains(modelFamily, "embed") {
				continue
			}

			isDeveloperModel := false
			for _, familyPrefix := range familyPrefixes {
				if strings.HasPrefix(modelFamily, familyPrefix) {
					isDeveloperModel = true
					break
				}
			}
			if !isDeveloperModel {
				continue
			}
			updatedPrices[modelID] = priceModel.Cost
		}
	}
	llmPriceLock.Lock()
	llmPrice = updatedPrices
	lastUpdateTime = time.Now()
	llmPriceLock.Unlock()
	return nil
}

func fetchLLMPriceBody(ctx context.Context) ([]byte, error) {
	directClient, err := getHTTPClientSystemProxy(false)
	if err == nil {
		body, requestErr := requestLLMPriceBody(ctx, directClient)
		if requestErr == nil {
			return body, nil
		}
		err = requestErr
	}
	if ctx.Err() != nil {
		return nil, err
	}

	log.Warnf("direct request failed, trying with proxy: %v", err)
	proxyClient, proxyErr := getHTTPClientSystemProxy(true)
	if proxyErr != nil {
		return nil, fmt.Errorf("direct request failed: %v; proxy client unavailable: %w", err, proxyErr)
	}
	return requestLLMPriceBody(ctx, proxyClient)
}

func requestLLMPriceBody(ctx context.Context, httpClient *http.Client) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, llmPriceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", llmPriceUserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch LLM info: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	return body, nil
}

func GetLastUpdateTime() time.Time {
	llmPriceLock.RLock()
	defer llmPriceLock.RUnlock()
	return lastUpdateTime
}

func GetLLMPrice(modelName string) *model.LLMPrice {
	modelName = strings.ToLower(strings.TrimSpace(modelName))
	if modelName == "" {
		return nil
	}
	customPrice, err := op.LLMGet(modelName)
	if err == nil {
		return &customPrice
	}
	return GetCatalogLLMPrice(modelName)
}

// GetCatalogLLMPrice resolves only the bundled/remote catalog and intentionally
// ignores manually stored database prices. It is used when rebuilding prices.
func GetCatalogLLMPrice(modelName string) *model.LLMPrice {
	modelName = strings.ToLower(strings.TrimSpace(modelName))
	if modelName == "" {
		return nil
	}

	llmPriceLock.RLock()
	defer llmPriceLock.RUnlock()
	if price, ok := llmPrice[modelName]; ok {
		return &price
	}

	// Treat non-alphanumeric characters (except dots) as boundaries so a
	// provider-specific suffix/prefix can still resolve to a catalog model.
	modelNameSegments := strings.FieldsFunc(modelName, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.'
	})

	matchedModelID := ""
	matchedSegmentCount := 0
	ambiguous := false
	for modelID, candidatePrice := range llmPrice {
		modelIDSegments := strings.Split(modelID, "-")
		for start := 0; start+len(modelIDSegments) <= len(modelNameSegments); start++ {
			matched := true
			for i, segment := range modelIDSegments {
				if modelNameSegments[start+i] != segment {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			if len(modelIDSegments) > matchedSegmentCount {
				matchedModelID = modelID
				matchedSegmentCount = len(modelIDSegments)
				ambiguous = false
			} else if len(modelIDSegments) == matchedSegmentCount && llmPrice[matchedModelID] != candidatePrice {
				ambiguous = true
			}
			break
		}
	}
	if matchedModelID == "" || ambiguous {
		return nil
	}
	price := llmPrice[matchedModelID]
	return &price
}
