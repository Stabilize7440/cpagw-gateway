#!/usr/bin/env python3
"""
cpagw - ClinePass 上游网关管理工具（经 cpamp 面板代理操作 CPA 插件）

用法:
  cpagw list                     # 插件状态 + 当前 gateway 配置
  cpagw switch baseten           # 切换主力上游（热生效，无需重启）
  cpagw switch baseten togetherai  # 多值候选池（网关动态选优，不保证顺序）
  cpagw status                   # 当前配置 + 实测一次请求的路由结果
  cpagw test                     # 连续 5 次实测，看路由分布
  cpagw rules                    # 列出模型级规则
  cpagw rules gpt-5* baseten     # 设置规则：模型 -> 上游（多值空格分隔 = 候选池）
  cpagw rules -d gpt-5*          # 删除规则

密钥来源（优先级）: 环境变量 CPAMP_KEY > --key 参数 > 本文件默认值
"""
import json
import os
import subprocess
import sys
import time
import urllib.parse
import urllib.request

PANEL = os.environ.get("CPAMP_URL", "http://localhost:18317")
CPA = os.environ.get("CPA_URL", "http://localhost:8317")
CPA_KEY = os.environ.get("CPA_API_KEY", "")  # 必填：从环境变量提供
PLUGIN_ID = "cpagw-gateway"

DEFAULT_PANEL_KEY = os.environ.get("CPAMP_KEY", "")  # 必填：从环境变量提供

VALID_PROVIDERS = [
    "baseten", "digitalocean", "fireworks", "modal",
    "moonshotai", "morph", "nebius", "togetherai", "zai",
]


def panel_key(args):
    if "--key" in args:
        return args[args.index("--key") + 1]
    return os.environ.get("CPAMP_KEY", DEFAULT_PANEL_KEY)


def mgmt(method, path, body=None, key=None):
    req = urllib.request.Request(
        PANEL + path, method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode())


def get_plugin(key):
    data = mgmt("GET", f"/v0/management/plugins", key=key)
    for p in data.get("plugins", []):
        if p["id"] == PLUGIN_ID:
            return p
    return None


def resource(path, key=None):
    """插件资源路由（/v0/resource/...）：CPA 侧公开，key 可选"""
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = f"Bearer {key}"
    req = urllib.request.Request(PANEL + path, method="GET", headers=headers)
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode())


def chat_cpa(model, max_tokens=8):
    body = {"model": model, "messages": [{"role": "user", "content": "say ok"}], "max_tokens": max_tokens}
    req = urllib.request.Request(
        CPA + "/v1/chat/completions", method="POST",
        data=json.dumps(body).encode(),
        headers={"Authorization": f"Bearer {CPA_KEY}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            d = json.loads(resp.read().decode())
        if "data" in d:
            return d["data"]["choices"][0]["message"].get("provider_metadata", {}).get("gateway", {}).get("routing", {}).get("finalProvider")
    except Exception:
        pass
    return "ERR"


def cmd_list(args):
    key = panel_key(args)
    p = get_plugin(key)
    if not p:
        print(f"插件 {PLUGIN_ID} 未找到")
        return 1
    print(f"插件:   {p['metadata']['name']} v{p['metadata']['version']}")
    print(f"状态:   registered={p['registered']} enabled={p['enabled']}")
    cfg = mgmt("GET", f"/v0/management/plugins/{PLUGIN_ID}/config", key=key)
    print(f"配置:   {json.dumps(cfg, ensure_ascii=False)}")
    return 0


def cmd_switch(args):
    providers = [a for a in args if not a.startswith("--") and a not in ("switch", "list", "status", "test")]
    if not providers:
        print("用法: cpagw switch <provider...>  可选: " + ", ".join(VALID_PROVIDERS))
        return 1
    for pv in providers:
        if pv not in VALID_PROVIDERS:
            print(f"未知上游: {pv}（可选: {', '.join(VALID_PROVIDERS)}）")
            return 1
    key = panel_key(args)
    out = mgmt("PATCH", f"/v0/management/plugins/{PLUGIN_ID}/config", body={"gateway": providers}, key=key)
    print(f"已切换 gateway -> {providers}（{out.get('status', 'ok')}）")
    time.sleep(1)
    fp = chat_cpa("cline-pass/kimi-k3")
    print(f"实测:   cline-pass/kimi-k3 -> {fp}")
    return 0


def cmd_status(args):
    key = panel_key(args)
    p = get_plugin(key)
    if not p:
        print(f"插件 {PLUGIN_ID} 未找到")
        return 1
    cfg = mgmt("GET", f"/v0/management/plugins/{PLUGIN_ID}/config", key=key)
    gw = (cfg.get("gateway") or [])
    print(f"当前 gateway: {gw}")
    for m in ["cline-pass/kimi-k3", "cline-pass/glm-5.2"]:
        fp = chat_cpa(m)
        print(f"  实测 {m}: {fp}")
    return 0


def cmd_rules(args):
    key = panel_key(args)
    rest = args[1:]
    if "--key" in rest:
        i = rest.index("--key")
        rest = rest[:i] + rest[i + 2:]
    base = f"/v0/resource/plugins/{PLUGIN_ID}/rules"
    if not rest:
        rules = (resource(base, key) or {}).get("rules") or {}
        if not rules:
            print("暂无模型规则（全部模型走全局 gateway）")
            return 0
        print("模型规则（优先于全局 gateway）:")
        for m, gws in sorted(rules.items()):
            print(f"  {m:<26} -> {', '.join(gws)}")
        return 0
    if rest[0] in ("-d", "--del"):
        if len(rest) != 2:
            print("用法: cpagw rules -d <model>")
            return 1
        resource(f"{base}?model={urllib.parse.quote(rest[1])}&gateway=-", key)
        print(f"已删除规则: {rest[1]}")
        return 0
    if len(rest) < 2:
        print("用法: cpagw rules <model> <provider...>  |  cpagw rules -d <model>")
        print("模型名支持尾部 * 通配（精确匹配优先），可选上游: " + ", ".join(VALID_PROVIDERS))
        return 1
    model, providers = rest[0], rest[1:]
    for pv in providers:
        if pv not in VALID_PROVIDERS:
            print(f"未知上游: {pv}（可选: {', '.join(VALID_PROVIDERS)}）")
            return 1
    resource(f"{base}?model={urllib.parse.quote(model)}&gateway={urllib.parse.quote(','.join(providers))}", key)
    print(f"已设置规则: {model} -> {', '.join(providers)}")
    return 0


def cmd_test(args):
    key = panel_key(args)
    cfg = mgmt("GET", f"/v0/management/plugins/{PLUGIN_ID}/config", key=key)
    print(f"当前 gateway: {cfg.get('gateway')}")
    from collections import Counter
    dist = Counter()
    for i in range(5):
        fp = chat_cpa("cline-pass/kimi-k3")
        dist[fp] += 1
        print(f"  #{i+1}: {fp}")
        time.sleep(1)
    print(f"分布: {dict(dist)}")
    return 0


def main():
    args = sys.argv[1:]
    if not args:
        print(__doc__)
        return 1
    cmd = args[0]
    if cmd == "list":
        return cmd_list(args)
    if cmd == "switch":
        return cmd_switch(args)
    if cmd == "status":
        return cmd_status(args)
    if cmd == "test":
        return cmd_test(args)
    if cmd == "rules":
        return cmd_rules(args)
    print(__doc__)
    return 1


if __name__ == "__main__":
    sys.exit(main())
