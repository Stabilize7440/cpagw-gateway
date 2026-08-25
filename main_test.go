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
