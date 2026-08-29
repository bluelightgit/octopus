package client

import (
	"net/http"
	"sync"
	"testing"
)

func resetCustomProxyClients() {
	customProxyClients.Range(func(key, value any) bool {
		if client, ok := value.(*http.Client); ok {
			client.CloseIdleConnections()
		}
		customProxyClients.Delete(key)
		return true
	})
}

func TestGetHTTPClientCustomProxyReusesClient(t *testing.T) {
	resetCustomProxyClients()
	t.Cleanup(resetCustomProxyClients)

	first, err := GetHTTPClientCustomProxy("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("first client: %v", err)
	}
	second, err := GetHTTPClientCustomProxy("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("second client: %v", err)
	}
	if first != second {
		t.Fatal("expected the same cached client for the same proxy URL")
	}

	other, err := GetHTTPClientCustomProxy("http://127.0.0.1:2")
	if err != nil {
		t.Fatalf("other client: %v", err)
	}
	if first == other {
		t.Fatal("different proxy URLs must not share a client")
	}
}

func TestGetHTTPClientCustomProxyConcurrentLookupReturnsOneClient(t *testing.T) {
	resetCustomProxyClients()
	t.Cleanup(resetCustomProxyClients)

	const workers = 32
	clients := make([]*http.Client, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			clients[index], errs[index] = GetHTTPClientCustomProxy("http://127.0.0.1:3")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if clients[i] != clients[0] {
			t.Fatalf("worker %d received a different client", i)
		}
	}
}
