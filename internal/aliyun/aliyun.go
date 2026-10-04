// Package aliyun 封装阿里云 API 客户端与签名（RPC-2）。
//
// 与上游 Python 版（monitor.py/report.py）行为保持一致：
//   - CDT 流量查询走 cdt.aliyuncs.com RPC-2 端点（官方 Go SDK 未生成 cdt 服务包）
//   - ECS 状态/开关机走生成好的 ecs 服务包
//   - BSS 账单/余额走生成好的 bssopenapi 服务包
//   - 所有请求走 HTTPS，并支持通过环境变量 HTTP_PROXY/HTTPS_PROXY 走代理
package aliyun

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/auth"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/auth/credentials"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/requests"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/bssopenapi"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"
)

const (
	// 各接口默认超时（毫秒），与 Python 版一致：连接 5s / 读取 15s
	defaultConnectTimeoutMS = 5000
	defaultReadTimeoutMS    = 15000
)

// CDTQueryParams 是 ListCdtInternetTraffic 的查询参数（RPC-2 无服务包，用通用请求）。
type CDTQueryParams struct {
	// 可选：按 InternetIP 或 ISP 过滤；传空表示查询全部
	InternetIP string
	ISP        string
	// 查询粒度，默认 "day"；可选 "hour"
	Granularity string
}

// Client 封装访问阿里云所需的凭据与客户端。
type Client struct {
	AK string
	SK string
	// Region 用于创建 ECS/BSS 客户端（仅影响 RegionId 参数与默认域名）
	Region string

	ecsClient *ecs.Client
	bssClient *bssopenapi.Client
	cfg       *sdk.Config
	cred      auth.Credential
}

// NewClient 创建 Client。ak/sk 会先 TrimSpace，避免配置中带入的空白字符导致鉴权失败。
func NewClient(ak, sk, region string) (*Client, error) {
	ak = strings.TrimSpace(ak)
	sk = strings.TrimSpace(sk)
	cfg := sdk.NewConfig().WithScheme("HTTPS")
	cfg.HttpTransport = http.DefaultTransport.(*http.Transport).Clone()
	cred := credentials.NewAccessKeyCredential(ak, sk)
	ecsClient, err := ecs.NewClientWithOptions(region, cfg, cred)
	if err != nil {
		return nil, err
	}
	ecsClient.SetReadTimeout(defaultReadTimeoutMS * time.Millisecond)
	ecsClient.SetConnectTimeout(defaultConnectTimeoutMS * time.Millisecond)
	bssClient, err := bssopenapi.NewClientWithOptions(region, cfg, cred)
	if err != nil {
		return nil, err
	}
	bssClient.SetReadTimeout(defaultReadTimeoutMS * time.Millisecond)
	bssClient.SetConnectTimeout(defaultConnectTimeoutMS * time.Millisecond)
	return &Client{
		AK: ak, SK: sk, Region: region,
		ecsClient: ecsClient, bssClient: bssClient,
		cfg: cfg, cred: cred,
	}, nil
}

// commonRequest 执行一次 RPC-2 通用请求（用于 CDT 这类没有生成服务包的接口）。
// 与 Python 版一致：强制 cn-hangzhou 客户端（部分地域对 CDT 请求存在兼容问题）。
func (c *Client) commonRequest(domain, version, action string, query map[string]string) ([]byte, error) {
	client, err := sdk.NewClientWithOptions("cn-hangzhou", c.cfg, c.cred)
	if err != nil {
		return nil, err
	}
	req := requests.NewCommonRequest()
	req.Method = http.MethodPost
	req.Scheme = "https"
	req.Domain = domain
	req.Version = version
	req.ApiName = action
	req.QueryParams = make(map[string]string)
	for k, v := range query {
		req.QueryParams[k] = v
	}
	// 与 Python 版保持一致：明确设置 RegionId
	req.QueryParams["RegionId"] = "cn-hangzhou"
	resp, err := client.ProcessCommonRequest(req)
	if err != nil {
		return nil, err
	}
	if resp.GetHttpStatus() != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.GetHttpStatus(), resp.GetHttpContentString())
	}
	return resp.GetHttpContentBytes(), nil
}

