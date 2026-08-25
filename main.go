package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const abiVersion uint32 = 1

const (
	pluginName    = "cpagw-gateway"
	pluginVersion = "0.5.0"
	modelPrefix   = "cline-pass/"

	failThresholdDefault = 2  // 连续失败多少次后触发 fallback
	recoverStreakTarget  = 20 // 自动切换后连续成功多少次，自动恢复主力
)

// ---------- envelope / RPC 协议 ----------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type rpcLifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type transformRequest struct {
	FromFormat string `json:"FromFormat"`
	ToFormat   string `json:"ToFormat"`
	Model      string `json:"Model"`
	Stream     bool   `json:"Stream"`
	Body       []byte `json:"Body"`
}

type payloadResponse struct {
	Body []byte `json:"Body"`
}

// request.complete 事件（RequestCompletion 的 JSON 子集，Go 默认字段名）
type requestCompletion struct {
	RequestID      string `json:"RequestID"`
	Model          string `json:"Model"`
	RequestedModel string `json:"RequestedModel"`
	Outcome        string `json:"Outcome"`
	StatusCode     int    `json:"StatusCode"`
	Error          string `json:"Error"`
}

type managementRequest struct {
	Method string              `json:"Method"`
	Path   string              `json:"Path"`
	Query  map[string][]string `json:"Query"`
	Body   []byte              `json:"Body"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

// ---------- 插件状态 ----------

type fallbackEvent struct {
	At     string `json:"at"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason"`
}

type pluginState struct {
	Gateway       []string              `json:"gateway"`         // providerOptions.gateway.only 当前值（单值锁定）
	Source        string                `json:"source"`          // state(手动) / auto(自动 fallback) / config / default
	Fallbacks     []string              `json:"fallbacks"`       // fallback 链：链首为主力
	FailThreshold int                   `json:"fail_threshold"`  // 连续失败触发阈值
	FailStreak    int                   `json:"fail_streak"`     // 当前连续失败计数
	SuccessStreak int                   `json:"success_streak"`  // 自动切换后连续成功计数（满 recoverStreakTarget 恢复主力）
	ModelRules    map[string][]string `json:"model_rules,omitempty"`     // 模型级规则：裸模型名（尾部 * 通配）-> gateway.only 候选列表
	ModelProviders map[string][]string `json:"model_providers,omitempty"` // 自定义支撑上游列表（面板按钮 + 报错解析结果，覆盖默认预置）
	LastEvent      *fallbackEvent        `json:"last_event,omitempty"`
}

var (
	mu                 sync.RWMutex
	st                 pluginState
	statePath          string
	fallbacksFromState bool // 运行时标记：state 文件是否显式提供过 fallbacks（优先于 config.yaml）
)

func defaultFallbacks() []string { return []string{"baseten", "togetherai", "fireworks"} }

// 预置模型支撑列表：2026-08-26 实测通过的上游（ClinePass 路由表动态变化，可用「从报错解析」刷新）
func defaultModelProviders() map[string][]string {
	return map[string][]string{
		"glm-5.2": {"baseten", "deepinfra", "digitalocean", "fireworks", "morph", "nebius", "novita", "runware", "togetherai", "wafer", "zai"},
		"glm-5.3": {"zai"},
		"kimi-k3": {"togetherai"},
	}
}

// mergedModelProviders 合并默认预置与自定义（自定义优先；调用方需持有 mu 读锁）
func mergedModelProviders() map[string][]string {
	merged := defaultModelProviders()
	for m, list := range st.ModelProviders {
		if len(list) > 0 {
			merged[m] = list
		}
	}
	return merged
}

