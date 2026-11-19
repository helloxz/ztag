# ZTAG

基于大模型的图片内容识别与风险审核服务。通过统一 HTTP API 调用：

- **图片分析**：提取关键词、描述，并输出内容分类与风险等级
- **风险审核**：图片 / 文本场景分类，返回 0~100 风险评分
- **多渠道模型**：支持 OpenAI、Anthropic 兼容接口，支持主备渠道切换
- **Web 管理台**：在线配置渠道、API 密钥，并内置接口调试与 API 文档

---

## Docker 部署

镜像：`ghcr.io/helloxz/ztag`（linux/amd64）

### docker run

```bash
docker pull ghcr.io/helloxz/ztag:latest

docker run -d --name ztag -p 18080:18080 \
  -v "$PWD/data:/app/data" \
  -e TZ=Asia/Shanghai \
  ghcr.io/helloxz/ztag:latest
```

### Docker Compose（推荐）

```bash
# 创建目录
mkdir ztag && cd ztag
# 下载compose文件
wget https://raw.githubusercontent.com/helloxz/ztag/refs/heads/main/compose.yaml
# 启动
docker compose up -d
```

默认监听 `18080`。配置与日志保存在容器内 `/app/data`，挂载到宿主机后即可持久化（首次启动自动生成 `data/config.toml`）。

---

## Web 管理台快速上手

部署完成后访问：

```
http://<服务器地址>:18080/admin
```

首次访问会引导创建管理员账号，登录后按以下步骤配置：

> 生产环境建议配置好后关闭管理台：在 `data/config.toml` 的 `[admin]` 段设置 `enabled = false` 并重启，此后 `/admin` 下的页面、接口与静态资源一律 404；需要再次管理时把该项改回 `true` 并重启即可（该开关不支持热加载，也不在管理台内提供）。

### 1. 配置图像分析渠道

进入「图像分析」，至少填写 `default` 渠道：

| 字段 | 说明 |
|---|---|
| 协议类型 | `openai-chat` / `openai-response` / `anthropic` |
| Base URL | 模型服务的接口地址 |
| API Key | 对应服务的密钥 |
| 模型列表 | 模型标识，可填多个 |

> 备注：`backup` 渠道默认不启用（配置模板中整段注释），用于主渠道失败时自动切换，请确保配置的模型支持多模态（图像输入），否则无法识别！

### 2. 配置风险审核（可选）

进入「风险审核」开启总开关，添加决策渠道：

- `gemini-decision`：Gemini Decision API
- `openai-decision`：OpenAI 兼容 Decisions API
- `systemone-decision`：System One（Jev 系，如 OpenRouter 的 decisions 端点）

不配置时，图片分析接口仍可正常使用，风险审核接口会返回 `decision model is not configured`。

### 3. 配置 API 密钥（可选）

进入「API 密钥」开启鉴权并添加 Key。开启后调用 `/api/v1` 接口需携带：

```
Authorization: Bearer <api_key>
# 或
X-API-Key: <api_key>
```

### 4. 重启服务生效

除管理员账号密码外，配置保存后需在右上角账号菜单点击「重启服务」生效。

管理台还提供：

- **接口调试**：直接填写图片 URL / 上传文件或输入文本，一键调用并查看真实返回
- **API 文档**：左侧菜单查看完整接口字段说明

---

## API 文档

所有对外接口位于 `/api/v1`：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/image/analyze` | 图片分析：关键词、描述、分类 |
| POST | `/api/v1/image/decision` | 图片风险审核 |
| POST | `/api/v1/text/decision` | 文本风险审核 |

完整的请求字段、响应结构、分类枚举与错误信息见：

- [API 文档](docs/api.md)
- 或登录管理台后打开左侧「API 文档」

所有接口返回统一格式：

```json
{ "code": 200, "msg": "", "data": {} }
```

成功 `code = 200`，失败 `code = -1000` 且 `msg` 为英文原因。
