package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"xlan/core"
	"xlan/internal/secure"
)

var (
	hcMu    sync.Mutex
	hcCache *http.Client
	hcTLS   bool
	hcInit  bool
)

// hc 按当前加密开关返回 HTTP 客户端，开关变了会重建。
func hc() *http.Client {
	hcMu.Lock()
	defer hcMu.Unlock()
	tlsOn := secure.Enabled()
	if hcInit && hcTLS == tlsOn {
		return hcCache
	}
	c := &http.Client{Timeout: 30 * time.Second}
	if tlsOn {
		c.Transport = secure.Transport(secure.PinsOf())
	}
	hcCache, hcTLS, hcInit = c, tlsOn, true
	return c
}

func urlOf(base string) string { return secure.Rewrite(base) }

type Info struct {
	OK          bool   `json:"ok"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	Version     string `json:"version"`
	Port        int    `json:"port"`
	UptimeSec   int64  `json:"uptime_sec"`
	AutoAccept  bool   `json:"auto_accept"`
	TLS         bool   `json:"tls"`
	Fingerprint string `json:"fingerprint"`
}

func GetInfo(ctx context.Context, base string) (Info, error) {
	var out Info
	err := getJSON(ctx, urlOf(base)+"/v1/info", &out)
	return out, err
}

type OfferReq struct {
	FromID     string         `json:"from_id"`
	FromName   string         `json:"from_name"`
	ToID       string         `json:"to_id"`
	Kind       string         `json:"kind"`
	Name       string         `json:"name,omitempty"`
	Size       int64          `json:"size,omitempty"`
	SHA256     string         `json:"sha256,omitempty"`
	Body       string         `json:"body,omitempty"`
	TransferID string         `json:"transfer_id,omitempty"`
	Manifest   *core.Manifest `json:"manifest,omitempty"`
}

type OfferResp struct {
	OK        bool     `json:"ok"`
	ID        string   `json:"id"`
	Accepted  bool     `json:"accepted"`
	Status    string   `json:"status"`
	Msg       string   `json:"msg,omitempty"`
	Duplicate bool     `json:"duplicate,omitempty"`
	Path      string   `json:"path,omitempty"`
	Resumed   bool     `json:"resumed,omitempty"`
	DoneFiles []string `json:"done_files,omitempty"`
}

func Offer(ctx context.Context, base string, req OfferReq) (OfferResp, error) {
	var out OfferResp
	err := postJSON(ctx, urlOf(base)+"/v1/offer", req, &out)
	return out, err
}

type OfferState struct {
	OK       bool   `json:"ok"`
	ID       string `json:"id"`
	Status   string `json:"status"`
	Accepted bool   `json:"accepted"`
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size"`
}

func GetOffer(ctx context.Context, base, id string) (OfferState, error) {
	var out OfferState
	err := getJSON(ctx, urlOf(base)+"/v1/offer/"+id, &out)
	return out, err
}

func Decision(ctx context.Context, base, id, action string) error {
	body := map[string]string{"action": action}
	return postJSON(ctx, urlOf(base)+"/v1/offer/"+id+"/decision", body, nil)
}

// PutRange 上传 [start, start+n) 这一段，total 是文件总长。
func PutRange(ctx context.Context, base, id string, r io.Reader, start, n, total int64) error {
	url := fmt.Sprintf("%s/v1/offer/%s/data", urlOf(base), id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, r)
	if err != nil {
		return err
	}
	req.ContentLength = n
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+n-1, total))
	resp, err := hc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload failed: %s %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}

type TextResp struct {
	OK       bool     `json:"ok"`
	Channel  string   `json:"channel"`
	Acked    []string `json:"acked,omitempty"`
	Degraded bool     `json:"degraded,omitempty"`
	Warn     string   `json:"warn,omitempty"`
}

// SendText 通过本地服务发一条文本（公共或私聊）。
func SendText(ctx context.Context, base, body, to string) (TextResp, error) {
	var out TextResp
	req := map[string]string{"body": body, "to": to}
	err := postJSON(ctx, urlOf(base)+"/v1/text", req, &out)
	return out, err
}

func Inbox(ctx context.Context, base string, limit int, since int64) ([]core.Item, error) {
	url := fmt.Sprintf("%s/v1/inbox?limit=%d&since=%d", urlOf(base), limit, since)
	var out struct {
		OK    bool        `json:"ok"`
		Items []core.Item `json:"items"`
	}
	err := getJSON(ctx, url, &out)
	return out.Items, err
}

func Peers(ctx context.Context, base string) ([]core.Peer, error) {
	var out struct {
		OK    bool        `json:"ok"`
		Peers []core.Peer `json:"peers"`
	}
	err := getJSON(ctx, urlOf(base)+"/v1/peers", &out)
	return out.Peers, err
}