// parseProvidersFromError 从 ClinePass 报错文本解析可用上游列表。
// 匹配模式：...Available providers are: a, b, c （结尾为引号或文本末尾）
func parseProvidersFromError(text string) []string {
	m := regexp.MustCompile(`Available providers are:\s*([^"\n]+?)\s*(?:"|$)`).FindStringSubmatch(text)
	if len(m) < 2 {
		return nil
	}
	var out []string
	for _, p := range strings.Split(m[1], ",") {
		p = strings.Trim(p, " .\x60)")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func stateFilePath() string {
	// 测试/特殊部署可注入路径；默认根据二进制位置推导（plugins 目录下）
	if statePath != "" {
		return statePath
	}
	exe, err := os.Executable()
	if err == nil && exe != "" {
		dir := filepath.Dir(exe)
		if strings.HasSuffix(dir, "plugins") || strings.HasSuffix(filepath.Base(dir), "plugins") {
			return filepath.Join(dir, "cpagw-gateway-state.json")
		}
		if fi, err := os.Stat(filepath.Join(dir, "plugins")); err == nil && fi.IsDir() {
			return filepath.Join(dir, "plugins", "cpagw-gateway-state.json")
		}
		return filepath.Join(dir, "plugins", "cpagw-gateway-state.json")
	}
	return "cpagw-gateway-state.json"
}

func loadState() {
	mu.Lock()
	defer mu.Unlock()
	st = pluginState{
		Gateway:       []string{"baseten"},
		Source:        "default",
		Fallbacks:     defaultFallbacks(),
		FailThreshold: failThresholdDefault,
	}
	data, err := os.ReadFile(stateFilePath())
	if err != nil {
		return
	}
	var saved pluginState
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	if len(saved.Gateway) > 0 {
		st.Gateway = saved.Gateway
		st.Source = saved.Source
		if st.Source == "" {
			st.Source = "state"
		}
	}
	if len(saved.Fallbacks) > 0 {
		st.Fallbacks = saved.Fallbacks
		fallbacksFromState = true
	}
	if saved.FailThreshold > 0 {
		st.FailThreshold = saved.FailThreshold
	}
	st.FailStreak = saved.FailStreak
	st.SuccessStreak = saved.SuccessStreak
	st.LastEvent = saved.LastEvent
	if len(saved.ModelRules) > 0 {
		st.ModelRules = saved.ModelRules
	}
	if len(saved.ModelProviders) > 0 {
		st.ModelProviders = saved.ModelProviders
	}
}

func persistState() error {
	sp := stateFilePath()
	data, _ := json.Marshal(st)
	if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
		return err
	}
	return os.WriteFile(sp, data, 0o644)
}

// applyConfig 解析 host 下发的 config_yaml（YAML 子集：gateway 列表 = 初始 fallback 链）
func applyConfig(configYAML []byte) {
	if len(configYAML) == 0 {
		return
	}
	var gw []string
	for _, line := range strings.Split(string(configYAML), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") {
			item := strings.Trim(strings.TrimPrefix(trimmed, "- "), `"' `)
			if item != "" && !strings.HasPrefix(item, "#") {
				gw = append(gw, item)
			}
		}
	}
	if len(gw) == 0 {
		return
	}
	mu.Lock()
	if !fallbacksFromState {
		st.Fallbacks = gw
	}
	// 手动/自动切换优先；未干预时用 config 首项锁定
	if st.Source != "state" && st.Source != "auto" {
		st.Gateway = []string{gw[0]}
		st.Source = "config"
	}
	mu.Unlock()
}

// ---------- 方法分发 ----------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if method == "plugin.reconfigure" && len(request) > 0 {
			var lr rpcLifecycleRequest
			if json.Unmarshal(request, &lr) == nil {
				applyConfig(lr.ConfigYAML)
			}
		}
		reg := `{"schema_version":1,"metadata":{"Name":"` + pluginName + `","Version":"` + pluginVersion +
			`","Author":"Stabilize7440","GitHubRepository":"https://github.com/Stabilize7440/cpagw-gateway","ConfigFields":[]}` +
			`,"capabilities":{"request_normalizer":true,"request_lifecycle_plugin":true,"management_api":true}}`
		return okEnvelopeJSON(reg)
	case "request.normalize":
		return normalizeRequest(request)
	case "request.complete":
		return handleRequestComplete(request)
	case "management.register":
		return okEnvelopeJSON(`{"resources":[` +
			`{"Path":"/home","Menu":"ClinePass 网关","Description":"ClinePass 上游供应商选择与热切换"},` +
			`{"Path":"/config","Description":"当前网关配置(JSON)"},` +
			`{"Path":"/switch","Description":"切换全局上游: ?gateway=baseten[,togetherai]"},` +
			`{"Path":"/rules","Description":"模型级规则: ?model=gpt-5*&gateway=baseten[,togetherai]（gateway=- 删除）"},` +
			`{"Path":"/providers","Description":"模型支撑上游列表: ?model=glm-5.2&providers=a,b,c（providers=- 回默认）"},` +
			`{"Path":"/parse-error","Description":"从报错文本解析可用上游: ?text=<urlencoded>"}` +
			`]}`)
	case "management.handle":
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// matchModelRule 查模型级规则：精确匹配优先，其次最长前缀通配（key 以 * 结尾）。
// 调用方需持有 mu（读或写锁）。
func matchModelRule(name string) ([]string, bool) {
	if len(st.ModelRules) == 0 {
		return nil, false
	}
	if gw, ok := st.ModelRules[name]; ok && len(gw) > 0 {
		return gw, true
	}
	bestPrefix := ""
	var bestRule []string
	for key, gw := range st.ModelRules {
		if !strings.HasSuffix(key, "*") || len(gw) == 0 {
			continue
		}
		prefix := strings.TrimSuffix(key, "*")
		if len(prefix) >= len(bestPrefix) && strings.HasPrefix(name, prefix) {
			bestPrefix = prefix
			bestRule = gw
		}
	}
	if bestRule != nil {
		return bestRule, true
	}
	return nil, false
}

