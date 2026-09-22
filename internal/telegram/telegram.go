// Package telegram là client tối giản cho Telegram Bot API, chỉ dùng
// net/http + encoding/json chuẩn của Go, không phụ thuộc thư viện ngoài.
package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"ani-telegram/internal/limits"
	"ani-telegram/internal/safeio"
)

type Client struct {
	token      string
	httpClient *http.Client
	limits     limits.Limits
}

func NewClient(token string, configured ...limits.Limits) *Client {
	lim := limits.Default()
	if len(configured) > 0 {
		lim = configured[0]
	}
	return &Client{
		token:  token,
		limits: lim,
		// timeout phải lớn hơn poll timeout gửi cho getUpdates
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// NewClientWithHTTPClient allows callers to provide transport policy while
// retaining the same limits and request handling as NewClient.
func NewClientWithHTTPClient(token string, httpClient *http.Client, configured ...limits.Limits) *Client {
	c := NewClient(token, configured...)
	if httpClient != nil {
		c.httpClient = httpClient
	}
	return c
}

type User struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// PhotoSize là 1 kích thước của 1 ảnh Telegram gửi lên — Telegram luôn gửi kèm nhiều size,
// mảng Message.Photo xếp từ nhỏ tới lớn, phần tử cuối là ảnh gốc/lớn nhất.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int    `json:"file_size"`
}

type Message struct {
	MessageID int         `json:"message_id"`
	From      *User       `json:"from"`
	Chat      Chat        `json:"chat"`
	Text      string      `json:"text"`
	Photo     []PhotoSize `json:"photo,omitempty"`
	Caption   string      `json:"caption,omitempty"` // chú thích ảnh — Telegram gửi qua field riêng, KHÔNG nằm trong Text
	Date      int64       `json:"date"`
}

type Update struct {
	UpdateID      int      `json:"update_id"`
	Message       *Message `json:"message"`
	EditedMessage *Message `json:"edited_message"`
}

type apiResponse[T any] struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
	Parameters  struct {
		RetryAfter int64 `json:"retry_after"`
	} `json:"parameters"`
	Result T `json:"result"`
}

// SendError reports whether a send is safe to retry without exposing request
// URLs, bot tokens, or message text.
type SendError struct {
	HTTPStatus int
	Code       int
	RetryAfter time.Duration
	Kind       string
	Cause      error
}

func (e *SendError) Error() string {
	if e == nil {
		return "telegram sendMessage failed"
	}
	return fmt.Sprintf("telegram sendMessage failed (kind=%s http_status=%d code=%d retry_after=%s)", e.Kind, e.HTTPStatus, e.Code, e.RetryAfter)
}

func (e *SendError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (c *Client) apiURL(method string) string {
	return fmt.Sprintf("https://api.telegram.org/bot%s/%s", c.token, method)
}

// requestError strips net/url wrappers because their Error method includes the full
// request URL, and Telegram embeds the bot token in that URL. The underlying cause is
// retained so errors.Is/errors.As still work for timeout and cancellation handling.
func requestError(operation string, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		cause := urlErr.Err
		for {
			nested, ok := cause.(*url.Error)
			if !ok {
				break
			}
			cause = nested.Err
		}
		return fmt.Errorf("telegram %s: request failed: %w", operation, cause)
	}
	return fmt.Errorf("telegram %s: request failed: %w", operation, err)
}