// GetCDTInternetTraffic 查询 CDT 公网流量（RPC-2 签名直连，不依赖服务包）。
// 返回以字节为单位的流量总和。
func (c *Client) GetCDTInternetTraffic(p CDTQueryParams) (int64, error) {
	q := map[string]string{}
	if p.Granularity != "" {
		q["Granularity"] = p.Granularity
	}
	if p.InternetIP != "" {
		q["InternetIP"] = p.InternetIP
	}
	if p.ISP != "" {
		q["ISP"] = p.ISP
	}
	body, err := c.commonRequest("cdt.aliyuncs.com", "2021-08-13", "ListCdtInternetTraffic", q)
	if err != nil {
		return 0, err
	}
	var out struct {
		TrafficDetails []struct {
			Traffic int64 `json:"Traffic"`
		} `json:"TrafficDetails"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("解析 CDT 响应失败: %v (body=%s)", err, truncate(string(body), 300))
	}
	var total int64
	for _, d := range out.TrafficDetails {
		total += d.Traffic
	}
	return total, nil
}

// GetInstanceStatus 查询单个 ECS 实例当前状态，返回 Aliyun 状态字符串（Running/Stopped/Starting 等）。
func (c *Client) GetInstanceStatus(instanceID string) (string, error) {
	req := ecs.CreateDescribeInstancesRequest()
	req.InstanceIds = fmt.Sprintf(`["%s"]`, instanceID)
	resp, err := c.ecsClient.DescribeInstances(req)
	if err != nil {
		return "", err
	}
	instances := resp.Instances.Instance
	if len(instances) == 0 {
		return "", fmt.Errorf("实例不存在: %s", instanceID)
	}
	return instances[0].Status, nil
}

// GetInstanceDetail 返回实例的详细状态（状态/IP/规格/备注名），供日报与控制机器人使用。
func (c *Client) GetInstanceDetail(instanceID string) (InstanceDetail, error) {
	req := ecs.CreateDescribeInstancesRequest()
	req.InstanceIds = fmt.Sprintf(`["%s"]`, instanceID)
	resp, err := c.ecsClient.DescribeInstances(req)
	if err != nil {
		return InstanceDetail{}, err
	}
	instances := resp.Instances.Instance
	if len(instances) == 0 {
		return InstanceDetail{}, fmt.Errorf("实例不存在: %s", instanceID)
	}
	inst := instances[0]
	detail := InstanceDetail{
		InstanceID: inst.InstanceId,
		Status:     inst.Status,
		CPU:        inst.Cpu,
		MemoryMB:   inst.Memory,
		Name:       inst.InstanceName,
	}
	if inst.EipAddress.IpAddress != "" {
		detail.IP = inst.EipAddress.IpAddress
	} else if len(inst.PublicIpAddress.IpAddress) > 0 {
		detail.IP = inst.PublicIpAddress.IpAddress[0]
	}
	return detail, nil
}

// InstanceDetail 描述一个 ECS 实例的静态/动态信息。
type InstanceDetail struct {
	InstanceID string
	Status     string
	CPU        int
	MemoryMB   int
	IP         string
	Name       string
}

// StartInstance 启动实例。
func (c *Client) StartInstance(instanceID string) error {
	req := ecs.CreateStartInstanceRequest()
	req.InstanceId = instanceID
	_, err := c.ecsClient.StartInstance(req)
	return err
}

// StopInstance 停止实例（force 为 true 表示强制关机）。
func (c *Client) StopInstance(instanceID string, force bool) error {
	req := ecs.CreateStopInstanceRequest()
	req.InstanceId = instanceID
	req.ForceStop = requests.NewBoolean(force)
	_, err := c.ecsClient.StopInstance(req)
	return err
}

// RebootInstance 重启实例。
func (c *Client) RebootInstance(instanceID string, force bool) error {
	req := ecs.CreateRebootInstanceRequest()
	req.InstanceId = instanceID
	req.ForceStop = requests.NewBoolean(force)
	_, err := c.ecsClient.RebootInstance(req)
	return err
}

// QueryAccountBalance 查询账户可用余额。返回 (余额, 货币代码)。优先使用 billEndpoint，
// 失败后回退到另一个节点（国内/国际站兼容），与 Python 版逻辑一致。
func (c *Client) QueryAccountBalance(billEndpoint string) (float64, string, error) {
	candidates := []string{}
	if billEndpoint != "" {
		candidates = append(candidates, billEndpoint)
	}
	for _, ep := range []string{"business.aliyuncs.com", "business.ap-southeast-1.aliyuncs.com"} {
		if !contains(candidates, ep) {
			candidates = append(candidates, ep)
		}
	}
	var lastErr error
	for _, ep := range candidates {
		body, err := c.commonRequest(ep, "2017-12-14", "QueryAccountBalance", nil)
		if err != nil {
			lastErr = err
			continue
		}
		var out struct {
			Success bool `json:"Success"`
			Data    struct {
				AvailableAmount string `json:"AvailableAmount"`
				Currency        string `json:"Currency"`
			} `json:"Data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			lastErr = fmt.Errorf("解析余额响应失败: %v", err)
			continue
		}
		if !out.Success {
			lastErr = fmt.Errorf("查询余额未成功: %s", truncate(string(body), 200))
			continue
		}
		amount, err := strconv.ParseFloat(strings.ReplaceAll(out.Data.AvailableAmount, ",", ""), 64)
		if err != nil {
			lastErr = fmt.Errorf("余额字段无法解析: %q", out.Data.AvailableAmount)
			continue
		}
		return amount, out.Data.Currency, nil
	}
	if lastErr != nil {
		return 0, "", lastErr
	}
	return 0, "", fmt.Errorf("无可用账单节点")
}

