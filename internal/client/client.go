package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"ipv6-proxy-pool/internal/lease"
)

// Options configures how the client subcommand talks to the management API.
type Options struct {
	AdminURL string
	Token    string
	Server   string
}

// AdminURLOrEmpty returns a normalized admin base URL.
func (o Options) AdminURLOrEmpty() string {
	return strings.TrimRight(o.AdminURL, "/")
}

func (o Options) do(method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, o.AdminURLOrEmpty()+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if o.Token != "" {
		request.Header.Set("Authorization", "Bearer "+o.Token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := fmt.Sprintf("HTTP %d", response.StatusCode)
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &payload) == nil && payload.Error != "" {
			message = payload.Error
		}
		return nil, errors.New(message)
	}
	return data, nil
}

// Create requests a new lease for the client name and prints the assigned
// IPv6 and SOCKS5 port.
func Create(opts Options, name string, persistent bool) error {
	data, err := opts.do(http.MethodPost, "/v1/leases", map[string]any{
		"id":         name,
		"persistent": persistent,
	})
	if err != nil {
		return err
	}
	var entry lease.Lease
	if err := json.Unmarshal(data, &entry); err != nil {
		return fmt.Errorf("decode lease: %w", err)
	}
	fmt.Println("代理已分配：")
	return printLease(opts, entry)
}

// Rotate asks the server to move the client to a fresh IPv6 address while
// keeping its port.
func Rotate(opts Options, name string) error {
	data, err := opts.do(http.MethodPost, "/v1/leases/"+url.PathEscape(name)+"/rotate", nil)
	if err != nil {
		return err
	}
	var entry lease.Lease
	if err := json.Unmarshal(data, &entry); err != nil {
		return fmt.Errorf("decode lease: %w", err)
	}
	fmt.Println("已更换 IPv6：")
	return printLease(opts, entry)
}

// Recycle releases the client's lease and immediately re-acquires a new slot
// under the same name: new port and a fresh IPv6 ("释放并自动重新获取").
func Recycle(opts Options, name string) error {
	data, err := opts.do(http.MethodPost, "/v1/leases/"+url.PathEscape(name)+"/recycle", nil)
	if err != nil {
		return err
	}
	var entry lease.Lease
	if err := json.Unmarshal(data, &entry); err != nil {
		return fmt.Errorf("decode lease: %w", err)
	}
	fmt.Println("已释放旧代理并重新获取：")
	return printLease(opts, entry)
}

// Delete releases the client's IPv6 and port back to the pool.
func Delete(opts Options, name string) error {
	if _, err := opts.do(http.MethodDelete, "/v1/leases/"+url.PathEscape(name), nil); err != nil {
		return err
	}
	fmt.Printf("客户端 %q 已释放，端口和 IPv6 已回收。\n", name)
	return nil
}

// List prints all current leases.
func List(opts Options) error {
	data, err := opts.do(http.MethodGet, "/v1/leases", nil)
	if err != nil {
		return err
	}
	var entries []lease.Lease
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("decode lease list: %w", err)
	}
	if len(entries) == 0 {
		fmt.Println("当前没有租约。")
		return nil
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(writer, "客户端\t端口\tIPv6\t持久\t请求数\t最后活动\n")
	for _, entry := range entries {
		port := "-"
		if entry.Port > 0 {
			port = fmt.Sprintf("%d", entry.Port)
		}
		persistent := "否"
		if entry.Persistent {
			persistent = "是"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\t%s\n",
			entry.ID, port, entry.IPv6, persistent, entry.Requests,
			entry.LastUsedAt.Local().Format("2006-01-02 15:04:05"))
	}
	return writer.Flush()
}

// Status prints the pool overview from the management API.
func Status(opts Options) error {
	data, err := opts.do(http.MethodGet, "/v1/status", nil)
	if err != nil {
		return err
	}
	var status struct {
		Status             string `json:"status"`
		LeaseCount         int    `json:"lease_count"`
		PersistentCount    int    `json:"persistent_count"`
		StandbyCount       int    `json:"standby_count"`
		MinLeases          int    `json:"min_leases"`
		MaxLeases          int    `json:"max_leases"`
		IPv6Prefix         string `json:"ipv6_prefix"`
		SOCKSMode          string `json:"socks_mode"`
		SOCKSListenAddress string `json:"socks_listen_address"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return fmt.Errorf("decode status: %w", err)
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(writer, "服务状态\t%s\n", status.Status)
	fmt.Fprintf(writer, "IPv6 前缀\t%s\n", status.IPv6Prefix)
	fmt.Fprintf(writer, "代理模式\t%s\n", status.SOCKSMode)
	fmt.Fprintf(writer, "SOCKS5 监听\t%s\n", status.SOCKSListenAddress)
	fmt.Fprintf(writer, "客户端租约\t%d（持久 %d）\n", status.LeaseCount, status.PersistentCount)
	fmt.Fprintf(writer, "常驻备用\t%d / %d\n", status.StandbyCount, status.MinLeases)
	fmt.Fprintf(writer, "总容量\t%d / %d\n", status.LeaseCount+status.StandbyCount, status.MaxLeases)
	return writer.Flush()
}

func printLease(opts Options, entry lease.Lease) error {
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(writer, "客户端\t%s\n", entry.ID)
	fmt.Fprintf(writer, "IPv6 出口\t%s\n", entry.IPv6)
	if entry.Port > 0 {
		fmt.Fprintf(writer, "SOCKS5 代理\t%s:%d\n", endpointHost(opts), entry.Port)
	} else {
		fmt.Fprintf(writer, "SOCKS5 代理\t单端口池，用户名 user:%s\n", entry.ID)
	}
	persistent := "否"
	if entry.Persistent {
		persistent = "是"
	}
	fmt.Fprintf(writer, "持久\t%s\n", persistent)
	fmt.Fprintf(writer, "创建时间\t%s\n", entry.CreatedAt.Local().Format("2006-01-02 15:04:05"))
	return writer.Flush()
}

func endpointHost(opts Options) string {
	if opts.Server != "" {
		return opts.Server
	}
	parsed, err := url.Parse(opts.AdminURL)
	if err != nil {
		return "127.0.0.1"
	}
	if host := parsed.Hostname(); host != "" {
		return host
	}
	return "127.0.0.1"
}

// PrintUsage prints the client subcommand help.
func PrintUsage() {
	fmt.Println(`用法：
  ipv6-proxy-pool client create  -name <客户端名> [-persistent] [-admin URL] [-token T] [-server HOST]
  ipv6-proxy-pool client rotate  -name <客户端名> [...]
  ipv6-proxy-pool client recycle -name <客户端名> [...]
  ipv6-proxy-pool client delete  -name <客户端名> [...]
  ipv6-proxy-pool client list    [...]
  ipv6-proxy-pool client status  [...]
  ipv6-proxy-pool client help

通用参数：
  -admin    管理 API 地址（默认 http://127.0.0.1:10070）
  -token    管理令牌；也可通过环境变量 IPV6_PROXY_POOL_TOKEN 提供
  -server   公网主机名或 IP，用于拼装 SOCKS5 代理地址（默认取 -admin 的主机部分）
  -name     客户端（租约）标识，用于在服务器上区分不同客户端

示例：
  ipv6-proxy-pool client create -name client-a -server proxy.example.com
  ipv6-proxy-pool client rotate -name client-a
  ipv6-proxy-pool client recycle -name client-a
  ipv6-proxy-pool client delete -name client-a`)
}
