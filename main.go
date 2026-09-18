// zen-direct: FreeTier 直连网关 v1。
// 配方（实测）: stream:true + tools 含 opencode 57 桩名 + 官方 UA + 合法 ses/req
// 即可 200，无需 opencode 子进程。harness 原生 tool loop 直通。
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const zenUA = "opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"
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

// mintIDAt 按 opencode identifier.ts 算法签发 prefix_ 开头 26 位 ID。
func mintIDAt(prefix string, tsMs int64) string {
	mintMu.Lock()
	if tsMs != mintLastTs {
		mintLastTs = tsMs
		mintCounter = 0
	}
	mintCounter++
	ctr := mintCounter
	mintMu.Unlock()

	current := uint64(tsMs)*0x1000 + uint64(ctr)
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

// setZenHeaders 上游匿名鉴权头 + 官方 UA + 合法会话。
func setZenHeaders(h http.Header, apiKey, project string) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("User-Agent", zenUA)
	setDefault(h, "X-Opencode-Client", "opencode")
	setDefault(h, "X-Opencode-Session", mintID("ses"))
	setDefault(h, "X-Opencode-Request", mintID("msg"))
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

var httpClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		ResponseHeaderTimeout: 60 * time.Second,
	},
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

func singleJoin(base, p string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(p, "/")
}

// normalizeModel 去 provider 前缀：harness 发 provider/model-id（如 test/...），
// 上游只认裸 model id（实测带前缀 401 Model is not supported）。
func normalizeModel(in map[string]any) {
	m, _ := in["model"].(string)
	if i := strings.Index(m, "/"); i >= 0 {
		in["model"] = m[i+1:]
	}
}

func isOfficialUA(ua string) bool { return strings.HasPrefix(ua, "opencode/") }

