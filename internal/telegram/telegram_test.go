package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func telegramResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestSendMessageResultContextDecodesRateLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{name: "HTTP 429", status: http.StatusTooManyRequests},
		{name: "HTTP 200 ok false", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret := "secret-token"
			message := "private reply"
			client := NewClient(secret)
			client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return telegramResponse(tc.status, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":2}}`), nil
			})

			_, err := client.SendMessageResultContext(context.Background(), 22, message)
			var sendErr *SendError
			if !errors.As(err, &sendErr) {
				t.Fatalf("error type=%T, want *SendError: %v", err, err)
			}
			if sendErr.HTTPStatus != tc.status || sendErr.Code != 429 || sendErr.Kind != "rate_limited" || sendErr.RetryAfter != 2*time.Second {
				t.Fatalf("unexpected SendError: %+v", sendErr)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), message) {
				t.Fatalf("error leaked sensitive data: %q", err)
			}
		})
	}
}

func TestSendMessageResultContextRejectsMissingMessageID(t *testing.T) {
	client := NewClient("token")
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramResponse(http.StatusOK, `{"ok":true,"result":{"chat":{"id":22}}}`), nil
	})

	_, err := client.SendMessageResultContext(context.Background(), 22, "hello")
	var sendErr *SendError
	if !errors.As(err, &sendErr) || sendErr.Kind != "unknown" {
		t.Fatalf("error=%T %v, want unknown SendError", err, err)
	}
}

func TestSendMessageResultContextRejectsNegativeMessageID(t *testing.T) {
	client := NewClient("token")
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramResponse(http.StatusOK, `{"ok":true,"result":{"message_id":-7}}`), nil
	})
	_, err := client.SendMessageResultContext(context.Background(), 22, "hello")
	var sendErr *SendError
	if !errors.As(err, &sendErr) || sendErr.Kind != "unknown" {
		t.Fatalf("error=%T %v, want unknown SendError", err, err)
	}
}

func TestSendMessageResultContextRejectsOverflowingRetryAfter(t *testing.T) {
	client := NewClient("token")
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramResponse(http.StatusTooManyRequests, `{"ok":false,"error_code":429,"parameters":{"retry_after":9223372036854775807}}`), nil
	})
	_, err := client.SendMessageResultContext(context.Background(), 22, "hello")
	var sendErr *SendError
	if !errors.As(err, &sendErr) || sendErr.Kind != "unknown" || sendErr.RetryAfter != 0 {
		t.Fatalf("error=%T %+v, want unknown SendError without delay", err, sendErr)
	}
}

func TestSendMessageResultContextDoesNotFollowRedirect(t *testing.T) {
	calls := 0
	client := NewClient("token")
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatalf("redirect was followed to %s", req.URL.Host)
		}
		resp := telegramResponse(http.StatusTemporaryRedirect, `{"ok":false,"error_code":307}`)
		resp.Header.Set("Location", "https://redirect.invalid/accepted")
		return resp, nil
	})
	_, err := client.SendMessageResultContext(context.Background(), 22, "hello")
	var sendErr *SendError
	if calls != 1 || !errors.As(err, &sendErr) {
		t.Fatalf("calls=%d error=%T %v, want one request and SendError", calls, err, err)
	}
}

func TestSendMessageResultContextClassifiesTransportUncertainty(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kind string
	}{
		{name: "generic after-send failure", err: errors.New("connection reset"), kind: "unknown"},
		{name: "dial failure", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, kind: "not_sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient("token")
			client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, tc.err })
			_, err := client.SendMessageResultContext(context.Background(), 22, "hello")
			var sendErr *SendError
			if !errors.As(err, &sendErr) || sendErr.Kind != tc.kind || !errors.Is(err, tc.err) {
				t.Fatalf("error=%T %v, want kind=%q preserving cause", err, err, tc.kind)
			}
		})
	}
}

func TestSendMessageResultContextUsesContext(t *testing.T) {
	client := NewClient("token")
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.SendMessageResultContext(ctx, 22, "hello")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
}

func TestChatTypeIsDecoded(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{"from":{"id":11},"chat":{"id":22,"type":"private"},"text":"hello"}`), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Chat.Type != "private" {
		t.Fatalf("Chat.Type=%q, want private", msg.Chat.Type)
	}
}

func TestTransportErrorDoesNotExposeBotToken(t *testing.T) {
	fakeToken := "123456789:" + strings.Repeat("A", 35)
	sentinel := errors.New("dial failed")
	c := NewClient(fakeToken)
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, sentinel
	})

	err := c.SendMessage(22, "hello")
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), fakeToken) || strings.Contains(err.Error(), "/bot") {
		t.Fatalf("transport error làm lộ bot token: %q", err)
	}
	if !strings.Contains(err.Error(), "sendMessage") {
		t.Fatalf("transport error thiếu operation: %q", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("transport error phải giữ nguyên cause: %v", err)
	}
}
