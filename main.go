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
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const abiVersion uint32 = 1

const (
	pluginName    = "cpagw-gateway"
	pluginVersion = "0.3.1"
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
	Gateway       []string       `json:"gateway"`         // providerOptions.gateway.only 当前值（单值锁定）
	Source        string         `json:"source"`          // state(手动) / auto(自动 fallback) / config / default
	Fallbacks     []string       `json:"fallbacks"`       // fallback 链：链首为主力
	FailThreshold int            `json:"fail_threshold"`  // 连续失败触发阈值
	FailStreak    int            `json:"fail_streak"`     // 当前连续失败计数
	SuccessStreak int            `json:"success_streak"`  // 自动切换后连续成功计数（满 recoverStreakTarget 恢复主力）
	LastEvent     *fallbackEvent `json:"last_event,omitempty"`
}

var (
	mu                 sync.RWMutex
	st                 pluginState
	statePath          string
	fallbacksFromState bool // 运行时标记：state 文件是否显式提供过 fallbacks（优先于 config.yaml）
)

func defaultFallbacks() []string { return []string{"baseten", "togetherai", "fireworks"} }

func stateFilePath() string {
	// 状态文件放在 plugins 目录下（二进制 /CLIProxyAPI/CLIProxyAPI 的 dirname + plugins/）
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
			`{"Path":"/switch","Description":"切换上游: ?gateway=baseten[,togetherai]"}` +
			`]}`)
	case "management.handle":
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ---------- request.normalize：注入 providerOptions ----------

func normalizeRequest(request []byte) ([]byte, error) {
	var tr transformRequest
	if err := json.Unmarshal(request, &tr); err != nil {
		return okEnvelopeJSON1(`{"Body":""}`), nil
	}
	model := strings.TrimSpace(tr.Model)
	mu.RLock()
	gw := make([]string, len(st.Gateway))
	copy(gw, st.Gateway)
	mu.RUnlock()

	if !strings.HasPrefix(model, modelPrefix) || len(tr.Body) == 0 || len(gw) == 0 {
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
			"last_event":     st.LastEvent,
			"plugin":         pluginName,
			"version":        pluginVersion,
		})
		mu.RUnlock()
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
  <div style="font-weight:600; margin-bottom:2px">快捷切换（单选）</div>
  <div class="sub" style="margin-bottom:0">手动切换后自动 fallback 不再干预</div>
  <div class="grid" id="grid"></div>
</div>

<div class="card">
  <div style="font-weight:600; margin-bottom:8px">自定义候选（逗号分隔，多值 = 候选池）</div>
  <div class="row">
    <input id="custom" type="text" placeholder="例如: baseten, togetherai, fireworks">
    <button class="action-btn" onclick="applyCustom()">应用</button>
  </div>
</div>

<div class="actions">
  <button class="action-btn ghost" onclick="refresh()">刷新</button>
  <span id="msg"></span>
</div>

<script>
const PROVIDERS = ["baseten","digitalocean","fireworks","modal","moonshotai","morph","nebius","togetherai"];
let current = [];
let busy = false;

function $(id) { return document.getElementById(id); }
function show(msg, ok) { const m = $("msg"); m.textContent = msg; m.className = ok ? "ok" : "err"; }
function esc(s) { return String(s).replace(/[&<>"']/g, c => ({'&':"&amp;",'<':"&lt;",'>':"&gt;",'"':"&quot;","'":"&#39;"}[c])); }

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
    $("custom").value = current.join(", ");
    renderGrid();
    if (!silent) show("已刷新", true);
  } catch (e) { if (!silent) show("加载失败: " + e.message, false); }
}

function renderGrid() {
  $("grid").innerHTML = PROVIDERS.map(p => {
    const active = current.length === 1 && current[0] === p;
    return '<button class="btn'+(active?" active":"")+'" onclick="switchTo(\''+esc(p)+'\')">'+esc(p)+'</button>';
  }).join("");
}

async function switchTo(p) {
  if (busy) return;
  busy = true;
  show("切换中…", true);
  try {
    const d = await api("/v0/resource/plugins/cpagw-gateway/switch?gateway=" + encodeURIComponent(p));
    current = d.gateway || [];
    show("已切换 → " + current.join(", "), true);
    refresh(true);
  } catch (e) { show("切换失败: " + e.message, false); }
  finally { busy = false; }
}

async function applyCustom() {
  const v = $("custom").value.trim();
  if (!v) { show("请输入上游名称", false); return; }
  if (busy) return;
  busy = true;
  show("应用中…", true);
  try {
    const d = await api("/v0/resource/plugins/cpagw-gateway/switch?gateway=" + encodeURIComponent(v));
    show("已应用 → " + (d.gateway || []).join(", "), true);
    refresh(true);
  } catch (e) { show("应用失败: " + e.message, false); }
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
