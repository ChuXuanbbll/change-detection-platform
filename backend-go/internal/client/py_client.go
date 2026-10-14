package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"cdp/backend-go/internal/model"
)

// UpstreamError preserves user validation errors across the business gateway.
type UpstreamError struct {
	StatusCode int
	Body       string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("py upstream %d: %s", e.StatusCode, e.Body)
}

// PyClient 是调用 Python 算法服务的 HTTP 客户端
// 带超时；仅 GET 请求进行指数退避重试
type PyClient struct {
	ControlToken string
	BaseURL      string
	HTTP         *http.Client
	// HTTPLong 用于本地裁切和检测，任务推理时限由冻结配置控制。
	HTTPLong *http.Client
	Retry    int
}

// New 构造 PyClient
func New(baseURL string, timeout time.Duration, retry int) *PyClient {
	if retry < 1 {
		retry = 1
	}
	return &PyClient{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: timeout},
		// 长耗时调用：允许大影像对齐、GPU 推理和 GeoJSON 生成完整结束。
		HTTPLong: &http.Client{Timeout: 25 * time.Hour},
		Retry:    retry,
	}
}

// GetAlgorithms 调 Python /py/algorithms（完全透传）
func (c *PyClient) GetAlgorithms(ctx context.Context) ([]model.Algorithm, error) {
	body, err := c.do(ctx, http.MethodGet, "/py/algorithms", nil)
	if err != nil {
		return nil, err
	}
	var out []model.Algorithm
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode algorithms: %w", err)
	}
	return out, nil
}

// LocalFetch 调 Python POST /py/local/fetch，返回裁切后的影像列表
func (c *PyClient) LocalFetch(ctx context.Context, req *model.LocalFetchReq) (*model.LocalFetchResp, error) {
	// Large local mosaics may take longer than a short HTTP request; never retry a crop.
	dctx, cancel := context.WithTimeout(ctx, 25*time.Hour)
	defer cancel()

	body, err := c.doLong(dctx, http.MethodPost, "/py/local/fetch", req)
	if err != nil {
		return nil, err
	}
	var out model.LocalFetchResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode local fetch response: %w", err)
	}
	return &out, nil
}

// GenerateThumbnail 调 Python POST /py/thumbnail 生成缩略图到 dstPath
// size：缩略图边长（px）
func (c *PyClient) GenerateThumbnail(ctx context.Context, srcPath, dstPath string, size int) error {
	q := url.Values{}
	q.Set("src_path", srcPath)
	q.Set("dst_path", dstPath)
	q.Set("size", fmt.Sprintf("%d", size))
	endpoint := "/py/thumbnail?" + q.Encode()

	_, err := c.doLong(ctx, http.MethodPost, endpoint, nil)
	return err
}

// ExportShapefile converts existing GeoJSON to an in-memory ZIP archive.
func (c *PyClient) ExportShapefile(ctx context.Context, collection json.RawMessage) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return c.doLong(ctx, http.MethodPost, "/py/export/shp", collection)
}

// do 带超时 + 重试（JSON，使用默认短超时客户端）
func (c *PyClient) do(ctx context.Context, method, path string, payload interface{}) ([]byte, error) {
	return c.doWith(ctx, method, path, payload, c.HTTP)
}

// doLong 使用长耗时客户端；POST 不重试。
func (c *PyClient) doLong(ctx context.Context, method, path string, payload interface{}) ([]byte, error) {
	return c.doWith(ctx, method, path, payload, c.HTTPLong)
}

func (c *PyClient) doWith(ctx context.Context, method, path string, payload interface{}, hc *http.Client) ([]byte, error) {
	var payloadBytes []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		payloadBytes = b
	}
	url := c.BaseURL + path
	var lastErr error
	attempts := c.Retry
	if method != http.MethodGet {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// 每次重试都要重新构造 body：bytes.Reader 一次性读取，
		// 复用同一个 reader 会让重试请求发出空 body，导致上游 422。
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payloadBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			return nil, err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-CDP-Control", c.ControlToken)
		resp, err := hc.Do(req)
		if err != nil {
			lastErr = err
			if i+1 < attempts {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(backoff(i)):
				}
			}
			continue
		}
		buf, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("py upstream %d: %s", resp.StatusCode, string(buf))
			if i+1 < attempts {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(backoff(i)):
				}
			}
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: string(buf)}
		}
		return buf, nil
	}
	return nil, fmt.Errorf("py request failed after %d retries: %w", attempts, lastErr)
}

func backoff(i int) time.Duration {
	d := time.Duration(1<<i) * 200 * time.Millisecond
	if d > 3*time.Second {
		d = 3 * time.Second
	}
	return d
}

// ---------- M3: change detection ----------

// DetectReq 提交一次变更检测所需参数
type DetectReq struct {
	BeforePath string                 `json:"before_path"`
	AfterPath  string                 `json:"after_path"`
	Algorithm  string                 `json:"algorithm"`
	Params     map[string]interface{} `json:"params"`
	OutputDir  string                 `json:"output_dir"`
}

// DetectResp Python 端返回的检测结果（输出文件路径 + 统计信息）
type DetectResp struct {
	MaskPath       string                 `json:"mask_path"`
	ConfidencePath string                 `json:"confidence_path"`
	GeoJSONPath    string                 `json:"geojson_path"`
	Stats          map[string]interface{} `json:"stats"`
}

// Detect makes one request. Python owns the configured inference timeout.
func (c *PyClient) Detect(ctx context.Context, req DetectReq) (*DetectResp, error) {
	body, err := c.doLong(ctx, http.MethodPost, "/py/detect", req)
	if err != nil {
		return nil, fmt.Errorf("py detect failed: %w", err)
	}
	var out DetectResp
	if err = json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode detect resp: %w", err)
	}
	return &out, nil
}

// JSON decodes an upstream response; only GET requests may be retried.
func (c *PyClient) JSON(ctx context.Context, method, path string, payload any, out any) error {
	body, err := c.doLong(ctx, method, path, payload)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}