// ---------- request.normalize：注入 providerOptions ----------

func normalizeRequest(request []byte) ([]byte, error) {
	var tr transformRequest
	if err := json.Unmarshal(request, &tr); err != nil {
		return okEnvelopeJSON1(`{"Body":""}`), nil
	}
	model := strings.TrimSpace(tr.Model)
	if !strings.HasPrefix(model, modelPrefix) || len(tr.Body) == 0 {
		return okEnvelopeJSON1(base64BodyJSON(tr.Body)), nil
	}
	mu.RLock()
	var gw []string
	if rule, ok := matchModelRule(strings.TrimPrefix(model, modelPrefix)); ok {
		gw = append(gw, rule...) // 模型级规则优先
	} else {
		gw = make([]string, len(st.Gateway))
		copy(gw, st.Gateway)
	}
	mu.RUnlock()

	if len(gw) == 0 {
		return okEnvelopeJSON1(base64BodyJSON(tr.Body)), nil
	}
	var payload map[string]any
	if err := json.Unmarshal(tr.Body, &payload); err != nil {
		return okEnvelopeJSON1(base64BodyJSON(tr.Body)), nil
	}
	only := make([]any, len(gw))
	for i, g := range gw {
		only[i] = g
	}
	payload["providerOptions"] = map[string]any{
		"gateway": map[string]any{"only": only},
	}
	newBody, err := json.Marshal(payload)
	if err != nil {
		return okEnvelopeJSON1(base64BodyJSON(tr.Body)), nil
	}
	return okEnvelopeJSON1(base64BodyJSON(newBody)), nil
}

// ---------- request.complete：被动 fallback（真实请求失败才切换） ----------

func handleRequestComplete(request []byte) ([]byte, error) {
	var rc requestCompletion
	if err := json.Unmarshal(request, &rc); err != nil {
		return okEnvelopeJSON1(`{}`), nil
	}
	if !strings.HasPrefix(strings.TrimSpace(rc.Model), modelPrefix) {
		return okEnvelopeJSON1(`{}`), nil // 只关心 cline-pass 请求
	}
	// 有模型级规则的请求：上游选择交给规则候选池（CPA 动态选优），不参与全局 fail-streak
	mu.RLock()
	_, ruled := matchModelRule(strings.TrimPrefix(strings.TrimSpace(rc.Model), modelPrefix))
	mu.RUnlock()
	if ruled {
		return okEnvelopeJSON1(`{}`), nil
	}
	switch rc.Outcome {
	case "succeeded":
		onRequestSucceeded()
	case "failed":
		// 5xx 或网络类错误（StatusCode==0 且带错误信息）视为上游故障；4xx/429 不切换
		if rc.StatusCode >= 500 || (rc.StatusCode == 0 && rc.Error != "") {
			onRequestFailed(rc.StatusCode, rc.Error)
		}
	}
	return okEnvelopeJSON1(`{}`), nil
}

func onRequestSucceeded() {
	mu.Lock()
	defer mu.Unlock()
	st.FailStreak = 0
	if st.Source == "auto" && len(st.Fallbacks) > 0 && len(st.Gateway) == 1 && st.Gateway[0] != st.Fallbacks[0] {
		st.SuccessStreak++
		if st.SuccessStreak >= recoverStreakTarget {
			from := st.Gateway[0]
			st.Gateway = []string{st.Fallbacks[0]}
			st.Source = "auto"
			st.SuccessStreak = 0
			st.LastEvent = &fallbackEvent{At: time.Now().Format(time.RFC3339), From: from, To: st.Fallbacks[0], Reason: "auto-recover (stable)"}
			_ = persistState()
		}
		return
	}
	st.SuccessStreak = 0
}

