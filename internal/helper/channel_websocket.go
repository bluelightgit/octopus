package helper

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

const websocketHandshakeTimeout = 30 * time.Second

func ChannelWebsocketDialer(channel *model.Channel) (*websocket.Dialer, error) {
	if channel == nil {
		return nil, fmt.Errorf("channel is nil")
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout: websocketHandshakeTimeout,
	}

	switch {
	case !channel.Proxy:
		dialer.Proxy = nil
	case channel.ChannelProxy == nil || strings.TrimSpace(*channel.ChannelProxy) == "":
		proxyURL, err := op.SettingGetString(model.SettingKeyProxyURL)
		if err != nil {
			return nil, err
		}
		if err := configureWebsocketDialerProxy(dialer, proxyURL); err != nil {
			return nil, err
		}
	default:
		if err := configureWebsocketDialerProxy(dialer, strings.TrimSpace(*channel.ChannelProxy)); err != nil {
			return nil, err
		}
	}

	return dialer, nil
}

func configureWebsocketDialerProxy(dialer *websocket.Dialer, proxyURLStr string) error {
	proxyURLStr = strings.TrimSpace(proxyURLStr)
	if proxyURLStr == "" {
		return fmt.Errorf("proxy url is empty")
	}
	if dialer == nil {
		return fmt.Errorf("websocket dialer is nil")
	}

	proxyURL, err := url.Parse(proxyURLStr)
	if err != nil {
		return fmt.Errorf("invalid proxy url: %w", err)
	}

	switch proxyURL.Scheme {
	case "http", "https":
		dialer.Proxy = http.ProxyURL(proxyURL)
		dialer.NetDialContext = nil
	case "socks", "socks5":
		socksDialer, err := proxy.FromURL(proxyURL, proxy.Direct)
		if err != nil {
			return fmt.Errorf("invalid socks proxy: %w", err)
		}
		dialer.Proxy = nil
		dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return socksDialer.Dial(network, addr)
		}
	default:
		return fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
	}

	return nil
}