// fpForEntry 下游会话键：X-Session-Id 头优先；无头按入口格式取首文本+instructions/system。
func fpForEntry(entry Format, h http.Header, in map[string]any) string {
	if sid := strings.TrimSpace(h.Get("X-Session-Id")); sid != "" {
		return fpKey(sid, "", "")
	}
	if sid := strings.TrimSpace(h.Get("x-session-affinity")); sid != "" {
		return fpKey(sid, "", "")
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
	return fpKey("", first, ins+sys)
}

// forwardResponses 通用入口：entry 格式由 path 定，上游格式由模型表定。
// responses 直连注桩；chat 双向转换；messages 直接 400（未探明，不裸奔）。
func forwardResponses(w http.ResponseWriter, req *http.Request, entry Format) {
	reqID := "resp_dir_" + randHex(6)
	raw, err := io.ReadAll(io.LimitReader(req.Body, 32<<20))
	if err != nil {
		writeErr(w, 400, "invalid_request_error", "read body: "+err.Error())
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
	target, ok := lookupFormat(model)
	if !ok {
		target = FmtResponses // 未知模型默认 responses 透传（现行行为）
	}
	if target == FmtGemini || target == FmtMessages {
		writeErr(w, 400, "invalid_request_error", "model requires unsupported upstream format: "+string(target))
		return
	}
	stream, _ := in["stream"].(bool)

	// 会话池：官方 UA 且自带真 session 不动；其余配池稳定 session。
	// （opencode 自带 x-session-id 真会话，透传即同一 caller；harness 无头，配池。）
	official := isOfficialUA(req.Header.Get("User-Agent")) && req.Header.Get("X-Opencode-Session") != ""
	var fp, poolSes string
	var isNewFP bool
	if !official {
		fp = fpForEntry(entry, req.Header, in)
		poolSes, isNewFP = poolBorrowEx(fp)
		// C 案兜底：首见 fp 的外来 blob 必为别处签发，剥 blob 留文本。
		if entry == FmtResponses {
			stripForeignBlobs(in, isNewFP)
		}
	}

	// 入口 -> 上游转换。
	upReq := in
	if entry == FmtChat && target == FmtResponses {
		upReq = chatToResponsesReq(in)
	} else if entry == FmtResponses && target == FmtChat {
		upReq = responsesToChatReq(in)
	}
	// 注桩（分上游格式）。
	if target == FmtResponses {
		mergeStubTools(upReq)
	} else {
		mergeChatStubTools(upReq)
	}
	upBody, _ := json.Marshal(upReq)

	doUpstream := func(ses string) (*http.Response, error) {
		u := *upstream
		u.Path = singleJoin(upstream.Path, target.upstreamPath())
		fwd, _ := http.NewRequestWithContext(req.Context(), "POST", u.String(), bytes.NewReader(upBody))
		fwd.Header.Set("Content-Type", "application/json")
		setZenHeaders(fwd.Header, apiKey, project)
		if ses != "" {
			fwd.Header.Set("X-Opencode-Session", ses)
		}
		return httpClient.Do(fwd)
	}

	upSes := ""
	if !official {
		upSes = poolSes
	}
	resp, err := doUpstream(upSes)
	if err != nil {
		writeErr(w, 502, "upstream_error", "upstream unreachable: "+err.Error())
		return
	}
	// 池 session 401/403：驱逐并同请求重试一次（新签 session，body 不变）。
	if (resp.StatusCode == 401 || resp.StatusCode == 403) && fp != "" {
		resp.Body.Close()
		log.Printf("[REQ %s] pooled session expired, evict and retry once", reqID)
		poolEvict(fp)
		poolSes, _ = poolBorrowEx(fp)
		resp, err = doUpstream(poolSes)
		if err != nil {
			writeErr(w, 502, "upstream_error", "upstream unreachable: "+err.Error())
			return
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		log.Printf("[REQ %s] upstream %d: %.300s", reqID, resp.StatusCode, string(b))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(b)
		return
	}
	if stream {
		// 同格式 responses 直传；跨格式走转换（DONE 语义由各转换函数保证）。
		if entry == FmtResponses && target == FmtResponses {
			pipeSSE(w, resp.Body)
			return
		}
		if entry == FmtChat && target == FmtResponses {
			streamResponsesToChat(w, resp.Body, model)
			return
		}
		if entry == FmtResponses && target == FmtChat {
			streamChatToResponses(w, resp.Body, model)
			return
		}
		pipeSSE(w, resp.Body)
		return
	}
	var upResp map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&upResp); err != nil {
		writeErr(w, 502, "upstream_error", "decode upstream response: "+err.Error())
		return
	}
	out := upResp
	if entry == FmtChat && target == FmtResponses {
		out = responsesToChatResp(upResp, model)
	} else if entry == FmtResponses && target == FmtChat {
		out = chatToResponsesResp(upResp, model)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// pipeSSE 同格式流原样透传（DONE 补齐语义保留）。
func pipeSSE(w http.ResponseWriter, r io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	// 上游有时以 ping 帧收尾、不发 [DONE]（但给了 completed）。
	// 有终态无 DONE 时由网关补一个，SSE 客户端才能干净结束；
	// 无终态（半截流）绝不补，沿用“不断言完成”语义。
	var tail []byte
	sawTerminal, sawDone := false, false
	note := func(p []byte) {
		tail = append(tail, p...)
		if len(tail) > 256 {
			tail = tail[len(tail)-256:]
		}
		s := string(tail)
		if strings.Contains(s, "response.completed") ||
			strings.Contains(s, "response.incomplete") ||
			strings.Contains(s, "response.failed") {
			sawTerminal = true
		}
		if strings.Contains(s, "[DONE]") {
			sawDone = true
		}
	}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			note(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	if sawTerminal && !sawDone {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

func main() {
	var err error
	upstream, err = url.Parse(env("OPENCODE_ZEN_BASE", "https://opencode.ai/zen/v1"))
	if err != nil {
		log.Fatalf("OPENCODE_ZEN_BASE 非法: %v", err)
	}
	apiKey = env("OPENCODE_ZEN_API_KEY", "public")
	project = env("OPENCODE_ZEN_PROJECT", "default")
	seedBuiltinTable()
	poolLoad()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","mode":"direct"}`)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			writeErr(w, 404, "invalid_request_error", "not found")
			return
		}
		forwardModels(w, req)
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
	log.Printf("zen-direct listening %s upstream %s", addr, upstream.String())
	log.Fatal(http.ListenAndServe(addr, mux))
}
