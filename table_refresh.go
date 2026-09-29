// table_refresh.go: 官方 Endpoints 表自动刷新。
//
// 为什么不能“自动试出协议”：上游 /v1/models 只给 id，没有格式字段；
// 运行时三格式试错成本高且信号模糊（500=格式错或模型挂，如 longcat
// responses 与 jev 全格式；403 是门禁问题不是格式问题）。
// 唯一权威来源是官网 zen.mdx 的 Endpoints 表（模型 ID -> 上游路径），
// 本文件在启动时抓取解析并定时刷新，失败则沿用内置快照（零风险降级）。
package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// defaultTableURL 官方 zen 文档 raw 地址（table.go 注释同源）。
const defaultTableURL = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/zen.mdx"

// zenTableClient 刷新专用客户端：15s 超时，走环境代理。
var zenTableClient = &http.Client{Timeout: 15 * time.Second}

// unsupportedModels 精确模型 ID -> 上游路径（网关不支持的端点，如 /systemone）。
// 命中时 forwardResponses 直接 400 明拒，不再按默认 responses 透传撞上游 500。
var unsupportedModels = map[string]string{}

// endpointToFormat 上游路径 -> 网关格式；ok=false 表示网关不支持该端点。
func endpointToFormat(ep string) (Format, bool) {
	e := strings.ToLower(strings.TrimSpace(ep))
	switch {
	case strings.Contains(e, "/chat/completions"):
		return FmtChat, true
	case strings.Contains(e, "/responses"):
		return FmtResponses, true
	case strings.Contains(e, "/messages"):
		return FmtMessages, true
	case strings.Contains(e, "/models/"):
		return FmtGemini, true
	default:
		return "", false
	}
}

// parseEndpointsTable 解析 zen.mdx 的 Endpoints 表。
// 只认含 "https://opencode.ai/zen/v1" 的行（自动跳过表头/分隔行/价格表/下线表）。
// 返回：模型->格式，模型->不支持端点。id 统一小写（lookup 用小写查）。
func parseEndpointsTable(md string) (map[string]Format, map[string]string) {
	formats := map[string]Format{}
	unsupported := map[string]string{}
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "|") {
			continue
		}
		if !strings.Contains(t, "https://opencode.ai/zen/v1") {
			continue
		}
		// | Model | Model ID | Endpoint | AI SDK Package |
		cells := strings.Split(t, "|")
		if len(cells) < 5 {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(cells[2]))
		ep := strings.Trim(strings.TrimSpace(cells[3]), "`")
		if id == "" || ep == "" {
			continue
		}
		if strings.ContainsAny(id, " \t") {
			continue // 防表头 "Model ID" 之类
		}
		if f, ok := endpointToFormat(ep); ok {
			formats[id] = f
		} else {
			unsupported[id] = ep
		}
	}
	return formats, unsupported
}

// tableURL 刷新地址（ZEN_TABLE_URL 覆盖，空回缺省）。
func tableURL() string {
	if v := strings.TrimSpace(os.Getenv("ZEN_TABLE_URL")); v != "" {
		return v
	}
	return defaultTableURL
}

// tableRefreshDisabled ZEN_TABLE_REFRESH=0/false/off 即全关（纯内置快照）。
func tableRefreshDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ZEN_TABLE_REFRESH"))) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// tableRefreshInterval 刷新间隔（ZEN_TABLE_INTERVAL，如 6h；非法回缺省 6h）。
func tableRefreshInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ZEN_TABLE_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("table: bad ZEN_TABLE_INTERVAL=%q, use 6h", v)
	}
	return 6 * time.Hour
}

// refreshTableFromOfficial 单次抓取+合并，返回新增/更新的模型数。
// 失败返回 -1（快照不动）。并发安全。
func refreshTableFromOfficial() int {
	url := tableURL()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("table: build request: %v", err)
		return -1
	}
	resp, err := zenTableClient.Do(req)
	if err != nil {
		log.Printf("table: fetch %s: %v", url, err)
		return -1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("table: fetch %s: http %d", url, resp.StatusCode)
		return -1
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		log.Printf("table: read body: %v", err)
		return -1
	}
	formats, unsupported := parseEndpointsTable(string(body))
	if len(formats) == 0 && len(unsupported) == 0 {
		log.Printf("table: parsed 0 rows from %s, keep snapshot", url)
		return -1
	}
	tableMu.Lock()
	n := 0
	for id, f := range formats {
		if old, ok := modelTable[id]; !ok || old != f {
			n++
		}
		modelTable[id] = f
		// 官方表优先：若某模型曾被标 unsupported，以官方格式为准。
		delete(unsupportedModels, id)
	}
	for id, ep := range unsupported {
		if _, ok := unsupportedModels[id]; !ok {
			n++
		}
		unsupportedModels[id] = ep
	}
	total := len(modelTable)
	tableMu.Unlock()
	log.Printf("table: refreshed %d models (+%d changed), unsupported=%d, total=%d",
		len(formats), n, len(unsupported), total)
	return n
}

// lookupUnsupported 模型是否命中网关不支持的上游端点。
func lookupUnsupported(model string) (string, bool) {
	tableMu.RLock()
	ep, ok := unsupportedModels[strings.ToLower(model)]
	tableMu.RUnlock()
	return ep, ok
}

// tableModelCount /health 暴露的表规模。
func tableModelCount() int {
	tableMu.RLock()
	defer tableMu.RUnlock()
	return len(modelTable)
}

// tableStartRefresher 启动时同步刷新一次（首请求即用新表），
// 再按间隔后台刷新。ZEN_TABLE_REFRESH=0 则全关。
func tableStartRefresher() {
	if tableRefreshDisabled() {
		log.Printf("table: refresh disabled (ZEN_TABLE_REFRESH=0), builtin snapshot only")
		return
	}
	refreshTableFromOfficial()
	go func() {
		t := time.NewTicker(tableRefreshInterval())
		defer t.Stop()
		for range t.C {
			refreshTableFromOfficial()
		}
	}()
}
