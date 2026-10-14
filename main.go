// zen-direct: OpenCode 免费层直连网关（V2 配方，见 oc2api v2.6.0 + 抓包验证）。
// stream:true + stream_options.include_usage + shell/read 官方桩 +
// V2 UA + 会话三元头 + 无 tool_choice:none 即可 200。
// 上游恒强制 stream:true；客户端非流由网关聚 SSE（nonstream.go）。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const zenUA = "opencode/latest/2.0.29/cli"
const mintChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
const mintHex = "0123456789abcdef"

var (
	mintMu      sync.Mutex
	mintLastTs  int64
	mintCounter int64
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// mintIDAt 按 opencode identifier.ts 原样签发 prefix_ 开头 26 位 ID，
// 含 descending 取反（上游 FreeTier 门禁认低 12 位大计数值；ascending 小
// 计数现签必 403，见 probe_dir/probe_rule 实测）。
func mintIDAt(prefix string, tsMs int64) string {
	mintMu.Lock()
	if tsMs != mintLastTs {
		mintLastTs = tsMs
		mintCounter = 0
	}
	mintCounter++
	ctr := mintCounter
	mintMu.Unlock()

	current := ^(uint64(tsMs)*0x1000 + uint64(ctr)) // descending（~now）
	var sb [30]byte
	copy(sb[:len(prefix)], prefix)
	sb[len(prefix)] = '_'
	for i := 0; i < 6; i++ {
		b := byte((current >> uint(40-8*i)) & 0xff)
		sb[len(prefix)+1+2*i] = mintHex[b>>4]
		sb[len(prefix)+1+2*i+1] = mintHex[b&0x0f]
	}
	var rb [14]byte
	if _, err := rand.Read(rb[:]); err != nil {
		n := uint64(time.Now().UnixNano())
		for i := range rb {
			rb[i] = byte((n >> uint(8*(i%8))) & 0xff)
		}
	}
	for i, b := range rb {
		sb[len(prefix)+13+i] = mintChars[int(b)%62]
	}
	return string(sb[:])
}

func mintID(prefix string) string { return mintIDAt(prefix, time.Now().UnixMilli()) }

// newID envelop ID（移植老仓，供转换函数用）。
func newID(prefix string) string { return prefix + randHex(12) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))[:2*n]
	}
	return hex.EncodeToString(b)
}

func setDefault(h http.Header, k, v string) {
	if h.Get(k) == "" {
		h.Set(k, v)
	}
}

// setZenHeaders 上游鉴权头 + V2 UA + 会话关联头（对照 2.0.29 源码与抓包）。
// 旧版 X-Opencode-Request 已废弃，不再发送。三元头与最终 session 一致：
// 调用方若自带 X-Opencode-Session 则沿用，否则现签。
func setZenHeaders(h http.Header, apiKey, project string) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("User-Agent", zenUA)
	ses := h.Get("X-Opencode-Session")
	if ses == "" {
		ses = mintID("ses")
		h.Set("X-Opencode-Session", ses)
	}
	setDefault(h, "X-Opencode-Client", "cli")
	setDefault(h, "X-Opencode-Session-Id", ses)
	setDefault(h, "X-Session-Affinity", ses)
	setDefault(h, "X-Session-Id", ses)
	setDefault(h, "X-Opencode-Project", project)
}

func writeErr(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}

var upstream *url.URL
var apiKey, project string

// maxReqBody 入口体上限（var 供单测覆盖小值）。
var maxReqBody = 32 << 20

// fwdSem 上游并发门：子弹先飞，内存先算账。
// 单请求常驻≈body(32M)+解析膨胀(2~3x)+上游体，满载 N 并发≈N×100MB。
var fwdSem = make(chan struct{}, fwdConcurrency())

func fwdConcurrency() int {
	n := 8
	if v := os.Getenv("DIRECT_CONCURRENCY"); v != "" {
		var parsed int
		if _, err := fmt.Sscanf(v, "%d", &parsed); err == nil && parsed >= 1 && parsed <= 32 {
			n = parsed
		}
	}
	return n
}

// 注：上游包体空闲超时复用 convert_stream.go 的 newIdleTimeoutBody +
// streamIdleTimeout()（超时直接 Close 打断阻塞 Read），main.go 不另造类型。

var httpClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		ResponseHeaderTimeout: headerTimeout(),
		// 透明 gzip 无上限解压=压缩炸弹敞口；SSE/JSON 体小，关了只多一点带宽。
		DisableCompression: true,
		// 代理/VPN 常静默掐空闲 keep-alive（对端 RST/半开），池里陈连接复用即
		// unexpected EOF。用比缺省更短的空闲上限，宁可多建连，不复用僵尸连接。
		IdleConnTimeout: 60 * time.Second,
	},
}

