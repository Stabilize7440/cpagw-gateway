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
	"strings"
	"sync"
	"unsafe"
)

const abiVersion uint32 = 1

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

// rpcLifecycleRequest 与 CPA host 的 rpc_schema.go 对应（config_yaml 为 base64）
type rpcLifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// RequestTransformRequest 与 sdk/pluginapi/types.go 对应
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

// ---------- 插件状态 ----------

var (
	mu         sync.RWMutex
	gateway    []string // providerOptions.gateway.only 的候选列表
	cfgModel   string   // 匹配的模型前缀
)

const (
	pluginName    = "cpagw-gateway"
	pluginVersion = "0.1.0"
	defaultModelPrefix = "cline-pass/"
)

// 默认配置：baseten 主力
func setDefaultConfig() {
	gateway = []string{"baseten"}
	cfgModel = defaultModelPrefix
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		// 解析配置（reconfigure 时携带 config_yaml）
		if method == "plugin.reconfigure" && len(request) > 0 {
			var lr rpcLifecycleRequest
			if err := json.Unmarshal(request, &lr); err == nil {
				applyConfig(lr.ConfigYAML)
			}
		}
		return okEnvelopeJSON(`{"schema_version":1,"metadata":{"Name":"` + pluginName + `","Version":"` + pluginVersion +
			`","Author":"Stabilize7440","GitHubRepository":"https://github.com/router-for-me/CLIProxyAPI","ConfigFields":[]},"capabilities":{"request_normalizer":true}}`)
	case "request.normalize":
		return normalizeRequest(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// applyConfig 解析 YAML 配置（gopkg.in/yaml.v3 风格，手动解析最小子集：gateway 列表）
func applyConfig(configYAML []byte) {
	if len(configYAML) == 0 {
		return
	}
	// config_yaml 是 YAML，例如：
	//   enabled: true
	//   priority: 0
	//   gateway:
	//     - baseten
	//     - togetherai
	// 手动解析 gateway 列表（缩进敏感，只支持简单结构）
	lines := strings.Split(string(configYAML), "\n")
	var gw []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") && !strings.HasPrefix(trimmed, "--") {
			item := strings.Trim(strings.TrimPrefix(trimmed, "- "), `"' `)
			if item != "" && !strings.HasPrefix(item, "#") {
				gw = append(gw, item)
			}
		}
	}
	mu.Lock()
	if len(gw) > 0 {
		gateway = gw
	}
	mu.Unlock()
}

// normalizeRequest 处理 request.normalize：匹配 cline-pass/* 注入 providerOptions
func normalizeRequest(request []byte) ([]byte, error) {
	var tr transformRequest
	if err := json.Unmarshal(request, &tr); err != nil {
		return okEnvelopeJSON1(`{"Body":""}`), nil
	}
	model := strings.TrimSpace(tr.Model)
	mu.RLock()
	prefix := cfgModel
	gw := make([]string, len(gateway))
	copy(gw, gateway)
	mu.RUnlock()

	if !strings.HasPrefix(model, prefix) || len(tr.Body) == 0 || len(gw) == 0 {
		// 不匹配或无需修改：原样返回
		return okEnvelopeJSON1(base64BodyJSON(tr.Body)), nil
	}

	// 解析 OpenAI chat.completions body 并注入 providerOptions
	var payload map[string]any
	if err := json.Unmarshal(tr.Body, &payload); err != nil {
		return okEnvelopeJSON1(base64BodyJSON(tr.Body)), nil
	}
	only := make([]any, len(gw))
	for i, g := range gw {
		only[i] = g
	}
	payload["providerOptions"] = map[string]any{
		"gateway": map[string]any{
			"only": only,
		},
	}
	newBody, err := json.Marshal(payload)
	if err != nil {
		return okEnvelopeJSON1(base64BodyJSON(tr.Body)), nil
	}
	return okEnvelopeJSON1(base64BodyJSON(newBody)), nil
}

func okEnvelopeJSON1(result string) []byte {
	raw, _ := json.Marshal(envelope{OK: true, Result: json.RawMessage(result)})
	return raw
}

func base64BodyJSON(body []byte) string {
	return `{"Body":"` + base64.StdEncoding.EncodeToString(body) + `"}`
}

func okEnvelopeJSON(result string) ([]byte, error) {
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(result)})
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
	setDefaultConfig()
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
