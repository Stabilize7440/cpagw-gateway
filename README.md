# cpagw-gateway

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 ClinePass 上游网关管理插件。
通过 `request_normalizer` 钩子在请求进入 ClinePass 网关前注入 `providerOptions.gateway.only`，
实现**上游供应商的指定与热切换**（baseten / togetherai / fireworks / moonshotai / nebius / modal / morph / digitalocean），
支持**按模型指定不同上游**。

## 为什么需要它

ClinePass（api.cline.bot）是 Cline 的订阅网关，同一个模型（如 `cline-pass/kimi-k3`）背后有多个推理上游。
请求体带 `providerOptions.gateway.only: ["<上游>"]` 即可锁定上游，但：

- ClinePass 官方文档未公开该参数，客户端一般不会传
- 切换上游需要改 CPA 配置并重启，很麻烦

本插件自动为所有 `cline-pass/*` 模型注入该参数，并暴露管理 API 支持**秒级热切换**。

## 工作原理

```
客户端 → CPA (8317) → [cpagw-gateway 插件] → ClinePass 网关 → 指定上游
                         ↑ request.normalize
                         匹配 model 前缀 "cline-pass/" → 注入 providerOptions.gateway.only
                         查模型级规则 -> 注入 providerOptions.gateway.only
                         有规则：该模型专属上游（单值锁定 / 多值候选池）
                         无规则：全局上游 + 自动 fallback
```

- 插件类型：`request_normalizer`（openai→openai 有原生 translator，`TranslateRequest` 钩子不触发，但 `NormalizeRequest` 总会触发）
- 切换：`PATCH /v0/management/plugins/cpagw-gateway/config` → 写盘 + `plugin.reconfigure` → 秒级生效，**无需重启**
- 仅匹配 `cline-pass/*` 模型，其他上游（火山方舟等）不受影响

## 部署

### 1. 编译

需要 CPA 容器为 glibc 环境（Debian），用 bookworm 镜像编译：

```bash
./build.sh          # 产出 cpagw-gateway.so
```

### 2. 放置插件

```bash
mkdir -p plugins/linux/amd64
cp cpagw-gateway.so plugins/linux/amd64/
```

### 3. docker-compose 挂载

```yaml
volumes:
  - ./plugins:/CLIProxyAPI/plugins
```

### 4. config.yaml 启用插件

```yaml
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    cpagw-gateway:
      enabled: true
      gateway:           # 插件自己的配置：上游候选（单值 = 严格锁定）
        - baseten
```

### 5. 重启生效

```bash
docker compose up -d
# 日志应出现: pluginhost: plugin registered plugin_id=cpagw-gateway
```

## 使用

### 模型级规则（按模型指定上游）

为不同模型指定专属上游：规则匹配优先于全局配置，未命中规则的模型走全局上游。

```bash
# 设置规则：模型名支持尾部 * 通配（精确匹配优先，长前缀优先）
curl "http://localhost:18317/v0/resource/plugins/cpagw-gateway/rules?model=gpt-5.1-codex&gateway=baseten"
curl "http://localhost:18317/v0/resource/plugins/cpagw-gateway/rules?model=claude-*&gateway=nebius"
curl "http://localhost:18317/v0/resource/plugins/cpagw-gateway/rules?model=kimi-k3&gateway=moonshotai,togetherai"

# 查看 / 删除
curl "http://localhost:18317/v0/resource/plugins/cpagw-gateway/rules"
curl "http://localhost:18317/v0/resource/plugins/cpagw-gateway/rules?model=claude-*&gateway=-"
```

- 规则值：单上游 = 严格锁定；多上游 = 候选池动态选优（同全局语义）
- 模型名写裸名（不含 `cline-pass/` 前缀），如 `gpt-5.1-codex`、`claude-*`
- 有规则的模型不参与全局 fail-streak 自动 fallback（上游选择交给规则候选池）
- 规则持久化在插件状态文件，跨重启保留；CPAMP 面板的「ClinePass 网关」页可直接编辑

### 热切换（无需重启）

```bash
# 经 cpamp 面板代理（推荐，面板持有 CPA 管理密钥）
curl -X PATCH http://localhost:18317/v0/management/plugins/cpagw-gateway/config \
  -H "Authorization: Bearer $CPAMP_KEY" -H "Content-Type: application/json" \
  -d '{"gateway":["togetherai"]}'
```

或使用随附的 CLI 工具：

```bash
export CPAMP_KEY=你的面板AdminKey
export CPA_API_KEY=你的CPA访问key

python3 cpagw.py status                    # 当前配置 + 实测路由
python3 cpagw.py switch togetherai         # 一键热切换
python3 cpagw.py switch baseten            # 切回
python3 cpagw.py test                      # 连续 5 次实测路由分布

python3 cpagw.py rules                     # 列出模型级规则
python3 cpagw.py rules 'gpt-5*' baseten    # 设置规则（模型名含 * 时加引号防 shell 展开）
python3 cpagw.py rules kimi-k3 moonshotai togetherai
python3 cpagw.py rules -d 'gpt-5*'         # 删除规则
```

### 验证路由

请求任一 cline-pass 模型，响应体自带路由自证：

```json
{"provider_metadata": {"gateway": {"routing": {"finalProvider": "baseten", ...}}}}
```

## 已知限制

- ClinePass 网关的 `only` 多值语义是「候选池动态选优」（按实时延迟/健康加权），**不是顺序 fallback**；严格锁定请用单值
- 插件配置变更通过 CPA 的配置热重载传播，Docker Desktop bind mount 下 Windows 侧直接改文件不触发 inotify，请走管理 API 或重启容器
- `model_rules` 存入插件状态文件而非 config.yaml：状态文件由插件自身读写，与 CPA 配置热重载无关

## 兼容性

- CPA ≥ v7.2.125（首个包含插件宿主 internal/pluginhost 的版本）
- 编译环境：golang:1.24-bookworm（glibc）

## 相关

- 管理面板：https://github.com/seakee/CPA-Manager-Plus
- CPA 插件示例：`examples/plugin/`（request-normalizer 等）