// isTransportRetryable 上游传输层失败是否值得同请求重试一次。
// 只认“连接没建成就断”的错（无响应字节，无服务端状态可言）；
// DNS/代理配置错、超时类一律不重试（重试也一样的结果，还多烧一次）。
func isTransportRetryable(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unexpected eof") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "forcibly closed") || // Windows 版的 RST：An existing connection was forcibly closed...
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, " eof")
}

func forwardModels(w http.ResponseWriter, req *http.Request) {
	u := *upstream
	u.Path = singleJoin(upstream.Path, "/models")
	fwd, _ := http.NewRequestWithContext(req.Context(), "GET", u.String(), nil)
	setZenHeaders(fwd.Header, apiKey, project)
	resp, err := httpClient.Do(fwd)
	if err != nil {
		writeErr(w, 502, "upstream_error", "upstream unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 4<<20))
}

// ipProviders 出口 IP 探测源（var 供单测替换）。
var ipProviders = []string{"https://api.ipquery.io", "http://ip-api.com/json"}

var ipv4Re = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)

func validIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

// fetchIPFrom 单源探测：10s 超时，正文抽第一个合法 IPv4。
func fetchIPFrom(ctx context.Context, src string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", src, nil)
	if err != nil {
		return "", err
	}
	resp, err := ipHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	m := ipv4Re.FindString(string(b))
	if m == "" || !validIPv4(m) {
		return "", fmt.Errorf("no ipv4 in body")
	}
	return m, nil
}

// ipHTTPClient /ip 探测专用客户端（不走 zen httpClient 的代理超时语义）。
var ipHTTPClient = &http.Client{Timeout: 12 * time.Second}

// handleIP 并行问多源，先到先得（oc2api 同款 /ip，查网关出口 IP）。
func handleIP(w http.ResponseWriter, req *http.Request) {
	type result struct {
		ip, source string
		err        error
	}
	ch := make(chan result, len(ipProviders))
	ctx := req.Context()
	for _, src := range ipProviders {
		src := src
		go func() {
			ip, err := fetchIPFrom(ctx, src)
			ch <- result{ip: ip, source: src, err: err}
		}()
	}
	for i := 0; i < len(ipProviders); i++ {
		select {
		case <-ctx.Done():
			writeErr(w, 504, "timeout_error", "ip lookup timeout")
			return
		case r := <-ch:
			if r.err != nil {
				continue
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"ip": r.ip, "source": r.source})
			return
		}
	}
	writeErr(w, 502, "upstream_error", "all IP providers failed")
}

func singleJoin(base, p string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(p, "/")
}

// normalizeModel 去 provider 前缀 + 剥 [1M] 标记 + 去空格。
// harness 发 provider/model-id（如 test/...），上游只认裸 model id
// （实测带前缀 401）。大小写保留原样透传上游（上游区分大小写），
// 路由查表时调用方用小写另查。
func normalizeModel(in map[string]any) {
	m, _ := in["model"].(string)
	m = stripOneMSuffix(m)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	in["model"] = m
}

func isOfficialUA(ua string) bool { return strings.HasPrefix(ua, "opencode/") }

// fpForEntry 下游会话键：X-Session-Id 头优先；无头按入口格式取首文本+instructions/system。
// keyed=false 表示无法定键（空体/首条非文本）：调用方不得进池（防多会话同键
// 串 caller），走“每请求现签 + 首见剥离”安全默认。
func fpForEntry(entry Format, h http.Header, in map[string]any) (string, bool) {
	if sid := strings.TrimSpace(h.Get("X-Session-Id")); sid != "" {
		return fpKey(sid, "", ""), true
	}
	if sid := strings.TrimSpace(h.Get("x-session-affinity")); sid != "" {
		return fpKey(sid, "", ""), true
	}
	ins, _ := in["instructions"].(string)
	sys, _ := in["system"].(string)
	first := ""
	items, _ := in["input"].([]any)
	msgs, _ := in["messages"].([]any)
	if entry == FmtChat && len(msgs) > 0 {
		if m, ok := msgs[0].(map[string]any); ok {
			first = textOfContent(m["content"])
		}
	} else if len(items) > 0 {
		if m, ok := items[0].(map[string]any); ok {
			first = textOfContent(m["content"])
		}
	}
	if strings.TrimSpace(first) == "" {
		return "", false
	}
	return fpKey("", first, ins+sys), true
}

// isCallerMismatch 上游 400 是否“签发 caller 不一致”（毒串回放）。
// 措辞三代：was not issued to this caller / not found or has expired / was not issued。
func isCallerMismatch(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "was not issued to this caller") ||
		strings.Contains(s, "not found or has expired") ||
		strings.Contains(s, "was not issued")
}

// debugOn DIRECT_DEBUG=1 时转储 reasoning IDs + 上游体前 4KB。
// 入口行（模型/会话/池/字节数）常驻一行，无需开关——单行成本，定位靠它。
func debugOn() bool { return os.Getenv("DIRECT_DEBUG") == "1" }

func shortStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// collectReasoningIDs 入口 input[] 顶层 + message.content[] 内嵌的 reasoning/thinking id。
func collectReasoningIDs(in map[string]any) []string {
	ids := []string{}
	items, _ := in["input"].([]any)
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "reasoning" {
			if id, _ := m["id"].(string); id != "" {
				ids = append(ids, id)
			} else {
				ids = append(ids, "(no-id)")
			}
			continue
		}
		if content, ok := m["content"].([]any); ok {
			for _, p := range content {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if pm["type"] == "reasoning" || pm["type"] == "thinking" {
					if id, _ := pm["id"].(string); id != "" {
						ids = append(ids, "nested:"+id)
					} else {
						ids = append(ids, "nested:(no-id)")
					}
				}
			}
		}
	}
	return ids
}
func buildUpstream(entry Format, target Format, in map[string]any) map[string]any {
	upReq := in
	if entry == FmtChat && target == FmtResponses {
		upReq = chatToResponsesReq(in)
	} else if entry == FmtResponses && target == FmtChat {
		upReq = responsesToChatReq(in)
	}
	// V2 全路径拒绝显式 tool_choice:none：客户端禁用工具时剥掉用户工具
	// （只剩 never-call 桩）并删除该键；自带真工具时原样透传其选择。
	if tc, ok := in["tool_choice"]; ok && tc == "none" {
		delete(in, "tool_choice")
		in["tools"] = []any{}
	}
	// 保留名改别名（须在注桩之前：harness 的 read/shell 让位给官方桩）。
	if n := aliasRequestTools(upReq, target); n > 0 && debugOn() {
		log.Printf("alias %d reserved tool(s) for upstream", n)
	}
	// 注桩前记录：客户端是否自带真工具。
	if target == FmtResponses {
		mergeStubTools(upReq)
		// 同格式直传时客户端 reasoning.effort 原样透传，上游白名单外一律 400
		//（如 effort 'none'）。有该键即归一化；缺省不动（沿用上游默认）。
		if _, ok := upReq["reasoning"]; ok {
			normalizeReasoningEffort(upReq)
		}
	} else {
		mergeChatStubTools(upReq)
	}
	// V2 全路径拒绝显式 tool_choice:none，缺省即 auto 语义。
	// 上方已剥离客户端 none；转换函数只透传用户自带的有效选择。
	return upReq
}