// GetUpdates long-poll các update mới kể từ offset (update_id đầu tiên muốn nhận).
func (c *Client) GetUpdates(offset int, timeoutSec int) ([]Update, error) {
	body, err := json.Marshal(map[string]any{
		"offset":  offset,
		"timeout": timeoutSec,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.apiURL("getUpdates"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, requestError("getUpdates", err)
	}
	defer resp.Body.Close()

	raw, err := safeio.ReadResponse(resp, c.limits.TelegramResponseBytes, "getUpdates response")
	if err != nil {
		return nil, err
	}

	var parsed apiResponse[[]Update]
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("getUpdates: parse lỗi: %w (response_bytes=%d)", err, len(raw))
	}
	if !parsed.OK {
		return nil, fmt.Errorf("getUpdates: telegram trả lỗi: %s", parsed.Description)
	}

	return parsed.Result, nil
}

// SendMessage gửi text về đúng chatID.
func (c *Client) SendMessage(chatID int64, text string) error {
	_, err := c.SendMessageResult(chatID, text)
	return err
}

// SendMessageResult sends text and returns Telegram's message metadata. The
// result is used by the durable event archive to record only confirmed sends.
func (c *Client) SendMessageResult(chatID int64, text string) (Message, error) {
	return c.SendMessageResultContext(context.Background(), chatID, text)
}

// SendMessageResultContext performs exactly one Telegram API attempt. Retry
// policy belongs to the delivery coordinator.
func (c *Client) SendMessageResultContext(ctx context.Context, chatID int64, text string) (Message, error) {
	body, err := json.Marshal(map[string]any{
		"chat_id": chatID,
		"text":    text,
	})
	if err != nil {
		return Message{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL("sendMessage"), bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	sendHTTPClient := *c.httpClient
	sendHTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := sendHTTPClient.Do(req)
	if err != nil {
		kind := "unknown"
		if definitelyNotSent(err) {
			kind = "not_sent"
		}
		return Message{}, &SendError{Kind: kind, Cause: requestError("sendMessage", err)}
	}
	defer resp.Body.Close()

	raw, err := safeio.ReadResponse(resp, c.limits.TelegramResponseBytes, "sendMessage response")
	if err != nil {
		return Message{}, &SendError{HTTPStatus: resp.StatusCode, Kind: "unknown", Cause: err}
	}

	var parsed apiResponse[Message]
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Message{}, &SendError{HTTPStatus: resp.StatusCode, Kind: "unknown", Cause: fmt.Errorf("sendMessage response parse failed: %w (response_bytes=%d)", err, len(raw))}
	}
	if resp.StatusCode >= 500 {
		return Message{}, &SendError{HTTPStatus: resp.StatusCode, Code: parsed.ErrorCode, Kind: "unknown", Cause: errors.New("telegram server error")}
	}
	if resp.StatusCode == http.StatusTooManyRequests || parsed.ErrorCode == http.StatusTooManyRequests {
		maxRetrySeconds := int64(math.MaxInt64 / int64(time.Second))
		if parsed.Parameters.RetryAfter < 0 || parsed.Parameters.RetryAfter > maxRetrySeconds {
			return Message{}, &SendError{HTTPStatus: resp.StatusCode, Code: parsed.ErrorCode, Kind: "unknown", Cause: errors.New("telegram retry delay is invalid")}
		}
		return Message{}, &SendError{HTTPStatus: resp.StatusCode, Code: parsed.ErrorCode, RetryAfter: time.Duration(parsed.Parameters.RetryAfter) * time.Second, Kind: "rate_limited", Cause: errors.New("telegram rate limited request")}
	}
	if !parsed.OK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Message{}, &SendError{HTTPStatus: resp.StatusCode, Code: parsed.ErrorCode, Kind: "rejected", Cause: errors.New("telegram rejected request")}
	}
	if parsed.Result.MessageID <= 0 {
		return Message{}, &SendError{HTTPStatus: resp.StatusCode, Code: parsed.ErrorCode, Kind: "unknown", Cause: errors.New("telegram acknowledgement missing message id")}
	}

	return parsed.Result, nil
}

func definitelyNotSent(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

type fileResult struct {
	FilePath string `json:"file_path"`
}

// GetFile lấy file_path Telegram tương ứng fileID — bước bắt buộc trước khi tải nội dung ảnh
// thật qua DownloadFile.
func (c *Client) GetFile(fileID string) (string, error) {
	body, err := json.Marshal(map[string]any{"file_id": fileID})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, c.apiURL("getFile"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", requestError("getFile", err)
	}
	defer resp.Body.Close()

	raw, err := safeio.ReadResponse(resp, c.limits.TelegramResponseBytes, "getFile response")
	if err != nil {
		return "", err
	}

	var parsed apiResponse[fileResult]
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("getFile: parse lỗi: %w (response_bytes=%d)", err, len(raw))
	}
	if !parsed.OK {
		return "", fmt.Errorf("getFile: telegram trả lỗi: %s", parsed.Description)
	}

	return parsed.Result.FilePath, nil
}

// DownloadFile tải nội dung nhị phân của file Telegram (VD ảnh) qua filePath lấy từ GetFile. Tải
// về xử lý nội bộ rồi encode base64 gửi kèm request cho model — không forward URL chứa bot token
// ra ngoài, cũng né việc URL file Telegram có thời hạn.
func (c *Client) DownloadFile(filePath string) ([]byte, error) {
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", c.token, filePath)

	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, requestError("downloadFile", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloadFile: telegram trả status %d", resp.StatusCode)
	}

	return safeio.ReadResponse(resp, c.limits.ImageBytes, "telegram file")
}

type DownloadedFile struct {
	Path   string
	Size   int64
	MIME   string
	SHA256 string
}

// DownloadFileToTemp streams a Telegram file to a bounded temporary file. The
// caller owns Path and must remove it. declaredSize comes from PhotoSize and is
// only a preflight check; the stream itself is always limited as well.
func (c *Client) DownloadFileToTemp(filePath string, declaredSize int) (_ DownloadedFile, retErr error) {
	if declaredSize > 0 && int64(declaredSize) > c.limits.ImageBytes {
		return DownloadedFile{}, &safeio.TooLargeError{Kind: "telegram photo", Limit: c.limits.ImageBytes}
	}
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", c.token, filePath)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return DownloadedFile{}, requestError("downloadFile", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DownloadedFile{}, fmt.Errorf("downloadFile: telegram trả status %d", resp.StatusCode)
	}
	if resp.ContentLength > c.limits.ImageBytes {
		return DownloadedFile{}, &safeio.TooLargeError{Kind: "telegram photo", Limit: c.limits.ImageBytes}
	}
	f, err := os.CreateTemp("", "ani-telegram-image-*")
	if err != nil {
		return DownloadedFile{}, err
	}
	path := f.Name()
	defer func() {
		if err := f.Close(); err != nil && retErr == nil {
			retErr = err
		}
		if retErr != nil {
			_ = os.Remove(path)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, c.limits.ImageBytes+1))
	if err != nil {
		return DownloadedFile{}, err
	}
	if n > c.limits.ImageBytes {
		return DownloadedFile{}, &safeio.TooLargeError{Kind: "telegram photo", Limit: c.limits.ImageBytes}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return DownloadedFile{}, err
	}
	header := make([]byte, 512)
	hn, err := io.ReadFull(f, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return DownloadedFile{}, err
	}
	return DownloadedFile{Path: path, Size: n, MIME: http.DetectContentType(header[:hn]), SHA256: fmt.Sprintf("%x", h.Sum(nil))}, nil
}