func onRequestFailed(status int, errMsg string) {
	mu.Lock()
	defer mu.Unlock()
	st.SuccessStreak = 0
	st.FailStreak++
	if st.FailStreak < st.FailThreshold || len(st.Fallbacks) < 2 {
		return
	}
	cur := ""
	if len(st.Gateway) > 0 {
		cur = st.Gateway[0]
	}
	idx := -1
	for i, f := range st.Fallbacks {
		if f == cur {
			idx = i
			break
		}
	}
	if idx < 0 || idx >= len(st.Fallbacks)-1 {
		return // 自定义上游或已在链尾：不再自动切
	}
	next := st.Fallbacks[idx+1]
	reason := "HTTP " + strconv.Itoa(status)
	if status == 0 {
		reason = "network error"
	}
	if errMsg != "" {
		if len(errMsg) > 120 {
			errMsg = errMsg[:120]
		}
		reason += " " + errMsg
	}
	st.Gateway = []string{next}
	st.Source = "auto"
	st.FailStreak = 0
	st.SuccessStreak = 0
	st.LastEvent = &fallbackEvent{At: time.Now().Format(time.RFC3339), From: cur, To: next, Reason: reason}
	_ = persistState()
}

// ---------- management.handle：配置 UI 的后端 ----------

func handleManagement(request []byte) ([]byte, error) {
	var mr managementRequest
	if err := json.Unmarshal(request, &mr); err != nil {
		return errEnvelopeResp1(400, "bad request"), nil
	}
	// host 传入的 Path 是完整 URL（如 /v0/resource/plugins/cpagw-gateway/home），剥离前缀后匹配
	fullPath := strings.TrimRight(mr.Path, "/")
	path := fullPath
	if i := strings.Index(fullPath, "/cpagw-gateway"); i >= 0 {
		path = fullPath[i+len("/cpagw-gateway"):]
	}
	if path == "" {
		path = "/"
	}
	switch {
	case mr.Method == "GET" && (path == "" || path == "/" || path == "/home"):
		return htmlResp1(pageHTML()), nil
	case mr.Method == "GET" && (path == "/config" || strings.HasSuffix(path, "/config")):
		// 保存 fallbacks / 阈值（query 参数），然后返回完整状态
		if raw := firstQuery(mr.Query, "fallbacks"); raw != "" {
			parts := strings.Split(raw, ",")
			fb := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					fb = append(fb, p)
				}
			}
			if len(fb) > 0 {
				mu.Lock()
				st.Fallbacks = fb
				fallbacksFromState = true
				if len(st.Gateway) == 0 {
					st.Gateway = []string{fb[0]}
				}
				mu.Unlock()
				_ = persistState()
			}
		}
		if raw := firstQuery(mr.Query, "threshold"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 10 {
				mu.Lock()
				st.FailThreshold = n
				mu.Unlock()
				_ = persistState()
			}
		}
		mu.RLock()
		body, _ := json.Marshal(map[string]any{
			"gateway":        st.Gateway,
			"source":         st.Source,
			"fallbacks":      st.Fallbacks,
			"fail_threshold": st.FailThreshold,
			"fail_streak":    st.FailStreak,
			"success_streak": st.SuccessStreak,
			"model_rules":    st.ModelRules,
			"model_providers": mergedModelProviders(),
			"last_event":     st.LastEvent,
			"plugin":         pluginName,
			"version":        pluginVersion,
		})
		mu.RUnlock()
		return jsonResp1(200, body), nil
	case mr.Method == "GET" && (path == "/rules" || strings.HasSuffix(path, "/rules")):
		// 模型级规则：?model=<name>&gateway=<a,b> 设置；gateway=- 删除
		if model := strings.TrimSpace(firstQuery(mr.Query, "model")); model != "" {
			raw := strings.TrimSpace(firstQuery(mr.Query, "gateway"))
			mu.Lock()
			if raw == "" || raw == "-" {
				delete(st.ModelRules, model)
			} else {
				parts := strings.Split(raw, ",")
				gw := make([]string, 0, len(parts))
				for _, p := range parts {
					if p = strings.TrimSpace(p); p != "" {
						gw = append(gw, p)
					}
				}
				if len(gw) > 0 {
					if st.ModelRules == nil {
						st.ModelRules = map[string][]string{}
					}
					st.ModelRules[model] = gw
				}
			}
			mu.Unlock()
			if err := persistState(); err != nil {
				return errEnvelopeResp1(500, "persist failed: "+err.Error()), nil
			}
		}
		mu.RLock()
		body, _ := json.Marshal(map[string]any{
			"rules":   st.ModelRules,
			"plugin":  pluginName,
			"version": pluginVersion,
		})
		mu.RUnlock()
		return jsonResp1(200, body), nil
	case mr.Method == "GET" && (path == "/providers" || strings.HasSuffix(path, "/providers")):
		// 模型支撑列表：?model=<name>&providers=<a,b,c> 设置（覆盖）；providers=- 删除自定义条目（回默认预置）
		if model := strings.TrimSpace(firstQuery(mr.Query, "model")); model != "" {
			raw := strings.TrimSpace(firstQuery(mr.Query, "providers"))
			mu.Lock()
			if raw == "" || raw == "-" {
				delete(st.ModelProviders, model)
			} else {
				parts := strings.Split(raw, ",")
				gw := make([]string, 0, len(parts))
				for _, p := range parts {
					if p = strings.TrimSpace(p); p != "" {
						gw = append(gw, p)
					}
				}
				if len(gw) > 0 {
					if st.ModelProviders == nil {
						st.ModelProviders = map[string][]string{}
					}
					st.ModelProviders[model] = gw
				}
			}
			mu.Unlock()
			if err := persistState(); err != nil {
				return errEnvelopeResp1(500, "persist failed: "+err.Error()), nil
			}
		}
		mu.RLock()
		body, _ := json.Marshal(map[string]any{
			"providers": mergedModelProviders(),
			"plugin":    pluginName,
			"version":   pluginVersion,
		})
		mu.RUnlock()
		return jsonResp1(200, body), nil
	case mr.Method == "GET" && (path == "/parse-error" || strings.HasSuffix(path, "/parse-error")):
		// 从报错文本解析可用上游：?text=<urlencoded>
		parsed := parseProvidersFromError(firstQuery(mr.Query, "text"))
		body, _ := json.Marshal(map[string]any{
			"providers": parsed,
			"plugin":    pluginName,
			"version":   pluginVersion,
		})
		return jsonResp1(200, body), nil
	case mr.Method == "GET" && (path == "/switch" || strings.HasSuffix(path, "/switch")):
		raw := firstQuery(mr.Query, "gateway")
		if raw == "" {
			return errEnvelopeResp1(400, "missing gateway query parameter"), nil
		}
		parts := strings.Split(raw, ",")
		gw := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				gw = append(gw, p)
			}
		}
		if len(gw) == 0 {
			return errEnvelopeResp1(400, "empty gateway list"), nil
		}
		mu.Lock()
		st.Gateway = gw
		st.Source = "state" // 手动切换优先，自动 fallback 不再干预
		st.FailStreak = 0
		st.SuccessStreak = 0
		mu.Unlock()
		if err := persistState(); err != nil {
			return errEnvelopeResp1(500, "persist failed: "+err.Error()), nil
		}
		body, _ := json.Marshal(map[string]any{"ok": true, "gateway": gw, "source": "state"})
		return jsonResp1(200, body), nil
	default:
		return errEnvelopeResp1(404, "not found"), nil
	}
}

