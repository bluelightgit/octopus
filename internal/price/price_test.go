package price

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bluelightgit/octopus/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestUpdateLLMPriceFallsBackToProxy(t *testing.T) {
	oldGetClient := getHTTPClientSystemProxy
	defer func() { getHTTPClientSystemProxy = oldGetClient }()

	var directCalls atomic.Int32
	var proxyCalls atomic.Int32
	directClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		directCalls.Add(1)
		return nil, errors.New("direct network unavailable")
	})}
	proxyClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		proxyCalls.Add(1)
		if got := req.Header.Get("User-Agent"); got != llmPriceUserAgent {
			return nil, errors.New("unexpected user agent")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(strings.NewReader(`{
				"openai": {"models": {"Fallback-Test-Model": {"id": "Fallback-Test-Model", "family": "gpt", "modalities": {"output": ["text"]}, "cost": {"input": 1.25, "output": 2.5}}}}
			}`)),
		}, nil
	})}
	getHTTPClientSystemProxy = func(useProxy bool) (*http.Client, error) {
		if useProxy {
			return proxyClient, nil
		}
		return directClient, nil
	}

	if err := UpdateLLMPrice(context.Background()); err != nil {
		t.Fatalf("UpdateLLMPrice() error = %v", err)
	}
	if got := directCalls.Load(); got != 1 {
		t.Fatalf("direct calls = %d, want 1", got)
	}
	if got := proxyCalls.Load(); got != 1 {
		t.Fatalf("proxy calls = %d, want 1", got)
	}

	llmPriceLock.RLock()
	got, ok := llmPrice["fallback-test-model"]
	llmPriceLock.RUnlock()
	if !ok {
		t.Fatal("fallback price was not stored")
	}
	if got.Input != 1.25 || got.Output != 2.5 {
		t.Fatalf("fallback price = %+v, want input=1.25 output=2.5", got)
	}
}

func TestUpdateLLMPriceDoesNotUseProxyAfterDirectSuccess(t *testing.T) {
	oldGetClient := getHTTPClientSystemProxy
	defer func() { getHTTPClientSystemProxy = oldGetClient }()

	var proxyCalls atomic.Int32
	directClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(strings.NewReader(`{
				"anthropic": {"models": {"Direct-Test-Model": {"id": "Direct-Test-Model", "family": "claude", "modalities": {"output": ["text"]}, "cost": {"input": 0.75, "output": 1.5}}}}
			}`)),
		}, nil
	})}
	proxyClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		proxyCalls.Add(1)
		return nil, errors.New("proxy should not be used")
	})}
	getHTTPClientSystemProxy = func(useProxy bool) (*http.Client, error) {
		if useProxy {
			return proxyClient, nil
		}
		return directClient, nil
	}

	if err := UpdateLLMPrice(context.Background()); err != nil {
		t.Fatalf("UpdateLLMPrice() error = %v", err)
	}
	if got := proxyCalls.Load(); got != 0 {
		t.Fatalf("proxy calls = %d, want 0", got)
	}
}

func TestUpdateLLMPriceKeepsOnlyFirstPartyTextModels(t *testing.T) {
	oldGetClient := getHTTPClientSystemProxy
	defer func() { getHTTPClientSystemProxy = oldGetClient }()
	oldPrices := llmPrice
	defer func() {
		llmPriceLock.Lock()
		llmPrice = oldPrices
		llmPriceLock.Unlock()
	}()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(strings.NewReader(`{
				"openai": {"models": {
					"good": {"id": "GPT-5.2", "family": "gpt-5", "modalities": {"output": ["text"]}, "cost": {"input": 1}},
					"embedding": {"id": "text-embedding-3-small", "family": "embedding", "modalities": {"output": ["embedding"]}, "cost": {"input": 2}},
					"hosted": {"id": "claude-3", "family": "claude", "modalities": {"output": ["text"]}, "cost": {"input": 3}}
				}}
			}`)),
		}, nil
	})}
	getHTTPClientSystemProxy = func(bool) (*http.Client, error) { return client, nil }

	if err := UpdateLLMPrice(context.Background()); err != nil {
		t.Fatalf("UpdateLLMPrice() error = %v", err)
	}

	llmPriceLock.RLock()
	_, good := llmPrice["gpt-5.2"]
	_, embedding := llmPrice["text-embedding-3-small"]
	_, hosted := llmPrice["claude-3"]
	llmPriceLock.RUnlock()
	if !good {
		t.Fatal("first-party text model was filtered out")
	}
	if embedding || hosted {
		t.Fatalf("unexpected non-first-party price entries: embedding=%t hosted=%t", embedding, hosted)
	}
}

func TestGetLLMPriceMatchesMostSpecificCatalogModel(t *testing.T) {
	oldPrices := llmPrice
	defer func() {
		llmPriceLock.Lock()
		llmPrice = oldPrices
		llmPriceLock.Unlock()
	}()

	llmPriceLock.Lock()
	llmPrice = map[string]model.LLMPrice{
		"gpt-4":    {Input: 1},
		"gpt-4o":   {Input: 2},
		"claude-3": {Input: 3},
	}
	llmPriceLock.Unlock()

	got := GetLLMPrice("provider/GPT-4o-mini")
	if got == nil || got.Input != 2 {
		t.Fatalf("matched price = %+v, want gpt-4o price", got)
	}
	if got = GetLLMPrice("unknown-model"); got != nil {
		t.Fatalf("unknown model matched price %+v", got)
	}
}

func TestFetchLLMPriceBodyReturnsProxyClientError(t *testing.T) {
	oldGetClient := getHTTPClientSystemProxy
	defer func() { getHTTPClientSystemProxy = oldGetClient }()

	directErr := errors.New("direct unavailable")
	proxyErr := errors.New("proxy setting invalid")
	getHTTPClientSystemProxy = func(useProxy bool) (*http.Client, error) {
		if useProxy {
			return nil, proxyErr
		}
		return nil, directErr
	}

	_, err := fetchLLMPriceBody(context.Background())
	if err == nil || !strings.Contains(err.Error(), proxyErr.Error()) || !strings.Contains(err.Error(), directErr.Error()) {
		t.Fatalf("fetchLLMPriceBody() error = %v, want direct and proxy errors", err)
	}
}