// QueryInstanceBill 查询当月指定实例的账单金额（PretaxAmount 汇总），返回（金额, 货币代码）。
// 失败返回 error（调用方可回退到 QueryBillOverview）。
func (c *Client) QueryInstanceBill(instanceID, billingCycle string) (float64, string, error) {
	req := bssopenapi.CreateDescribeInstanceBillRequest()
	req.BillingCycle = billingCycle
	req.InstanceID = instanceID
	resp, err := c.bssClient.DescribeInstanceBill(req)
	if err != nil {
		return 0, "", err
	}
	var total float64
	var currency string
	// 用通用请求解析 JSON（避免依赖生成结构的每个字段）
	body := resp.GetHttpContentBytes()
	var out struct {
		Data struct {
			Items []struct {
				PretaxAmount float64 `json:"PretaxAmount"`
				Currency     string  `json:"Currency"`
			} `json:"Items"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, "", fmt.Errorf("解析实例账单失败: %v", err)
	}
	for _, it := range out.Data.Items {
		total += it.PretaxAmount
		if currency == "" {
			currency = it.Currency
		}
	}
	return total, currency, nil
}

// QueryBillOverview 查询账单总览（国际站兼容路径）。
func (c *Client) QueryBillOverview(billEndpoint, billingCycle string) (float64, string, error) {
	body, err := c.commonRequest(billEndpoint, "2017-12-14", "QueryBillOverview", map[string]string{
		"BillingCycle": billingCycle,
	})
	if err != nil {
		return 0, "", err
	}
	var out struct {
		Data struct {
			Items struct {
				Item []struct {
					PretaxAmount float64 `json:"PretaxAmount"`
					Currency     string  `json:"Currency"`
				} `json:"Item"`
			} `json:"Items"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, "", fmt.Errorf("解析账单总览失败: %v", err)
	}
	var total float64
	var currency string
	for _, it := range out.Data.Items.Item {
		total += it.PretaxAmount
		if currency == "" {
			currency = it.Currency
		}
	}
	return total, currency, nil
}

// ---------- 内部工具 ----------

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// signRPC2 计算阿里云 RPC-2 请求签名（HMAC-SHA1）。当前实现由 aliyun 官方 Go SDK
// 的签名器负责（CommonRequest 自动签名），本函数仅保留用于单元测试对照。
func signRPC2(ak, sk string, params map[string]string) map[string]string {
	p := make(map[string]string, len(params)+6)
	for k, v := range params {
		p[k] = v
	}
	p["AccessKeyId"] = ak
	p["SignatureMethod"] = "HMAC-SHA1"
	p["SignatureVersion"] = "1.0"
	p["SignatureNonce"] = fmt.Sprintf("%d%d", time.Now().UnixNano(), rand.Int63())
	p["Timestamp"] = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(p[k]))
	}
	canon := strings.Join(parts, "&")
	toSign := "POST&%2F&" + url.QueryEscape(canon)
	mac := hmac.New(sha1.New, []byte(sk+"&"))
	mac.Write([]byte(toSign))
	p["Signature"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return p
}