func firstQuery(q map[string][]string, key string) string {
	if v, ok := q[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

func jsonResp1(status int, body []byte) []byte {
	return okEnvelopeJSON1(rawEnvelope(status, "application/json; charset=utf-8", body))
}

func htmlResp1(html string) []byte {
	return okEnvelopeJSON1(rawEnvelope(200, "text/html; charset=utf-8", []byte(html)))
}

func errEnvelopeResp1(status int, msg string) []byte {
	return okEnvelopeJSON1(rawEnvelope(status, "application/json; charset=utf-8", []byte(`{"error":"`+msg+`"}`)))
}

func rawEnvelope(status int, contentType string, body []byte) string {
	raw, _ := json.Marshal(managementResponse{
		StatusCode: status,
		Headers:    http.Header{"content-type": []string{contentType}},
		Body:       body,
	})
	return string(raw)
}

// ---------- 工具 ----------

func base64BodyJSON(body []byte) string {
	return `{"Body":"` + base64.StdEncoding.EncodeToString(body) + `"}`
}

func okEnvelopeJSON(result string) ([]byte, error) {
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(result)})
}

func okEnvelopeJSON1(result string) []byte {
	raw, _ := json.Marshal(envelope{OK: true, Result: json.RawMessage(result)})
	return raw
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	loadState()
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var reqBytes []byte
	if request != nil && requestLen > 0 {
		reqBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), reqBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// ---------- 内嵌管理页面 ----------

func pageHTML() string {
	return `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ClinePass 网关</title>
<style>
:root {
  color-scheme: light dark;
  --bg: var(--app-surface, #ffffff);
  --bg-muted: var(--app-surface-muted, #f6f7f9);
  --text: var(--app-text-primary, #1f2937);
  --text-muted: var(--app-text-muted, #8b95a6);
  --border: var(--app-border, rgba(15,23,42,0.10));
  --primary: var(--primary-color, #4f46e5);
  --primary-hover: var(--primary-hover, #6366f1);
  --danger: var(--danger-color, #ef4444);
  --ok: #10b981;
  --warn: #f59e0b;
  --radius: var(--app-radius-md, 10px);
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font: 14px/1.6 -apple-system, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
  background: var(--bg); color: var(--text);
  padding: 20px; max-width: 680px; margin: 0 auto;
}
h1 { font-size: 17px; font-weight: 650; margin-bottom: 4px; }
.sub { color: var(--text-muted); font-size: 12.5px; margin-bottom: 18px; }
.card {
  background: var(--bg-muted); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 14px 16px; margin-bottom: 14px;
}
.row { display: flex; align-items: center; justify-content: space-between; gap: 10px; flex-wrap: wrap; }
.badge {
  display: inline-block; padding: 2px 10px; border-radius: 999px;
  background: color-mix(in srgb, var(--primary) 12%, transparent);
  color: var(--primary); font-size: 12.5px; font-weight: 600;
}
.badge.src-state { background: color-mix(in srgb, var(--ok) 14%, transparent); color: var(--ok); }
.badge.src-auto { background: color-mix(in srgb, var(--warn) 16%, transparent); color: var(--warn); }
.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(140px, 1fr)); gap: 8px; margin-top: 10px; }
.btn {
  border: 1px solid var(--border); background: var(--bg); color: var(--text);
  border-radius: 8px; padding: 9px 10px; font-size: 13px; cursor: pointer;
  transition: all .15s; text-align: center; word-break: break-all;
}
.btn:hover { border-color: var(--primary); color: var(--primary); }
.btn.active {
  background: var(--primary); border-color: var(--primary);
  color: var(--primary-contrast, #fff); font-weight: 600;
}
.btn:disabled { opacity: .5; cursor: not-allowed; }
input[type=text] {
  flex: 1; min-width: 200px; border: 1px solid var(--border); border-radius: 8px;
  background: var(--bg); color: var(--text); padding: 8px 10px; font-size: 13px;
}
input[type=text]:focus { outline: none; border-color: var(--primary); }
.actions { display: flex; gap: 8px; margin-top: 10px; align-items: center; flex-wrap: wrap; }
.action-btn {
  border: none; background: var(--primary); color: var(--primary-contrast, #fff);
  border-radius: 8px; padding: 8px 18px; font-size: 13px; font-weight: 600; cursor: pointer;
}
.action-btn:hover { background: var(--primary-hover); }
.action-btn.ghost { background: transparent; color: var(--primary); border: 1px solid var(--primary); }
#msg { font-size: 12.5px; min-height: 18px; }
#msg.ok { color: var(--ok); } #msg.err { color: var(--danger); }
code { background: var(--bg-muted); border: 1px solid var(--border); padding: 1px 6px; border-radius: 5px; font-size: 12px; }
.rule-row { display: flex; align-items: center; gap: 10px; padding: 7px 0; border-bottom: 1px solid var(--border); flex-wrap: wrap; }
.rule-row:last-child { border-bottom: none; }
.rule-row code { min-width: 150px; }
.rule-row .gws { display: flex; gap: 4px; flex-wrap: wrap; }
.del { border: none; background: transparent; color: var(--danger); cursor: pointer; font-size: 16px; padding: 2px 8px; line-height: 1; margin-left: auto; }
.mcard {
  border: 1px solid var(--border); border-radius: var(--radius);
  padding: 10px 12px; margin-top: 10px; background: var(--bg);
}
.mcard .mhead { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.mcard .mrule { font-size: 12px; color: var(--text-muted); }
.mcard .mrule b { color: var(--text); }
.mcard .grid { margin-top: 8px; }
.mcard .toolbar { display: flex; gap: 8px; margin-top: 8px; flex-wrap: wrap; }
.mcard input[type=text] { min-width: 120px; flex: 1; }
.mcard .action-btn { padding: 6px 12px; font-size: 12.5px; }
.small-btn {
  border: 1px solid var(--border); background: transparent; color: var(--text-muted);
  border-radius: 8px; padding: 6px 12px; font-size: 12.5px; cursor: pointer;
}
.small-btn:hover { color: var(--primary); border-color: var(--primary); }
</style>
</head>
<body>
<h1>🔀 ClinePass 上游网关</h1>
<div class="sub">cpagw-gateway v` + pluginVersion + ` · 注入 <code>providerOptions.gateway.only</code> · 上游故障自动 fallback</div>

<div class="card">
  <div class="row">
    <div><b>当前上游</b> <span id="src" class="badge">…</span></div>
    <div id="gw-badges"></div>
  </div>
  <div id="meta" class="sub" style="margin:8px 0 0 0">…</div>
</div>

<div class="card">
  <div style="font-weight:600; margin-bottom:2px">Fallback 链（自动切换顺序）</div>
  <div class="sub" style="margin-bottom:8px">当前上游连续失败 <code id="th">2</code> 次 → 自动切到下一个；自动切换后连续成功 20 次 → 自动恢复链首主力</div>
  <div class="row">
    <input id="fbs" type="text" placeholder="baseten, togetherai, fireworks">
    <button class="action-btn" onclick="saveFallbacks()">保存</button>
  </div>
</div>

<div class="card">
  <div style="font-weight:600; margin-bottom:2px">模型上游（快捷切换）</div>
  <div class="sub" style="margin-bottom:0">每个模型独立的支持上游列表，点按钮 = 把该模型规则锁定为对应上游（立即生效）；无规则的模型用全局配置；列表可用「从报错解析」刷新</div>
  <div id="models"></div>
</div>

<div class="card">
  <div style="font-weight:600; margin-bottom:2px">自定义规则（任意模型）</div>
  <div class="sub" style="margin-bottom:0">模型名支持尾部 <code>*</code> 通配（如 <code>gpt-5*</code>），精确匹配优先；单上游 = 严格锁定，多上游 = 候选池动态选优</div>
  <div id="rules"></div>
  <div class="row" style="margin-top:8px">
    <input id="rule-model" type="text" placeholder="模型名，如 gpt-5.1-codex 或 claude-*">
    <input id="rule-gw" type="text" placeholder="上游，如 baseten 或 baseten,togetherai">
    <button class="action-btn" onclick="addRule()">添加</button>
  </div>
</div>

<div class="actions">
  <button class="action-btn ghost" onclick="refresh()">刷新</button>
  <span id="msg"></span>
</div>

<script>
let current = [];
let rules = {};
let providers = {};
let busy = false;

function $(id) { return document.getElementById(id); }
function show(msg, ok) { const m = $("msg"); m.textContent = msg; m.className = ok ? "ok" : "err"; }
function esc(s) { return String(s).replace(/[&<>"']/g, c => ({'&':"&amp;",'<':"&lt;",'>':"&gt;",'"':"&quot;","'":"&#39;"}[c])); }
function dedupe(arr) { return arr.filter(function(v, i) { return arr.indexOf(v) === i; }); }

async function api(path) {
  const r = await fetch(path, { cache: "no-store" });
  if (!r.ok) throw new Error("HTTP " + r.status);
  return r.json();
}

async function refresh(silent) {
  try {
    const d = await api("/v0/resource/plugins/cpagw-gateway/config");
    current = d.gateway || [];
    const src = d.source || "?";
    $("src").textContent = "来源: " + src;
    $("src").className = "badge" + (src === "state" ? " src-state" : src === "auto" ? " src-auto" : "");
    $("gw-badges").innerHTML = current.map(function(g){return '<span class="badge">'+esc(g)+'</span>'}).join(" ");
    $("fbs").value = (d.fallbacks || []).join(", ");
    $("th").textContent = d.fail_threshold;
    let meta = "连续失败 " + (d.fail_streak || 0) + " 次";
    if (d.success_streak > 0) meta += " · 备胎已稳定 " + d.success_streak + " 次";
    if (d.last_event) {
      const e = d.last_event;
      meta += " · 最近事件: " + e.from + " → " + e.to + "（" + e.reason + " " + e.at + "）";
    }
    $("meta").textContent = meta;
    rules = d.model_rules || {};
    providers = d.model_providers || {};
    renderModels();
    renderRules();
    if (!silent) show("已刷新", true);
  } catch (e) { if (!silent) show("加载失败: " + e.message, false); }
}

// ---------- 模型子配置 ----------

function ruleFor(m) { return rules[m] || null; }

function renderModels() {
  const box = $("models");
  const keys = Object.keys(providers);
  if (!keys.length) {
    box.innerHTML = '<div class="sub" style="margin-top:6px">暂无已配置模型</div>';
    return;
  }
  box.innerHTML = keys.map(m => {
    const list = providers[m] || [];
    const cur = ruleFor(m);
    const state = cur ? '<b>'+esc(cur.join(', '))+'</b>' : '走全局配置';
    const btns = list.map(p => {
      const active = cur && cur.indexOf(p) >= 0;
      return '<button class="btn'+(active?' active':'')+'" onclick="setModelRule(\''+esc(m)+'\',\''+esc(p)+'\')">'+esc(p)+'</button>';
    }).join("");
    return '<div class="mcard">' +
      '<div class="mhead"><b>'+esc(m)+'</b>' +
      '<span class="mrule">当前规则: '+state+'</span>' +
      '<button class="del" onclick="delRule(\''+esc(m)+'\')" title="清除规则">×</button></div>' +
      '<div class="grid">'+btns+'</div>' +
      '<div class="toolbar">' +
        '<input type="text" placeholder="自定义上游名" data-m="'+esc(m)+'">' +
        '<button class="action-btn" onclick="addCustom(\''+esc(m)+'\')">添加</button>' +
        '<button class="small-btn" onclick="parseError(\''+esc(m)+'\')">从报错解析</button>' +
      '</div></div>';
  }).join("");
}

async function setModelRule(m, p) {
  if (busy) return;
  busy = true;
  show("切换中…", true);
  try {
    await api("/v0/resource/plugins/cpagw-gateway/rules?model=" + encodeURIComponent(m) + "&gateway=" + encodeURIComponent(p));
    show("已锁定 " + m + " → " + p, true);
    refresh(true);
  } catch (e) { show("切换失败: " + e.message, false); }
  finally { busy = false; }
}

async function addCustom(m) {
  const inp = document.querySelector('input[data-m="'+m+'"]');
  const v = (inp.value || "").trim();
  if (!v) { show("请输入上游名称", false); return; }
  if (busy) return;
  busy = true;
  show("保存中…", true);
  try {
    const merged = dedupe((providers[m]||[]).concat(v.split(/[,\s]+/).filter(Boolean)));
    await api("/v0/resource/plugins/cpagw-gateway/providers?model=" + encodeURIComponent(m) + "&providers=" + encodeURIComponent(merged.join(",")));
    await api("/v0/resource/plugins/cpagw-gateway/rules?model=" + encodeURIComponent(m) + "&gateway=" + encodeURIComponent(merged.join(",")));
    show("已添加并配置: " + m + " → " + merged.join(", "), true);
    refresh(true);
  } catch (e) { show("保存失败: " + e.message, false); }
  finally { busy = false; }
}

async function parseError(m) {
  const text = prompt("粘贴 ClinePass 报错文本（含 Available providers are: ...）：");
  if (!text) return;
  if (busy) return;
  busy = true;
  show("解析中…", true);
  try {
    const d = await api("/v0/resource/plugins/cpagw-gateway/parse-error?text=" + encodeURIComponent(text));
    const parsed = d.providers || [];
    if (!parsed.length) { show("未从文本中解析到上游列表，请确认包含 Available providers are: ...", false); return; }
    const merged = dedupe((providers[m]||[]).concat(parsed));
    await api("/v0/resource/plugins/cpagw-gateway/providers?model=" + encodeURIComponent(m) + "&providers=" + encodeURIComponent(merged.join(",")));
    show("解析到 " + parsed.length + " 个上游，已并入 " + m + "（" + merged.join(", ") + "）", true);
    refresh(true);
  } catch (e) { show("解析失败: " + e.message, false); }
  finally { busy = false; }
}

// ---------- 通用规则 ----------

function renderRules() {
  const box = $("rules");
  const keys = Object.keys(rules).sort();
  if (!keys.length) { box.innerHTML = '<div class="sub" style="margin-top:6px">暂无规则</div>'; return; }
  box.innerHTML = keys.map(k => {
    const badges = (rules[k]||[]).map(g => '<span class="badge">'+esc(g)+'</span>').join("");
    return '<div class="rule-row"><code>'+esc(k)+'</code><span class="gws">'+badges+'</span>' +
      '<button class="del" data-m="'+esc(k)+'" onclick="delRule(this.dataset.m)" title="删除">×</button></div>';
  }).join("");
}

async function addRule() {
  const m = $("rule-model").value.trim();
  const g = $("rule-gw").value.trim();
  if (!m || !g) { show("模型名和上游都要填", false); return; }
  if (busy) return;
  busy = true;
  show("保存中…", true);
  try {
    await api("/v0/resource/plugins/cpagw-gateway/rules?model=" + encodeURIComponent(m) + "&gateway=" + encodeURIComponent(g));
    show("规则已保存: " + m, true);
    refresh(true);
  } catch (e) { show("保存失败: " + e.message, false); }
  finally { busy = false; }
}

async function delRule(m) {
  if (busy) return;
  busy = true;
  show("删除中…", true);
  try {
    await api("/v0/resource/plugins/cpagw-gateway/rules?model=" + encodeURIComponent(m) + "&gateway=-");
    show("已删除: " + m, true);
    refresh(true);
  } catch (e) { show("删除失败: " + e.message, false); }
  finally { busy = false; }
}

async function saveFallbacks() {
  const v = $("fbs").value.trim();
  if (!v) { show("请输入 fallback 链", false); return; }
  if (busy) return;
  busy = true;
  show("保存中…", true);
  try {
    await api("/v0/resource/plugins/cpagw-gateway/config?fallbacks=" + encodeURIComponent(v));
    show("fallback 链已保存", true);
    refresh(true);
  } catch (e) { show("保存失败: " + e.message, false); }
  finally { busy = false; }
}

refresh();
</script>
</body>
</html>`
}