// writeBodyWithType 透传已读的上游错误体（保留原 Content-Type）。
func writeBodyWithType(w http.ResponseWriter, resp *http.Response, status int, b []byte) {	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// passthruUpstreamError 上游非 200 读体透传（idle 包裹，截断 256KB）。
func passthruUpstreamError(w http.ResponseWriter, reqID string, resp *http.Response, idle time.Duration) {
	ir := newIdleTimeoutBody(resp.Body, idle)
	defer ir.Close()
	b, _ := io.ReadAll(io.LimitReader(ir, 256<<10))
	log.Printf("[REQ %s] upstream %d: %.300s", reqID, resp.StatusCode, string(b))
	writeBodyWithType(w, resp, resp.StatusCode, b)
}

// logForensics caller-mismatch 取证落盘：生产 stderr 常丢（Hidden 启动），
// 下次复发直接看文件即可定位毒条目形状，无需抓包。
// 路径 DIRECT_400_LOG，缺省 %TEMP%/zen-direct-400s.log。
// forensicsMaxBytes 取证文件上限（默认 5MB）：超限转单备 .bak 后重写。
// 取证行只在 400 自愈路径写，正常流量下几乎不涨；上限防极端刷屏。
var forensicsMaxBytes int64 = 5 << 20

func logForensics(reqID string, entry Format, model, fp string, in map[string]any, healed bool) {
	path := os.Getenv("DIRECT_400_LOG")
	if path == "" {
		path = filepath.Join(os.TempDir(), "zen-direct-400s.log")
	}
	ids := []string{}
	nItems := 0
	if items, ok := in["input"].([]any); ok {
		nItems = len(items)
		for _, raw := range items {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "reasoning" {
				if id, _ := m["id"].(string); id != "" && len(ids) < 8 {
					ids = append(ids, id)
				}
			}
			if content, ok := m["content"].([]any); ok {
				for _, p := range content {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					if (pm["type"] == "reasoning" || pm["type"] == "thinking") && len(ids) < 8 {
						if id, _ := pm["id"].(string); id != "" {
							ids = append(ids, "nested:"+id)
						} else {
							ids = append(ids, "nested:(no-id)")
						}
					}
				}
			}
		}
	}
	rec := map[string]any{
		"ts": time.Now().Format(time.RFC3339), "req": reqID,
		"entry": string(entry), "model": model, "fp": fp,
		"nItems": nItems, "reasoningIDs": ids, "healed": healed,
	}
	b, _ := json.Marshal(rec)
	if fi, err := os.Stat(path); err == nil && fi.Size() >= forensicsMaxBytes {
		_ = os.Rename(path, path+".bak")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("[REQ %s] forensics write: %v", reqID, err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// forwardResponses 通用入口：entry 格式由 path 定，上游格式由模型表定。
// responses 直连注桩；chat 双向转换；messages 直接 400（未探明，不裸奔）。
// retry502Backoff 502/503 重试退避（包级变量，单测可覆盖成毫秒级）。
var retry502Backoff = 1500 * time.Millisecond

func forwardResponses(w http.ResponseWriter, req *http.Request, entry Format) {
	reqID := "resp_dir_" + randHex(6)
	reqStart := time.Now()
	// ttfbMs 首轮响应头耗时（连接健康的最直接信号）；重试不覆盖。
	var ttfbMs int64
	// 熔断开直接 fast-fail：不占槽、不打上游（风暴时减负）。
	if !cbAllow() {
		log.Printf("[REQ %s] circuit open, fast-fail 502", reqID)
		statsAddCircuitOpen()
		writeErr(w, 502, "circuit_open", "upstream circuit open, retry later")
		return
	}
	// 并发门在读体之前：排队请求不抱 32MB（先读后抢=内存放大器）。
	waitStart := time.Now()
	select {
	case fwdSem <- struct{}{}:
		defer func() { <-fwdSem }()
	case <-req.Context().Done():
		writeErr(w, 499, "client_closed", "client closed while queued")
		return
	}
	waitMs := time.Since(waitStart).Milliseconds()
	statsAddReq()
	raw, err := io.ReadAll(io.LimitReader(req.Body, int64(maxReqBody)+1))
	if err != nil {
		writeErr(w, 400, "invalid_request_error", "read body: "+err.Error())
		return
	}
	if len(raw) > maxReqBody {
		writeErr(w, 413, "invalid_request_error", "request body too large")
		return
	}
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, 400, "invalid_request_error", "invalid json: "+err.Error())
		return
	}
	if entry == FmtMessages {
		writeErr(w, 400, "invalid_request_error", "messages entry not supported, use chat or responses")
		return
	}
	normalizeModel(in)
	model, _ := in["model"].(string)
	// 网关不支持的上游端点（如 jev 的 /systemone）：400 明拒，
	// 不再默认 responses 透传撞上游 500。
	if ep, bad := lookupUnsupported(model); bad {
		writeErr(w, 400, "invalid_request_error", "model requires unsupported upstream endpoint: "+ep)
		return
	}
	// 路由查表用小写，透传上游保留原大小写。
	target, ok := lookupFormat(strings.ToLower(model))
	if !ok {
		target = FmtResponses // 未知模型默认 responses 透传（现行行为）
	}
	if target == FmtGemini || target == FmtMessages {
		writeErr(w, 400, "invalid_request_error", "model requires unsupported upstream format: "+string(target))
		return
	}
	stream, _ := in["stream"].(bool)

	// 会话池：官方 UA（opencode 本体）一律直通——不进池、不剥离。
	// 自带真 session 即同一 caller；无头首轮也不进池（误剥自己的首轮回放
	// 比什么都贵）。其余（harness）配池稳定 session。
	// 无键请求（fpForEntry keyed=false）不进池：现签 + 首见剥离。
	official := isOfficialUA(req.Header.Get("User-Agent"))
	var fp, poolSes string
	var isNewFP bool
	usePool := false
	if !official {
		var keyed bool
		fp, keyed = fpForEntry(entry, req.Header, in)
		if keyed {
			usePool = true
			poolSes, isNewFP = poolBorrowEx(fp)
		} else {
			// 无键=无法定 lineage，按首见处理（剥离开），但不入库。
			isNewFP = true
		}
		if entry == FmtResponses {
			stripForeignBlobs(in, isNewFP)
		}
		// 池命中（strip 被跳过）：只剥记名毒 id，其余原样保 lineage。
		if usePool && !isNewFP && entry == FmtResponses {
			if n := stripKnownBadIDs(in); n > 0 && debugOn() {
				log.Printf("[REQ %s] debug stripKnownBad=%d", reqID, n)
			}
		}
	}

	buildBody := func() []byte {
		u := buildUpstream(entry, target, in)
		// FreeTier 门禁实测要求上游 stream:true（否则 403）；
		// 客户端非流由网关聚 SSE 回 JSON（见 nonstream.go）。
		u["stream"] = true
		// V2 实测配方：usage 随流回（oc2api 同款，不影响网关聚合/透传）。
		u["stream_options"] = map[string]any{"include_usage": true}
		b, _ := json.Marshal(u)
		return b
	}
	upBody := buildBody()
	// 失败轮不进池：本轮新建的 session 若首战即败（传输错/非 2xx），
	// 直接驱逐，免死 session 占池坑；命中轮（老 lineage）绝不驱逐。
	// 半截流（terminal=false）不驱逐：首轮已有响应输出，lineage 有效，
	// 驱逐反丢 lineage；残留毒串由既有 400 自愈机制兜底。
	// 401/403 与 caller-mismatch 分支已做 poolRotate，不在此列。
	evictIfNewFP := func() {
		if usePool && isNewFP {
			poolEvict(fp)
		}
	}
	log.Printf("[REQ %s] in=%s model=%s target=%s stream=%v official=%v fp=%s ses=%s new=%v upBytes=%d waitMs=%d",
		reqID, entry, model, target, stream, official, shortStr(fp, 16), shortStr(poolSes, 20), isNewFP, len(upBody), waitMs)
	if diagOn() && len(upBody) > 1<<20 {
		var nItems int
		if arr, ok := in["input"].([]any); ok {
			nItems = len(arr)
		}
		log.Printf("[REQ %s] WARN huge upstream body bytes=%d nItems=%d", reqID, len(upBody), nItems)
	}
	if debugOn() && diagOn() {
		ids := collectReasoningIDs(in)
		log.Printf("[REQ %s] debug reasoningIDs=%v", reqID, ids)
		n := len(upBody)
		if n > 4096 {
			n = 4096
		}
		log.Printf("[REQ %s] debug upBody[:%d]=%.4000s", reqID, n, string(upBody[:n]))
	}

	doUpstream := func(ses string, body []byte) (*http.Response, error) {
		u := *upstream
		u.Path = singleJoin(upstream.Path, target.upstreamPath())
		fwd, _ := http.NewRequestWithContext(req.Context(), "POST", u.String(), bytes.NewReader(body))
		fwd.Header.Set("Content-Type", "application/json")
		if ses != "" {
			fwd.Header.Set("X-Opencode-Session", ses)
		}
		setZenHeaders(fwd.Header, apiKey, project)
		// 首字节计时：Do 在响应头到达后返回，即 TTFB；只采首轮（重试不覆盖）。
		t0 := time.Now()
		resp, err := httpClient.Do(fwd)
		if ttfbMs == 0 {
			ttfbMs = time.Since(t0).Milliseconds()
			statsAddTTFB(ttfbMs)
		}
		return resp, err
	}

	upSes := ""
	if usePool {
		upSes = poolSes
	}
	// 上游包体空闲超时：首字节后 stall 即掐（防半截流钉死 handler）。
	// 超时直接 Close 上游体打断阻塞 Read（见 newIdleTimeoutBody）。
	streamIdle := streamIdleTimeout()
	resp, err := doUpstream(upSes, upBody)
	// 传输层失败（建连即断，无响应字节）同请求重试一次：
	// zen 经本地代理时对端 RST/EOF 是常态，重试一次多半就通；
	// 写下游字节前判定，流/非流皆安全，且仅一次。
	if err != nil && isTransportRetryable(err) {
		log.Printf("[REQ %s] transport error, retry once: %.150s", reqID, err.Error())
		statsAddTransportRetry()
		resp, err = doUpstream(upSes, upBody)
	}
	if err != nil {
		evictIfNewFP()
		writeErr(w, 502, "upstream_error", "upstream unreachable: "+err.Error())
		return
	}
	// 池 session 401/403：原子轮换并同请求重试一次（新签 session，body 不变）。
	if (resp.StatusCode == 401 || resp.StatusCode == 403) && usePool {
		resp.Body.Close()
		log.Printf("[REQ %s] pooled session expired, rotate and retry once", reqID)
		poolSes, _ = poolRotate(fp)
		upSes = poolSes // 502 分支重用：不能拿烧掉的旧 session
		resp, err = doUpstream(poolSes, upBody)
		if err != nil {
			writeErr(w, 502, "upstream_error", "upstream unreachable: "+err.Error())
			return
		}
	}
	// caller-mismatch 400 自愈（仅 responses 入口 + 池 lineage + 一次）：
	// 池命中时首轮 strip 被跳过，历史里混着跨 key 毒串必 400；
	// 此时轮换 session + 强制剥离重建 body 再发一次。
	// 写下游字节前判定，流/非流皆安全。
	if resp.StatusCode == 400 && entry == FmtResponses && usePool {
		bpeek, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if isCallerMismatch(bpeek) {
			log.Printf("[REQ %s] caller-mismatch, rotate+strip and retry once", reqID)
			learnBadIDs(collectReasoningIDs(in)) // 实锤毒 id 入负缓存，下轮命中路径先行剥离，免每轮交 400 税。
			poolSes, _ = poolRotate(fp)
			stripForeignBlobs(in, true)
			upBody = buildBody()
			upSes = poolSes // 502 分支重用：body 已按新 session 重建
			resp, err = doUpstream(poolSes, upBody)
			if err != nil {
				writeErr(w, 502, "upstream_error", "upstream unreachable: "+err.Error())
				return
			}
			if resp.StatusCode != 200 {
				logForensics(reqID, entry, model, fp, in, false)
				passthruUpstreamError(w, reqID, resp, streamIdle)
				return
			}
			log.Printf("[REQ %s] caller-mismatch healed, retry 200", reqID)
			logForensics(reqID, entry, model, fp, in, true)
		} else {
			// 非 caller-mismatch 的 400：原样透传已读体。
			log.Printf("[REQ %s] upstream 400: %.300s", reqID, string(bpeek))
			writeBodyWithType(w, resp, 400, bpeek)
			return
		}
	}
	// 上游 502/503 瞬时抖：退避重发一次（写下游字节前，流/非流皆安全）。
	// 500 不碰（模型真失败，重试纯烧钱）；429 不碰（限流越冲越死）。
	// 注意：自愈分支的 400 重试若撞上 502/503，会落到这里再重试一次（最多 3 次上游调用）。
	if resp.StatusCode == 502 || resp.StatusCode == 503 {
		log.Printf("[REQ %s] upstream %d flaky, backoff retry once", reqID, resp.StatusCode)
		statsAddFlakyRetry()
		resp.Body.Close()
		select {
		case <-time.After(retry502Backoff):
		case <-req.Context().Done():
			writeErr(w, 499, "client_closed", "client closed during retry backoff")
			return
		}
		resp, err = doUpstream(upSes, upBody)
		if err != nil {
			evictIfNewFP()
			writeErr(w, 502, "upstream_error", "upstream unreachable: "+err.Error())
			return
		}
	}
	ir := newIdleTimeoutBody(resp.Body, streamIdle)
	defer ir.Close()
	if resp.StatusCode != 200 {
		cbRecord(resp.StatusCode) // 只计 5xx；4xx（门禁/毒串）是请求问题，不熔断。
		statsAddUpstream(resp.StatusCode)
		evictIfNewFP()
		b, _ := io.ReadAll(io.LimitReader(ir, 256<<10))
		log.Printf("[REQ %s] upstream %d: %.300s", reqID, resp.StatusCode, string(b))
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(b)
		return
	}
	cbRecord(200) // 上游受理（流式以响应头 200 为准，中途截断不影响受理计数）。
	if stream {
		// 同格式 responses 直传；跨格式走转换（DONE 语义由各转换函数保证）。
		if entry == FmtResponses && target == FmtResponses {
			pipeSSE(w, ir, reqID, model, ttfbMs)
			return
		}
		if entry == FmtChat && target == FmtResponses {
			streamResponsesToChat(w, ir, model)
			return
		}
		if entry == FmtResponses && target == FmtChat {
			streamChatToResponses(w, ir, model)
			return
		}
		pipeSSE(w, ir, reqID, model, ttfbMs)
		return
	}
	// 上游恒为 SSE（buildBody 强制 stream:true）；客户端非流时聚合回 JSON。
	raw, errRd := io.ReadAll(io.LimitReader(ir, 64<<20))
	if errRd != nil {
		writeErr(w, 502, "upstream_error", "read upstream response: "+errRd.Error())
		return
	}
	var upResp map[string]any
	if looksLikeSSE(raw) {
		upResp, err = assembleNonStreamFromSSE(raw, target, model)
		if err != nil {
			log.Printf("[REQ %s] non-stream assemble: %v", reqID, err)
			writeErr(w, 502, "upstream_error", err.Error())
			return
		}
	} else if errJ := json.Unmarshal(raw, &upResp); errJ != nil {
		writeErr(w, 502, "upstream_error", "decode upstream response: "+errJ.Error())
		return
	}
	// 非流 completed 同样回显 tools：harness 自带定义，删之（有 output 才认定是响应体形状）。
	if _, hasOut := upResp["output"]; hasOut {
		if tools, ok := upResp["tools"].([]any); ok && len(tools) > 0 {
			delete(upResp, "tools")
			log.Printf("[REQ %s] stripped %d echoed tools from non-stream response", reqID, len(tools))
		}
	}
	out := upResp
	if entry == FmtChat && target == FmtResponses {
		out = responsesToChatResp(upResp, model)
	} else if entry == FmtResponses && target == FmtChat {
		out = chatToResponsesResp(upResp, model)
	}
	w.Header().Set("Content-Type", "application/json")
	outBytes, _ := json.Marshal(out)
	outBytes = restoreAliasNames(outBytes)
	log.Printf("[REQ %s] non-stream done ttfbMs=%d ms=%d outBytes=%d",
		reqID, ttfbMs, time.Since(reqStart).Milliseconds(), len(outBytes))
	_, _ = w.Write(outBytes)
}

// pipeSSE 同格式流原样透传（DONE 补齐语义保留）。
// reqID/model 仅用于收尾日志与 abnormal failed 帧。
// 正常结束（见终态）：缺 DONE 则补；异常结束（无终态即 EOF/超时）：
// 发 response.failed（code+type 双写，pi-ai 只读 code）再补 DONE——
// 下游从“ended before terminal”UNKNOWN 变为可分类错误
// （harness 走 TIMEOUT/TRANSPORT 重试），不再静默挂起。
// stripCompletedToolsLine 剥 response.completed 帧里的 tools 回显数组。
// harness 自带工具定义，回显纯冗余（实测 30KB+ 单行，巨帧正是下游 parse 炸的主因）；
// reasoning/usage/output 原样保留。只认 data: 行 + 含 "response.completed"；
// 解析失败一律原样（不猜不修）。
func stripCompletedToolsLine(line []byte) []byte {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) || !bytes.Contains(t, []byte(`"response.completed"`)) {
		return line
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(t, []byte("data:")))
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return line
	}
	resp, ok := m["response"].(map[string]any)
	if !ok {
		return line
	}
	if _, ok := resp["tools"]; !ok {
		return line
	}
	delete(resp, "tools")
	out, err := json.Marshal(m)
	if err != nil {
		return line
	}
	return append(append([]byte("data: "), out...), '\n')
}

