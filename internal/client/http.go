package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluelightgit/octopus/internal/conf"
	"github.com/bluelightgit/octopus/internal/model"
	"github.com/bluelightgit/octopus/internal/op"
	"golang.org/x/net/proxy"
)

var (
	systemDirectClient    *http.Client
	systemProxyClient     *http.Client
	systemProxyURL        string
	clientLock            sync.RWMutex
	customProxyClients    sync.Map // proxy URL -> *http.Client
	responseHeaderTimeout = 30 * time.Second
)

func init() {
	if raw := strings.TrimSpace(os.Getenv(strings.ToUpper(conf.APP_NAME) + "_RELAY_UPSTREAM_HEADER_TIMEOUT_MS")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 0 {
			responseHeaderTimeout = time.Duration(v) * time.Millisecond
		}
	}
}

func ReloadRuntimeSettings() {
	timeout := responseHeaderTimeout
	if v, err := op.SettingGetInt(model.SettingKeyRelayUpstreamHeaderTimeoutMS); err == nil && v >= 0 {
		timeout = time.Duration(v) * time.Millisecond
	}

	clientLock.Lock()
	defer clientLock.Unlock()

	responseHeaderTimeout = timeout
	if systemDirectClient != nil {
		systemDirectClient.CloseIdleConnections()
	}
	if systemProxyClient != nil {
		systemProxyClient.CloseIdleConnections()
	}
	systemDirectClient = nil
	systemProxyClient = nil
	systemProxyURL = ""
	customProxyClients.Range(func(key, value any) bool {
		if client, ok := value.(*http.Client); ok {
			client.CloseIdleConnections()
		}
		customProxyClients.Delete(key)
		return true
	})
}

// GetHTTPClientSystemProxy returns a cached http.Client.
// - useProxy=false: bypass proxy
// - useProxy=true: use proxy settings from system/app settings (setting key: proxy_url)
func GetHTTPClientSystemProxy(useProxy bool) (*http.Client, error) {
	if useProxy {
		currentProxyURL, err := op.SettingGetString(model.SettingKeyProxyURL)
		if err != nil {
			return nil, err
		}
		if currentProxyURL == "" {
			return nil, fmt.Errorf("proxy url is empty")
		}

		clientLock.RLock()
		if systemProxyClient != nil && systemProxyURL == currentProxyURL {
			clientLock.RUnlock()
			return systemProxyClient, nil
		}
		clientLock.RUnlock()

		clientLock.Lock()
		defer clientLock.Unlock()

		// Re-check after acquiring write lock.
		if systemProxyClient != nil && systemProxyURL == currentProxyURL {
			return systemProxyClient, nil
		}

		client, err := newHTTPClientCustomProxy(currentProxyURL)
		if err != nil {
			return nil, err
		}
		if systemProxyClient != nil {
			systemProxyClient.CloseIdleConnections()
		}
		systemProxyClient = client
		systemProxyURL = currentProxyURL
		return systemProxyClient, nil
	}

	clientLock.RLock()
	if !useProxy && systemDirectClient != nil {
		clientLock.RUnlock()
		return systemDirectClient, nil
	}
	clientLock.RUnlock()

	clientLock.Lock()
	defer clientLock.Unlock()

	if systemDirectClient != nil {
		return systemDirectClient, nil
	}
	client, err := newHTTPClientNoProxy()
	if err != nil {
		return nil, err
	}
	systemDirectClient = client
	return systemDirectClient, nil
}

// GetHTTPClientCustomProxy returns a cached http.Client for each proxy URL.
// proxyURL supports: http, https, socks, socks5
func GetHTTPClientCustomProxy(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return nil, fmt.Errorf("proxy url is empty")
	}
	if cached, ok := customProxyClients.Load(proxyURL); ok {
		return cached.(*http.Client), nil
	}
	client, err := newHTTPClientCustomProxy(proxyURL)
	if err != nil {
		return nil, err
	}
	actual, loaded := customProxyClients.LoadOrStore(proxyURL, client)
	if loaded {
		client.CloseIdleConnections()
	}
	return actual.(*http.Client), nil
}

func clonedDefaultTransport() (*http.Transport, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default transport is not *http.Transport")
	}
	return transport.Clone(), nil
}

func newHTTPClientNoProxy() (*http.Client, error) {
	cloned, err := clonedDefaultTransport()
	if err != nil {
		return nil, err
	}
	cloned.Proxy = nil
	cloned.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: cloned}, nil
}

func newHTTPClientCustomProxy(proxyURLStr string) (*http.Client, error) {
	cloned, err := clonedDefaultTransport()
	if err != nil {
		return nil, err
	}

	proxyURL, err := url.Parse(proxyURLStr)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy url: %w", err)
	}

	switch proxyURL.Scheme {
	case "http", "https":
		cloned.Proxy = http.ProxyURL(proxyURL)
	case "socks", "socks5":
		socksDialer, err := proxy.FromURL(proxyURL, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("invalid socks proxy: %w", err)
		}
		cloned.Proxy = nil
		cloned.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return socksDialer.Dial(network, addr)
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
	}

	cloned.ResponseHeaderTimeout = responseHeaderTimeout

	return &http.Client{Transport: cloned}, nil
}