func Scan(ctx context.Context, base string, deep bool) ([]core.Peer, error) {
	url := urlOf(base) + "/v1/scan"
	if deep {
		url += "?deep=1"
	}
	var out struct {
		OK    bool        `json:"ok"`
		Peers []core.Peer `json:"peers"`
	}
	err := postJSON(ctx, url, nil, &out)
	return out.Peers, err
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func postJSON(ctx context.Context, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

var (
	streamClient = &http.Client{}
	streamTLS    bool
)

func streamHTTP() *http.Client {
	if secure.Enabled() {
		streamClient.Transport = secure.Transport(secure.PinsOf())
	} else {
		streamClient.Transport = nil
	}
	return streamClient
}

func GetItem(ctx context.Context, base, id string) (core.Item, error) {
	var out struct {
		OK   bool      `json:"ok"`
		Item core.Item `json:"item"`
	}
	err := getJSON(ctx, urlOf(base)+"/v1/item/"+id, &out)
	return out.Item, err
}

func SetTrusted(ctx context.Context, base, id string, trusted bool) error {
	return postJSON(ctx, urlOf(base)+"/v1/trust", map[string]any{"id": id, "trusted": trusted}, nil)
}

// PutFileRange 上传目录里的一个文件，rel 是相对路径。
func PutFileRange(ctx context.Context, base, id, rel string, r io.Reader, start, n, total int64) error {
	url := fmt.Sprintf("%s/v1/offer/%s/file/%s", urlOf(base), id, escapeRel(rel))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, r)
	if err != nil {
		return err
	}
	req.ContentLength = n
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+n-1, total))
	resp, err := hc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload %s failed: %s %s", rel, resp.Status, bytes.TrimSpace(b))
	}
	return nil
}

// FinishFolder 告诉接收方所有文件都传完了，让它做完整性检查。
func FinishFolder(ctx context.Context, base, id string) error {
	return postJSON(ctx, urlOf(base)+"/v1/offer/"+id+"/finish", map[string]string{}, nil)
}

func escapeRel(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = neturl.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Watch 订阅 SSE 事件流。
func Watch(ctx context.Context, base string) (<-chan core.Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlOf(base)+"/v1/events", nil)
	if err != nil {
		return nil, err
	}
	resp, err := streamHTTP().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("events: %s", resp.Status)
	}
	ch := make(chan core.Event, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev core.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				continue
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// Shutdown 让对端（通常是自己）的服务优雅退出。
func Shutdown(ctx context.Context, base string) error {
	return postJSON(ctx, urlOf(base)+"/v1/shutdown", map[string]string{}, nil)
}

// Restart 请求服务重启。deferred 为 true 表示还有传输在跑，等服务空闲再重启。
func Restart(ctx context.Context, base string) (deferred bool, inflight int64, err error) {
	var out struct {
		OK       bool  `json:"ok"`
		Deferred bool  `json:"deferred"`
		Inflight int64 `json:"inflight"`
	}
	if err := postJSON(ctx, urlOf(base)+"/v1/restart", map[string]string{}, &out); err != nil {
		return false, 0, err
	}
	return out.Deferred, out.Inflight, nil
}

// CleanResp 是清理收件箱的结果。
type CleanResp struct {
	OK           bool        `json:"ok"`
	DryRun       bool        `json:"dry_run,omitempty"`
	Count        int         `json:"count"`
	RemovedFiles int         `json:"removed_files,omitempty"`
	Items        []core.Item `json:"items,omitempty"`
}

// CleanInbox 清掉早于 olderThanSec 的记录，可选连文件一起删。
func CleanInbox(ctx context.Context, base string, olderThanSec int64, withFiles, dryRun bool) (CleanResp, error) {
	var out CleanResp
	body := map[string]any{
		"older_than_sec": olderThanSec,
		"with_files":     withFiles,
		"dry_run":        dryRun,
	}
	err := postJSON(ctx, urlOf(base)+"/v1/inbox/clean", body, &out)
	return out, err
}

// AddHistory 上报一条收发记录。
func AddHistory(ctx context.Context, base string, rec core.TransferRecord) error {
	return postJSON(ctx, base+"/v1/history", rec, nil)
}

// History 取最近的收发记录。
func History(ctx context.Context, base string, limit int) ([]core.TransferRecord, error) {
	var out struct {
		OK    bool                  `json:"ok"`
		Items []core.TransferRecord `json:"items"`
	}
	err := getJSON(ctx, fmt.Sprintf("%s/v1/history?limit=%d", urlOf(base), limit), &out)
	return out.Items, err
}

// CleanHistory 清掉旧的收发记录。
func CleanHistory(ctx context.Context, base string, olderThanSec int64) (int, error) {
	var out struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	err := postJSON(ctx, urlOf(base)+"/v1/history/clean", map[string]any{"older_than_sec": olderThanSec}, &out)
	return out.Count, err
}

// RemoveItem 删除收件箱里的一条记录，withFiles 时连文件一起删。
func RemoveItem(ctx context.Context, base, id string, withFiles bool) error {
	url := fmt.Sprintf("%s/v1/item/%s", urlOf(base), id)
	if withFiles {
		url += "?files=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}
