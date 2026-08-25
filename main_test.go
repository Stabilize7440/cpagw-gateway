package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setRules(t *testing.T, rules map[string][]string) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	st.ModelRules = rules
}

func decodeMgmt(t *testing.T, out []byte) string {
	t.Helper()
	var env envelope
	if json.Unmarshal(out, &env) != nil || !env.OK {
		t.Fatalf("bad envelope: %s", out)
	}
	var resp managementResponse
	if json.Unmarshal(env.Result, &resp) != nil {
		t.Fatalf("bad management result: %s", env.Result)
	}
	return string(resp.Body)
}

func TestMatchModelRule(t *testing.T) {
	cases := []struct {
		name  string
		rules map[string][]string
		model string
		want  []string
		found bool
	}{
		{"no rules", nil, "gpt-5", nil, false},
		{"exact match", map[string][]string{"gpt-5": {"baseten"}}, "gpt-5", []string{"baseten"}, true},
		{"no match", map[string][]string{"gpt-5": {"baseten"}}, "claude-4", nil, false},
		{"wildcard", map[string][]string{"gpt-*": {"nebius"}}, "gpt-5.1-codex", []string{"nebius"}, true},
		{"exact beats wildcard", map[string][]string{"gpt-*": {"nebius"}, "gpt-5": {"baseten"}}, "gpt-5", []string{"baseten"}, true},
		{"longer prefix wins", map[string][]string{"gpt-*": {"nebius"}, "gpt-5.1*": {"fireworks"}}, "gpt-5.1-codex", []string{"fireworks"}, true},
		{"empty rule ignored", map[string][]string{"x": {}}, "x", nil, false},
		{"star-only matches all", map[string][]string{"*": {"morph"}}, "anything", []string{"morph"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setRules(t, c.rules)
			got, found := matchModelRule(c.model)
			if found != c.found {
				t.Fatalf("found = %v, want %v", found, c.found)
			}
			if found && strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
	setRules(t, nil)
}

func TestNormalizeRequestModelRules(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"model": "x", "messages": []string{}})
	tr := transformRequest{FromFormat: "openai", ToFormat: "openai", Model: "cline-pass/gpt-5.1", Body: body}

	mu.Lock()
	st.Gateway = []string{"baseten"}
	st.ModelRules = map[string][]string{"gpt-5.1": {"togetherai"}}
	mu.Unlock()

	// payloadResponse.Body 为 []byte，JSON 反序列化时自动 base64 解码
	injectedGateways := func(model string) []string {
		tr.Model = model
		raw, _ := json.Marshal(tr)
		out, err := normalizeRequest(raw)
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if json.Unmarshal(out, &env) != nil || !env.OK {
			t.Fatalf("bad envelope: %s", out)
		}
		var pr payloadResponse
		if json.Unmarshal(env.Result, &pr) != nil {
			t.Fatal("bad result")
		}
		var payload map[string]any
		if json.Unmarshal(pr.Body, &payload) != nil {
			t.Fatal("bad body")
		}
		po, ok := payload["providerOptions"]
		if !ok {
			return nil // 未注入
		}
		only := po.(map[string]any)["gateway"].(map[string]any)["only"].([]any)
		gw := make([]string, len(only))
		for i, g := range only {
			gw[i] = g.(string)
		}
		return gw
	}

	if gw := injectedGateways("cline-pass/gpt-5.1"); len(gw) != 1 || gw[0] != "togetherai" {
		t.Fatalf("rule not applied: %v", gw)
	}
	if gw := injectedGateways("cline-pass/claude-4"); len(gw) != 1 || gw[0] != "baseten" {
		t.Fatalf("global fallback broken: %v", gw)
	}
	if gw := injectedGateways("gpt-5.1"); gw != nil {
		t.Fatal("non cline-pass model must not be injected")
	}
}

func TestRequestCompleteSkipsRuledModels(t *testing.T) {
	orig := statePath
	statePath = filepath.Join(t.TempDir(), "state.json")
	defer func() { statePath = orig }()

	rc := requestCompletion{Model: "cline-pass/gpt-5.1", Outcome: "failed", StatusCode: 500, Error: "boom"}
	raw, _ := json.Marshal(rc)

	mu.Lock()
	st.Gateway = []string{"baseten"}
	st.Fallbacks = []string{"baseten", "togetherai"}
	st.FailThreshold = 2
	st.FailStreak = 1
	st.ModelRules = map[string][]string{"gpt-5.1": {"nebius"}}
	mu.Unlock()

	if _, err := handleRequestComplete(raw); err != nil {
		t.Fatal(err)
	}
	mu.RLock()
	streak1 := st.FailStreak
	mu.RUnlock()
	if streak1 != 1 {
		t.Fatalf("ruled model must not affect global fail streak: %d", streak1)
	}

	// 无规则模型照常计数（触发阈值切换，验证全局逻辑不受规则影响）
	rc.Model = "cline-pass/claude-4"
	raw, _ = json.Marshal(rc)
	if _, err := handleRequestComplete(raw); err != nil {
		t.Fatal(err)
	}
	mu.RLock()
	defer mu.RUnlock()
	if st.Gateway[0] != "togetherai" || st.FailStreak != 0 {
		t.Fatalf("non-ruled model should trigger fallback and reset streak: streak=%d gw=%v", st.FailStreak, st.Gateway)
	}
}

func TestRulesManagementAPI(t *testing.T) {
	orig := statePath
	statePath = filepath.Join(t.TempDir(), "state.json")
	defer func() { statePath = orig }()

	setRules(t, nil)

	setRule := func(model, gateway string) string {
		mr := managementRequest{
			Method: "GET", Path: "/v0/resource/plugins/cpagw-gateway/rules",
			Query: map[string][]string{"model": {model}, "gateway": {gateway}},
		}
		raw, _ := json.Marshal(mr)
		out, _ := handleManagement(raw)
		return decodeMgmt(t, out)
	}

	// 设置
	if body := setRule("gpt-5*", "baseten,togetherai"); !strings.Contains(body, "gpt-5*") {
		t.Fatalf("set rule failed: %s", body)
	}
	mu.RLock()
	got := st.ModelRules["gpt-5*"]
	mu.RUnlock()
	if len(got) != 2 || got[0] != "baseten" || got[1] != "togetherai" {
		t.Fatalf("rule not stored: %v", got)
	}

	// 读取
	mr := managementRequest{Method: "GET", Path: "/v0/resource/plugins/cpagw-gateway/rules"}
	raw, _ := json.Marshal(mr)
	out, _ := handleManagement(raw)
	if body := decodeMgmt(t, out); !strings.Contains(body, "gpt-5*") {
		t.Fatalf("list rules failed: %s", body)
	}

	// 删除
	setRule("gpt-5*", "-")
	mu.RLock()
	_, exists := st.ModelRules["gpt-5*"]
	mu.RUnlock()
	if exists {
		t.Fatal("rule not deleted")
	}
}

func TestStatePersistenceKeepsRules(t *testing.T) {
	dir := t.TempDir()
	orig := statePath
	statePath = filepath.Join(dir, "state.json")
	defer func() { statePath = orig }()

	mu.Lock()
	st.ModelRules = map[string][]string{"claude-*": {"nebius"}}
	mu.Unlock()
	if err := persistState(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	st.ModelRules = nil
	st.Gateway = []string{"x"}
	mu.Unlock()
	loadState()
	mu.RLock()
	rules := st.ModelRules
	mu.RUnlock()
	if rules == nil || len(rules["claude-*"]) != 1 || rules["claude-*"][0] != "nebius" {
		t.Fatalf("model rules not persisted/reloaded: %v", rules)
	}

	// 旧格式 state（无 model_rules 字段）兼容
	legacy := `{"gateway":["baseten"],"source":"state","fallbacks":["baseten","togetherai"],"fail_threshold":2}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	loadState()
	mu.RLock()
	defer mu.RUnlock()
	if st.ModelRules != nil {
		t.Fatal("legacy state should yield nil rules")
	}
	if st.Gateway[0] != "baseten" {
		t.Fatalf("legacy state broken: %v", st.Gateway)
	}
}

func TestParseProvidersFromError(t *testing.T) {
	full := `Failed to create stream: ... {"error":{"message":"No available providers match the 'only' filter: baseten. Available providers are: alibaba, baseten, crusoe, deepinfra, digitalocean","type":"invalid_request_error","param":{"modelId":"zai/glm-5.2"}}}`
	got := parseProvidersFromError(full)
	if len(got) != 5 || got[0] != "alibaba" || got[4] != "digitalocean" {
		t.Fatalf("parse full error: %v", got)
	}
	// 列表在文本末尾（无尾引号）
	got = parseProvidersFromError("Available providers are: zai, nebius")
	if len(got) != 2 || got[1] != "nebius" {
		t.Fatalf("parse tail list: %v", got)
	}
	// 无匹配
	if got := parseProvidersFromError("some unrelated error"); got != nil {
		t.Fatalf("should be nil: %v", got)
	}
	// 空文本
	if got := parseProvidersFromError(""); got != nil {
		t.Fatalf("empty should be nil: %v", got)
	}
}

func TestProvidersManagementAPI(t *testing.T) {
	orig := statePath
	statePath = filepath.Join(t.TempDir(), "state.json")
	defer func() { statePath = orig }()

	mu.Lock()
	st.ModelProviders = nil
	mu.Unlock()

	call := func(query map[string][]string) string {
		mr := managementRequest{
			Method: "GET", Path: "/v0/resource/plugins/cpagw-gateway/providers",
			Query: query,
		}
		raw, _ := json.Marshal(mr)
		out, _ := handleManagement(raw)
		return decodeMgmt(t, out)
	}

	// 默认预置（无自定义时 merged 返回默认 3 模型）
	body := call(nil)
	if !strings.Contains(body, "glm-5.2") || !strings.Contains(body, "glm-5.3") || !strings.Contains(body, "kimi-k3") {
		t.Fatalf("default providers missing: %s", body)
	}
	// 默认 glm-5.3 只有 zai
	var resp struct {
		Providers map[string][]string `json:"providers"`
	}
	if json.Unmarshal([]byte(body), &resp) != nil {
		t.Fatal("bad providers response")
	}
	if len(resp.Providers["glm-5.3"]) != 1 || resp.Providers["glm-5.3"][0] != "zai" {
		t.Fatalf("default glm-5.3: %v", resp.Providers["glm-5.3"])
	}
	// 自定义覆盖
	call(map[string][]string{"model": {"glm-5.3"}, "providers": {"zai,wafer"}})
	mu.RLock()
	got := st.ModelProviders["glm-5.3"]
	mu.RUnlock()
	if len(got) != 2 || got[1] != "wafer" {
		t.Fatalf("custom providers not stored: %v", got)
	}
	// 删除自定义 → 回默认
	call(map[string][]string{"model": {"glm-5.3"}, "providers": {"-"}})
	mu.RLock()
	_, exists := st.ModelProviders["glm-5.3"]
	mu.RUnlock()
	if exists {
		t.Fatal("custom providers not deleted")
	}
	body = call(nil)
	var resp2 struct {
		Providers map[string][]string `json:"providers"`
	}
	if json.Unmarshal([]byte(body), &resp2) != nil {
		t.Fatal("bad providers response 2")
	}
	if len(resp2.Providers["glm-5.3"]) != 1 || resp2.Providers["glm-5.3"][0] != "zai" {
		t.Fatalf("reset to default failed: %v", resp2.Providers["glm-5.3"])
	}
}

func TestParseErrorEndpoint(t *testing.T) {
	mr := managementRequest{
		Method: "GET", Path: "/v0/resource/plugins/cpagw-gateway/parse-error",
		Query: map[string][]string{"text": {"No available providers match the 'only' filter: x. Available providers are: zai, nebius"}},
	}
	raw, _ := json.Marshal(mr)
	out, _ := handleManagement(raw)
	body := decodeMgmt(t, out)
	if !strings.Contains(body, "zai") || !strings.Contains(body, "nebius") {
		t.Fatalf("parse-error endpoint failed: %s", body)
	}
}