func pipeSSE(w http.ResponseWriter, r io.Reader, reqID, model string, ttfbMs int64) {
	start := time.Now()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	// 上游有时以 ping 帧收尾、不发 [DONE]（但给了 completed）。
	// 有终态无 DONE 时由网关补一个，SSE 客户端才能干净结束；
	// 无终态（半截流）绝不补，沿用“不断言完成”语义。
	var tail []byte
	var overlap []byte // 上一块尾部 256B：终态标记被读边界切开时靠它续接。
	sawTerminal, sawDone := false, false
	note := func(p []byte) {
		tail = append(tail, p...)
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		// 粘性标记必须扫“重叠+新块”，不能只扫滑动窗口：
		// completed 体回显 57 桩 tools（8KB+ 单行）时，标记远在窗口之外，
		// 只扫窗口=标记被挤出=把成功流误判为半截流（shadow 实测误报）。
		scan := string(append(append([]byte{}, overlap...), p...))
		// 行级匹配（上游主形态是 `event: response.completed` 行，data 行为 JSON）。
		// 裸词匹配已删：模型正文里的 response.completed 不得误判（否则半截流被当正常结束）。
		for _, ln := range strings.Split(scan, "\n") {
			t := strings.TrimSpace(ln)
			if strings.HasPrefix(t, "event:") {
				ev := strings.TrimSpace(strings.TrimPrefix(t, "event:"))
				if ev == "response.completed" || ev == "response.incomplete" || ev == "response.failed" {
					sawTerminal = true
				}
			}
			if strings.Contains(t, `"response.completed"`) ||
				strings.Contains(t, `"response.incomplete"`) ||
				strings.Contains(t, `"response.failed"`) {
				sawTerminal = true
			}
			if strings.HasPrefix(t, "data:") && strings.Contains(t, "[DONE]") {
				sawDone = true
			}
		}
		if len(tail) > 256 {
			overlap = tail[len(tail)-256:]
		} else {
			overlap = tail
		}
	}
	buf := make([]byte, 32<<10)
	var nBytes int64
	// 行缓冲：只转发行（以 \n 结尾），半截永不落地。
	// 延迟不变（逐行 flush）；探测 note() 仍喂原始字节流，overlap 语义不变。
	var pending []byte
	var upErr, downErr error
	emit := func(line []byte) {
		nBytes += int64(len(line))
		note(line)
		if _, werr := w.Write(line); werr != nil {
			downErr = werr
		} else if fl != nil {
			fl.Flush()
		}
	}
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				emit(restoreAliasNames(stripCompletedToolsLine(pending[:i+1])))
				pending = pending[i+1:]
				if downErr != nil {
					break
				}
			}
			if downErr != nil {
				break
			}
			// 错乱上游单行永不换行：8MB 兜底直发，防 pending 无界涨内存
			// （正常 completed 单行 <1MB）。
			if len(pending) > 8<<20 {
				log.Printf("[REQ %s] stream huge line without newline, passthrough %d bytes", reqID, len(pending))
				emit(pending)
				pending = nil
				if downErr != nil {
					break
				}
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				upErr = rerr
			}
			break
		}
	}
	timedOut := false
	firstByte := false
	if ib, ok := r.(*idleTimeoutBody); ok && ib.wasTimeout() {
		timedOut = true
		firstByte = ib.timedOutWaitingFirst()
	}
	log.Printf("[REQ %s] stream end bytes=%d terminal=%v done=%v upErr=%v downErr=%v ttfbMs=%d ms=%d",
		reqID, nBytes, sawTerminal, sawDone, upErr, downErr, ttfbMs, time.Since(start).Milliseconds())
	if downErr != nil {
		return // 下游已死，补帧无意义，只留日志。
	}
	if sawTerminal && !sawDone {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
		return
	}
	if !sawTerminal {
		// 半截尾部吞掉不转发（下游严格 parse 见半截 data: 必炸）；
		// 发 failed+DONE 走可重试终态，本轮内容由重试无损拿回。
		if len(pending) > 0 {
			log.Printf("[REQ %s] stream swallowed partial line bytes=%d", reqID, len(pending))
		}
		msg, code := "upstream stream ended without terminal event", "upstream_truncated"
		if timedOut {
			if firstByte {
				msg, code = "upstream first byte timeout", "upstream_first_byte_timeout"
			} else {
				msg, code = "upstream idle timeout", "upstream_timeout"
			}
		}
		fb, _ := json.Marshal(map[string]any{"type": "response.failed",
			"response": map[string]any{"id": reqID, "model": model, "status": "failed",
				"error": map[string]any{"message": msg, "type": code, "code": code}}})
		_, _ = io.WriteString(w, "event: response.failed\ndata: "+string(fb)+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

func main() {
	var err error
	upstream, err = url.Parse(env("OPENCODE_ZEN_BASE", "https://opencode.ai/inference/v1"))
	if err != nil {
		log.Fatalf("OPENCODE_ZEN_BASE 非法: %v", err)
	}
	// 网关自管日志文件：LOG_FILE 设路径即启用，按 LOG_MAX_MB（默认 200）总量轮转；
	// 未设则沿用 stderr（旧行为）。自管后不再依赖外部重定向，Hidden 启动不丢日志。
	if lp := env("LOG_FILE", ""); lp != "" {
		budget := 200
		if v := env("LOG_MAX_MB", ""); v != "" {
			var parsed int
			if _, err := fmt.Sscanf(v, "%d", &parsed); err == nil && parsed >= 1 {
				budget = parsed
			}
		}
		if rw, err := newRotateWriter(lp, budget); err != nil {
			log.Printf("log file %s 打开失败，沿用 stderr: %v", lp, err)
		} else {
			log.SetOutput(rw)
			log.Printf("file logging on: %s budget=%dMB", lp, budget)
		}
	}
	apiKey = env("OPENCODE_ZEN_API_KEY", "public")
	project = env("OPENCODE_ZEN_PROJECT", "global")
	seedBuiltinTable()
	tableStartRefresher() // 官方 Endpoints 表刷新（失败则沿用内置快照）
	poolLoad()
	if diagOn() {
		go statsReporter(5 * time.Minute)
	} else {
		log.Printf("diagnostics off (DIRECT_DIAG=0): no [STATS]/WARN/debug lines")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s := statsSnapshot()
		fmt.Fprintf(w, `{"status":"ok","mode":"direct","version":%q,"table_models":%d,"stats":{"reqs":%d,"transport_retry":%d,"flaky_retry":%d,"up5xx":%d,"up4xx":%d,"circuit_open":%d,"ttfb_p50_ms":%d,"ttfb_p95_ms":%d}}`,
			appVersion, tableModelCount(), s.Reqs, s.TransportRetry, s.FlakyRetry, s.Up5xx, s.Up4xx, s.CircuitOpens, s.TTFBP50, s.TTFBP95)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			writeErr(w, 404, "invalid_request_error", "not found")
			return
		}
		forwardModels(w, req)
	})
	mux.HandleFunc("/ip", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			writeErr(w, 404, "invalid_request_error", "not found")
			return
		}
		handleIP(w, req)
	})
	post := func(entry Format) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, req *http.Request) {
			if req.Method != "POST" {
				writeErr(w, 404, "invalid_request_error", "not found")
				return
			}
			forwardResponses(w, req, entry)
		}
	}
	mux.HandleFunc("/v1/responses", post(FmtResponses))
	mux.HandleFunc("/v1/chat/completions", post(FmtChat))
	mux.HandleFunc("/v1/messages", post(FmtMessages))

	addr := ":" + env("PORT", "8080")
	log.Printf("zen-direct %s listening %s upstream %s debug=%v concurrency=%d", appVersion, addr, upstream.String(), debugOn(), fwdConcurrency())
	log.Printf("timeouts: header=%ds first_byte=%ds idle=%ds",
		int(headerTimeout().Seconds()), int(firstByteTimeout().Seconds()), int(streamIdleTimeout().Seconds()))
	// WriteTimeout 不设：SSE 流可长达数分钟，写超时会掐正常流。
	// 空闲超时 + 读头超时防慢连接占 handler。
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
