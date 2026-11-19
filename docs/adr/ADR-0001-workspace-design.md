# ADR-0001：IDP Workspace 的设计

状态：已接受（2026-11-04）
范围：idp-workspace（企业工作入口：工作空间、应用注册与安装、工作收件箱、工作上下文、动作契约、应用桥）
依据：IDP TELOS 系统架构 v1.0（五大独立子系统）、仓库工程架构 v1.0（idp-workspace = UI / Experience，所有任务状态只经 HOST / EER API 访问）、idp-host ADR-0001、idp-eer 运行时 API（`contracts/openapi/runtime-v1.yaml`）、idp-cmi ADR-0001

## 1. 背景

架构基线把 Workspace 定为企业的工作入口："独立企业应用平台，不是简单 AI 聊天 UI"，组成为 App Shell、App Registry、H5 App Runtime、App Bridge SDK、Work Context、Work Inbox、Action Contract、审批与人机协作。它权威持有的是工作空间、应用安装与配置、界面与协作状态、工作 Inbox；它**不得**持有业务领域事务、Agent Harness 状态、EER Checkpoint，也不得把界面缓存当作审批记录。允许的调用：Workspace → HOST（Agent API / AG-UI），Workspace → EER（Task / Approval 公共 API）。

开工前核对了两个下游的公开接口（2026-11-04）：

- **idp-host**：`POST /v1/agents/{ref}/runs` 以 AG-UI `RunAgentInput` 建运行（`Idempotency-Key`），`GET /v1/runs/{id}` 读状态，`GET /v1/runs/{id}/events` 以 SSE 流式给出事件；JWT 的 `tenant_id` 与 `roles`（`host-user`）声明决定身份。没有租户级事件流，也没有跨线程的待确认列表。
- **idp-eer**：`POST /v1/executions/{id}/approval` 以 `{tenant_id, decision: approved|rejected, actor_id}` 对 T2 审批作决定（202；409 不在等待审批；403 actor 与令牌不符）；租户与 actor 由运行时配置按 (`iss`, `sub`) 映射。**没有**列出待审批或执行的接口，也没有对外的事件订阅或 webhook。

因此 Workspace 不能"拉"出全企业的待办；收件箱必须由拥有事项的系统**推入**。

## 2. 决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | **单一 Go 服务 + 前端包**：`idp-workspace` 是一个 Go 二进制（API，PostgreSQL），前端以 TypeScript 包交付（M1：`packages/bridge` 应用桥 SDK；M2：`apps/web` 壳）。工作空间是租户内的命名空间，成员带工作空间内角色（小写名），租户管理员可管理全部工作空间 | 与 HOST / CMI 同一套运维形态（单二进制、RLS、静态镜像）；前端与后端独立发布 |
| D2 | **应用注册与安装**：应用以清单 `idp.workspace.app/v1` 发布（名称、语义版本、标题、发布者、可选 H5 入口 URL、申请的桥能力、动作契约、可选配置 JSON Schema）。发布不可变：同名同版本再发布只在摘要（规范化 JSON 的 sha256）相同时幂等成功，否则 409；发布可被撤销（撤销后已安装的仍在但不可用）。安装把某版本装进工作空间并带配置（按清单的配置 Schema 校验，带 `revision` 与 If-Match）；每个工作空间每个应用一份安装，升级即改版本 | 应用是第三方代码，需可追溯、可撤销；配置错误应在安装时而非运行时发现 |
| D3 | **工作收件箱是视图，不是决定**：生产者（角色 `ws-producer` 的服务：EER 侧适配器、HOST 侧适配器、应用后端）按 `(producer, externalRef)` 幂等地放入事项：种类 approval / task / run / notification，标题、摘要、权威对象引用 `ref {system, kind, id, url?}`、指派（主体与 / 或工作空间角色，均空 = 全体成员）、优先级、到期、可执行的动作（应用 + 动作 + 预置输入）、实体引用。只有生产者能把事项置为 resolved / withdrawn 并写结果；成员只能标记已读 / 归档（自己的视图）。成员从事项发起动作时，Workspace 把动作转给拥有者，**不**改事项状态——由拥有者确认后回写 | 架构基线："不可将 UI 缓存当作审批记录"；EER 无列表与事件接口，推入是唯一不伪造权威的方式 |
| D4 | **动作契约**：清单中的动作声明输入 JSON Schema、目标与可调用的工作空间角色。调用 `POST /v1/workspaces/{ws}/actions/invoke`：校验成员资格、安装存在且发布未撤销、角色、输入 Schema（与事项预置输入合并，成员字段优先），再转发：`host.run` → HOST `POST /v1/agents/{agent}/runs`（AG-UI 输入：动作输入作为 user 消息，工作上下文的实体作为 context）；`eer.decision` → EER `POST /v1/executions/{id}/approval`（执行 id 取自事项 `ref`（system eer、kind execution）或输入的 `executionId`，actor 为调用者主体）；`webhook` → 应用后端 URL（HMAC-SHA256 签名 `Idp-Signature: t=…,v1=…`）。每次调用记录为 Invocation（输入摘要、目标、结果引用或错误），按 `Idempotency-Key` 幂等。下游凭据按目标配置：`forward`（转发调用者令牌，需身份提供方签发含下游 audience 的令牌）或 `static`（服务令牌文件）；令牌交换（RFC 8693）留到 M2 | 动作必须由拥有决定的系统执行；Workspace 只负责"谁、在哪、以什么输入、被谁接收" |
| D5 | **工作上下文与应用桥**：每个成员在每个工作空间有一份工作上下文（实体引用、当前应用、线程、当前事项，带 `version`）。H5 应用在沙箱 iframe 中运行（`sandbox="allow-scripts allow-forms allow-same-origin"`：应用保留自己的源，桥才能核对来源，应用也能用自己的存储；不给顶层导航、弹窗与下载；应用入口与壳同源时壳拒绝装入，因为那时 same-origin 会让应用进入壳），与壳之间只走 `postMessage` 协议 `idp.bridge/v1`：请求 / 响应 / 事件三种消息；方法 `bridge.hello`、`context.get`、`context.set`、`actions.invoke`、`inbox.list`、`ui.notify`，事件 `context.changed`。壳只接受来自安装入口源（origin）的消息，只执行清单申请过的能力，绝不把用户令牌交给应用 | 第三方页面不可信；能力由清单声明并在安装时可见，桥是唯一通道 |
| D6 | **身份与角色**：JWT（生产 JWKS，开发 HS256），声明 `tenant_id`、`roles`；角色 `ws-admin`（工作空间、应用、成员、安装）、`ws-member`（在所属工作空间里工作）、`ws-producer`（放入与结束事项）。成员资格由 Workspace 自己的成员表决定 | 与 HOST / CMI 相同的身份约定 |
| D7 | **存储**：PostgreSQL，所有租户表 FORCE RLS，服务角色不得是超级用户或 BYPASSRLS；管理动作写审计 | 与 HOST / CMI 一致 |
| D8 | **契约** `idp.telos.workspace` 1.0.0：AppManifest（含 ActionContract、Target）、AppRelease、Installation、InboxItem、WorkContext、Invocation、BridgeMessage、Envelope（错误）。生产者副本在 `api/contracts/`，实现侧一致性测试钉住，登记到 idp-contracts | 应用开发者、生产者与壳都依赖这些形状 |

## 3. 里程碑

- **M1**（本 ADR）：服务（工作空间与成员、应用注册与安装、收件箱、工作上下文、动作调用与三种目标）、契约 1.0.0、`packages/bridge`（壳端与应用端，零依赖 TypeScript，Node 内置测试）、单元 / 契约 / 集成测试（假 HOST、假 EER、假 webhook）、镜像构建、CI。
- **M2**：`apps/web` 壳（React：收件箱、应用启动器、应用框架与桥）、应用令牌（Workspace 签发的短期、受众为应用后端的令牌）、收件箱实时推送（SSE）、HOST 运行结果自动入箱、下游令牌交换、与真实 idp-host 的 compose 联调。
- **之后**：EER 侧生产者适配器（属 idp-eer：T2 审批进入 Waiting 时推入 Workspace），需 EER 自己的 ADR。

## 4. 不做

- 不持有审批、运行、任务的状态（D3）；不缓存下游对象当作事实。
- 不做通用 API 网关：动作目标只有三种，且都按清单声明。
- 不在 Workspace 内做 Agent 推理或上下文检索：那是 HOST 与 CMI。

## 5. M1 实施结果（2026-11-04，WSL 实测）

- D1、D6、D7：单一 Go 服务（`cmd/idp-workspace`：serve / migrate / ensure-login / seed-tenant / dev-token），PostgreSQL 九张租户表全部 FORCE RLS，服务角色为超级用户或 BYPASSRLS 时拒绝启动；工作空间创建者成为 owner 成员；非成员访问工作空间一律 404。
- D2：发布按规范化清单的 sha256 判定幂等（键序与空白不同的同一清单得到同一摘要），同版本异内容 409；清单、动作输入与配置 Schema 在发布时编译（只允许对象 Schema，拒绝外部 `$ref`），入口与 webhook 必须 https（回环地址除外）；撤销后安装保留但不出现在成员的应用列表、不可调用、不可再安装。
- D3：事项按 `(producer, externalRef)` upsert（开放时 version + 1，已结束 409）；可见性 = 指派主体、指派角色与成员的工作空间角色相交，或无指派；结束只限生产者；成员标记只影响自己。
- D4：调用顺序为成员资格 → 安装与发布状态 → 动作 → 契约角色（管理员不豁免）→ 事项（开放、提供该动作、预置输入合并）→ 输入 Schema → 转发；失败以 `{error, invocation}` 应答并记录；`Idempotency-Key` 按请求哈希重放或 409。`host.run` 以调用者令牌（forward）建运行、run id 即幂等键、工作上下文的实体与线程进入 AG-UI 输入；`eer.decision` 以 `{tenant_id, decision, actor_id = 调用者}` 调用 EER，执行 id 取自事项引用或输入（两者不一致时 422）；`webhook` 以 `Idp-Signature` 签名、从不携带用户令牌、不跟随重定向。
- D5：`packages/bridge`：协议解析与契约逐项一致（测试读取 `schema.json` 的方法与事件枚举）；壳端忽略其他源与其他 frame 的消息、未申请能力的请求以 `CAPABILITY_DENIED` 拒绝、处理器内部错误不外泄；应用端只认壳的源、请求超时与关闭时拒绝；webhook 签名在 Go 与 TypeScript 两端用同一向量钉住。
- D8：契约 `idp.telos.workspace` 1.0.0（19 个定义），示例由 Go 值生成，闭合性与 Go 结构逐字段一致，拒绝用例覆盖清单、事项、桥消息与调用记录。

**验证**：单元（schema、dispatch、config、契约）、集成（workspaces-and-members、app-registry、installations、inbox-is-a-view、work-context、actions-go-to-the-owner、revoked-and-uninstalled、audit）、应用桥 11 项（Node 24、`tsc --strict`）、镜像构建与 compose 端到端全部通过。

**局限（不标 PASS）**
- 与真实 idp-host、idp-eer 的联调未做：EER 侧要由 T2 审批推入事项，需要 idp-eer 的生产者适配器；转发令牌模式要求身份提供方签发含下游 audience 的令牌（令牌交换留到 M2）。
- 没有前端壳（M2 的 `apps/web`）；应用桥只以内存通道测试，未在浏览器 iframe 中实测。
- EER 的 actor 取调用者主体；若 EER 的 (iss, sub) 映射给出不同的 actor id，决定会被 EER 以 403 拒绝（记录为失败调用）。

## 6. M2 决策（2026-11-04）

| # | 决策 | 理由 |
|---|---|---|
| D9 | **Web 壳**：`apps/web`（React 19 + Vite + TypeScript），与 `packages/bridge` 同属仓库根的 npm workspaces。构建产物由 Go 服务从 `IDP_WORKSPACE_WEB_DIR` 提供（`/` 与 `/assets/*`；入口页带 CSP：`default-src 'self'`，`frame-src` 限 https 与回环地址，`connect-src 'self'`），镜像内置该目录。壳的功能：工作空间切换、收件箱（含事项动作与确认）、应用启动器、应用框架（沙箱 iframe + 桥壳端：上下文读写、动作调用、收件箱列表、通知）。M2 的登录是开发模式下粘贴令牌（存 sessionStorage）；OIDC 授权码 + PKCE 留到 M3 | 前端与服务同版本发布、同源，避免 CORS 与令牌跨源；登录与身份提供方对接是独立的一块 |
| D10 | **收件箱推送**：`GET /v1/workspaces/{ws}/inbox/stream` 以 SSE 推送调用者可见、在游标之后变化的事项（事件 `item`，id 为 `<微秒时间戳>:<itemId>`，支持 `Last-Event-ID` 续传），服务端每 2 秒查询一次、15 秒心跳。浏览器端用 fetch 读流（EventSource 不能带 Authorization 头） | 不引入消息中间件；每连接一条按索引的查询，成本可控 |
| D11 | **运行结果入箱**：`host.run` 成功后，Workspace 以同一凭据跟随 `GET /v1/runs/{id}/events`（SSE）直到 `RUN_FINISHED` / `RUN_ERROR` 或 10 分钟，放入一项 `run`（生产者 `idp-workspace`，`externalRef = run:<runId>`，引用 host / run / id，只指派给调用者）：调用时即放入"运行中"，结束时更新为最后一段助手文本或错误；事项保持 open 供成员阅读后归档（它是通知，不是待决定的事）。跟随在进程内；重启时未完成的跟随丢失（不伪造结果） | 成员发起的 Agent 运行有结果可看，而不必回到 HOST；Workspace 只转述，不保存运行状态 |
| D13 | **壳里的管理界面**：持 `ws-admin` 的成员在导航里看到"管理"，含三页：工作空间与成员（新建工作空间、加 / 改 / 移除成员及其工作空间角色）、应用（粘贴清单发布、撤销、按版本安装或升级并填配置、卸载）、审计。界面只调用 `/api/v1`，服务端仍逐个判定；`GET /v1/me` 只给导航用。新增管理员可见的 `GET /api/v1/workspaces/{ws}/installations`（含已撤销版本的安装，成员的应用列表看不到它们）。升级安装时带当前 revision 作为 If-Match | 没有这一页，建工作空间、发布与安装应用只能手写 API 调用，壳不能独立使用 |
| D12 | **与真实 idp-host 联调**：`deploy/e2e.sh` 的 `HOST=1` 起 idp-host 镜像（及其确定性模型替身），Workspace 以 static 凭据（HOST 的开发令牌）调用；测试发布带 `host.run` 动作的应用，调用后在收件箱等到运行结果 | 证明 D4 / D11 在真实进程之间成立 |

## 7. M2 实施结果（2026-11-04，WSL 实测）

- D9：`apps/web`（React 19、Vite 7、TypeScript 严格模式）与 `packages/bridge` 组成仓库根的 npm workspaces；`deploy/build.sh` 构建壳并把 `dist` 装入镜像（`IDP_WORKSPACE_WEB_DIR`）。服务对非 `/api/`、`/v1/` 的 GET 提供壳：入口页 `Cache-Control: no-cache`、CSP（`default-src 'self'`、`frame-src https:` 与回环、`frame-ancestors 'none'`）、`X-Frame-Options: DENY`；`/assets/*` 不可变缓存；路径穿越回落到入口页；`/shell-config.json` 告诉壳是否开发模式。API 仍全部要求令牌。壳：工作空间切换、收件箱（优先级、实体、来源、动作按钮与 confirm、已读 / 归档）、应用导航、沙箱应用框架与桥、通知。
- D5 修订：浏览器实测发现不带 `allow-same-origin` 的沙箱 frame 的源是不透明的 `"null"`，桥的来源核对会丢弃全部消息；改为 `allow-scripts allow-forms allow-same-origin`，并拒绝装入与壳同源的应用（那时 same-origin 才危险）。
- D10：`GET /v1/workspaces/{ws}/inbox/stream`：首轮推送游标之后（或 `replay=open` 时全部）可见事项，之后每 2 秒查询变化，按 (itemId, version) 去重并保留 5 秒重叠窗口以免漏掉晚提交的事务；清除服务器写超时以维持长连接；浏览器端以 fetch 读流并按最后的事件 id 续传。
- D11：`host.run` 成功后放入"运行中"事项并在后台跟随 HOST 的事件流；结束后同一事项（`run:<runId>`）更新为"finished / failed / result unavailable"与最后的助手文本或错误，只对调用者可见。
- D12：`HOST=1 bash deploy/e2e.sh`：真实的 idp-host 镜像、worker 与模型替身；Workspace 以 HOST 的开发令牌（static）调用；四眼批准的 agent，经 Workspace 动作发起的运行在 HOST 上 COMPLETED，结果"Echo: Why is order 1001 late?"回到成员收件箱。
- 浏览器冒烟（`BROWSER=1`，Playwright + Chromium，CI 中运行）：开发令牌登录、收件箱、经流实时到达的事项、第三方应用页（手写 `idp.bridge/v1`，不用 SDK）在 frame 中拿到工作空间与上下文、`inbox.list` 被 `CAPABILITY_DENIED`、动作失败码 `TARGET_UNAVAILABLE` 原样回到应用、通知出现在壳里、应用看到的任何应答都不含令牌。它还发现并修复了壳每次渲染新建 API 客户端导致的请求风暴（组件测试钉住）。

**验证**：Go 单元（新增 runs）、集成（新增 inbox-stream、web-shell 两个场景，actions-go-to-the-owner 增加运行结果入箱）、前端（bridge 11 项、web 8 项、两者 `tsc --strict`）、镜像 + compose 端到端、浏览器冒烟、与真实 idp-host 的联调全部通过；`npm audit` 0。

**局限（不标 PASS）**
- 登录只有开发令牌；OIDC 授权码 + PKCE、应用令牌（Workspace 签发、受众为应用后端）、下游令牌交换留到 M3。
- 运行跟随在进程内，重启时进行中的跟随丢失（事项停在"运行中"）；forward 模式下用户令牌在跟随期间过期也会如此。
- 收件箱流是每连接轮询；大量并发连接时需要改为 LISTEN/NOTIFY 或消息总线。
- 与 idp-eer 的联调仍待 EER 侧生产者适配器。

## 8. M2 补充：管理界面（2026-11-04）

- D13 已实现：`apps/web/src/Admin.tsx`。服务端拒绝的原因（如 `CONFLICT`、`INVALID_MANIFEST`）原样显示给管理员；非法 JSON 在发出请求前就被拒绝。
- 验证：组件测试 15 项（管理页：建空间、加成员、发布、安装、升级带 revision、服务端拒绝、审计；导航：只有管理员有"管理"）；集成测试覆盖 `/v1/me` 与已撤销版本仍可见于管理员列表；浏览器冒烟新增整条管理员流程——管理员只用界面建 `eu-ops`、把 bob 加为 approver、发布并安装 `eu-desk`、在审计里看到四类操作、提交非法清单被拒；随后 bob 登录，看到新工作空间和应用，且没有"管理"入口。
- 局限：不能在界面里编辑清单（发布后不可变，改了要发新版本）；撤销理由用浏览器的 prompt 输入；没有用户目录，成员只能按令牌里的 sub 手填。

## 9. M3 决策（2026-11-04）

| # | 决策 | 理由 |
|---|---|---|
| D14 | **OIDC 登录**：壳以授权码 + PKCE（S256）作为公开客户端登录（`IDP_WORKSPACE_OIDC_CLIENT_ID`，发行者即 `IDP_WORKSPACE_ISSUER`），回调路径 `/callback`，校验 state；访问令牌存 sessionStorage，到期前用 refresh token 续期，失败则回到登录。`/shell-config.json` 告诉壳发行者与客户端；入口页 CSP 的 `connect-src` 加上发行者的源（发现文档与令牌端点由浏览器直接访问）。服务端照旧以 JWKS 验证令牌（`tenant_id`、`roles` 声明，受众 `idp-workspace`）；开发令牌只在开发模式可用 | 生产中用户由企业身份提供方登录；Workspace 不保存密码，也不代理登录 |
| D15 | **应用令牌**：清单可申请新能力 `identity.token`；桥新增同名方法。壳调用 `POST /v1/workspaces/{ws}/apps/{app}/token`，Workspace 以自己的 ES256 密钥签发 5 分钟的令牌：`iss` = `IDP_WORKSPACE_PUBLIC_URL`，`aud` = `idp-workspace-app:<app>`，`sub`、`tenant_id`、`workspace`、`app`、`appVersion`、`roles`（工作空间角色）、`jti`。公钥在 `GET /.well-known/jwks.json`。密钥来自 `IDP_WORKSPACE_SIGNING_KEY_FILE`（PEM，P-256）；开发模式缺省时启动时生成。`packages/bridge` 提供后端校验函数。契约 `idp.telos.workspace` 1.1.0：能力与桥方法各加一个枚举值 | 应用后端需要知道"是哪位成员、在哪个工作空间"，又不能拿到用户令牌（D5）；受众按应用分开，一个应用的令牌不能用在另一个应用 |
| D16 | **下游令牌交换**：目标凭据新增 `auth: exchange`（`exchange: {tokenUrl, clientId, clientSecretFile, audience}`）：以 RFC 8693 把调用者令牌换成受众为下游的令牌（Workspace 为请求方客户端），按（令牌摘要、受众）缓存到过期前 30 秒；运行跟随（D11）也用它。交换被拒时调用记录为 `TARGET_UNAVAILABLE` 并带身份提供方的错误码 | forward 要求一个令牌同时对多个受众有效，交换让每个下游只收到发给自己的令牌，且由身份提供方决定是否允许 |
| D17 | **EER 推入收件箱**：在 idp-eer 中，执行进入 T2 审批等待时放入 approval 事项（`externalRef = <execution_id>/approval`，引用 eer / execution，动作为批准 / 拒绝），审批被决定或过期时以对应结果结束该事项；以 `ws-producer` 身份调用，失败不影响执行（只记录）。属 idp-eer 的变更，按其 ADR 流程 | EER 没有列表与事件接口（第 1 节），由拥有者推入是唯一不伪造权威的方式 |

**验证要求**：真实 Keycloak（`OIDC=1 bash deploy/e2e.sh`）——浏览器经 Keycloak 登录页登录壳；以交换得到的令牌调用真实 idp-host（HOST 以 JWKS 验证）；应用后端以 JWKS 校验应用令牌。

## 10. M3 实施结果（2026-11-04，WSL 实测）

- D14：`apps/web/src/oidc.ts`：发现文档、PKCE（S256，32 字节随机 verifier）、state 校验（一次性，伪造或重放都拒绝）、授权码换令牌、refresh 续期、带 `id_token_hint` 的退出。服务端：`/shell-config.json` 给出发行者与客户端；入口页 CSP 的 `connect-src` 加入发行者的源；只有使用开发验证器时壳才显示开发令牌表单。新增 `IDP_WORKSPACE_SUBJECT_CLAIM`：Keycloak 的 `sub` 是不透明 id，以 `preferred_username` 作成员名，管理员才能按用户名添加成员。
- D15：`internal/apptoken`（ES256，kid 为公钥摘要，5 分钟，受众 `idp-workspace-app:<app>`）；`POST /v1/workspaces/{ws}/apps/{app}/token` 只对成员、已安装且已发布、清单申请了 `identity.token` 的应用签发；`GET /.well-known/jwks.json`。`packages/bridge` 的 `verifyAppToken`（Web Crypto，校验算法、kid、签名、iss、aud、exp、iat）。契约 `idp.telos.workspace` 1.1.0。
- D16：`auth: exchange`：RFC 8693，按（令牌摘要、受众）缓存到过期前 30 秒并清理过期项；身份提供方的拒绝码（如 `invalid_token`）出现在失败的调用记录里，下游收不到任何请求。运行跟随用同一路径取令牌。
- 端到端（`OIDC=1 BROWSER=1 bash deploy/e2e.sh`，本机，真实 Keycloak 26.8.1 与 idp-host）：Chromium 在 Keycloak 登录页以 bob 登录（授权请求带 PKCE），壳不再出现开发令牌表单；bob 从收件箱事项发起运行——壳的令牌受众只有 idp-workspace，HOST（以 Keycloak 的 JWKS 验证）接受的是交换得到的令牌，运行完成、结果回到收件箱；应用 Desk 经桥拿到应用令牌，Node 中以 `verifyAppToken` 和 Workspace 的 JWKS 校验通过（Go 签、TypeScript 验），同一令牌对另一个应用被拒；退出后再次登录要求重新输入密码。
- 契约 1.1.0 已登记到 idp-contracts。

**验证**：Go 单元（apptoken、exchange）、集成（app-tokens 场景）、前端（bridge 13 项、web 17 项）、开发模式浏览器冒烟与 OIDC 端到端全部通过。

**局限（不标 PASS）**
- OIDC 端到端需要 idp-host 镜像，未进 CI（CI 跑开发模式的浏览器冒烟）。
- 刷新令牌只在页面打开时续期；离开页面超过刷新令牌寿命就要重新登录。
- 应用令牌签名密钥的轮换：JWKS 只发布当前一把钥匙，换钥匙会让未过期的旧令牌立即失效（最长 5 分钟）。
- D17（EER 推入收件箱）在 idp-eer 中实现，见其 ADR。

## 11. D17 实施结果（2026-11-04）

- idp-eer ADR-038（PR #23）：`eer-projector` 跟随 Event Fabric 的 `execution.transitioned`：进入 T2 审批等待时以 `ws-producer` 身份放入 approval 事项（`externalRef = <execution_id>/approval`，动作为应用 `eer-approvals` 的 `approval.approve` / `approval.reject`），离开等待时按触发器结束（approved / rejected / expired，其余 withdrawn）。不改 EER 的工作流，不加 EER 表；启动时从头重放，依靠本服务的幂等（已结束的事项 409、不存在的事项结束 404）保证结果不变。
- 本仓库提供配套清单 `deploy/apps/eer-approvals.json`（两个动作都限 `approver` 角色、需确认，目标 `eer.decision`）。
- 联机验证：EER 的 `TestLiveWorkspace` 对真实 Workspace 镜像推入两项、结束一项、再以新投影器从头重放，两项状态不变；Chromium 中 approver 成员看到 "Approval needed: execution:…" 与"Approve / Reject"按钮。
- 未验证：从壳里点"Approve"到真实 EER（Temporal 中的 T2 执行）完成一次决定——需要 EER 的 OIDC 发行者与 Workspace 用户的主体映射。
- 2026-11-04 补充：上面"未验证"的一条已由平台验收证明——idp-telos-go `deploy/compose/e2e.sh` 的 PATH B 中，bob 从收件箱批准一个真实的 T2 执行（Temporal 中的 idp-eer），Workspace 以令牌交换调用 EER，EER 记录 bob 的批准、完成执行并结束事项（idp-telos-go ADR-0001 §5）。

## 12. D18：运行等待确认时的入箱（2026-11-06）

| # | 决策 | 理由 |
|---|---|---|
| D18 | **等待确认不是结束（修订 D11）**：idp-host 遇到需要所有者确认的工具调用时，运行停在 `AWAITING_INPUT`，事件流以一条 `outcome.type` 为 `interrupt` 的 `RUN_FINISHED` 结束（`interrupts[]` 带 `type: confirmation`、工具名与 `expiresAt`）。运行跟随不再把它当作结束：同一事项（`run:<runId>`）更新为"<动作标题> — awaiting confirmation"，摘要写明要调用的工具、Workspace 里暂时不能确认、请求何时过期（过期按拒绝处理），`dueAt` 设为最早的过期时间；随后立即以最后的事件 id（`after`）重连事件流，跟到真正的 `RUN_FINISHED` 或 `RUN_ERROR`，再按真实结果更新事项（finished / failed，`dueAt` 清空）。D11 的 10 分钟上限只计运行在跑的时间：遇到确认请求，跟随期限延到 max(原期限, 最晚过期时间 + 10 分钟)。事件流不带 id、无法续读时，事项记为 result unavailable | 原来的跟随把等待确认当成运行结束：事项显示 finished，之后再不更新，HOST 上的运行却在确认或过期之后才真正结束（2026-11-06 本机复现）。HOST 默认的确认超时（900 秒）比 D11 的 10 分钟长；HOST 最迟在过期时让运行继续，之后运行仍有 10 分钟。重连在中断后立即发生，而 HOST 只在建流时校验凭据（流上每 15 秒一次心跳，服务端没有写超时），所以交换或转发得到的令牌在等待期间过期不影响已经建立的流。不改契约：事项结构、动作与 `dueAt` 字段都是已有的；在 Workspace 里确认要等契约与平台目录的变更 |

**实施结果（2026-11-06，WSL 实测）**

- `internal/runs`：`read` 记下 SSE 的 `id`，可从 `after` 续读，并解析 `RUN_FINISHED` 的 `outcome`（对象 `{"type":"interrupt","interrupts":[…]}`，也认裸字符串 `"interrupt"`）；`Follow` 改为循环：遇到中断就放"awaiting confirmation"事项并重连，直到运行真正结束；最后一段助手文本跨重连保留。`Follower.Store` 改为只含 `PutItem` 的接口，`*postgres.Store` 满足它，调用方不变。
- 本机复现环境（不在仓库内）：Workspace（static 凭据）、idp-host、idp-mcp 的注册服务与网关、测试替身上游，agent 对 `minimal__echo` 要求确认。修复前后对比：
  - 等到确认过期：修复前，暂停时事项就成了"finished｜The run finished without a text answer."，过期后不变；修复后，暂停时为"awaiting confirmation"（`dueAt` 为过期时间），过期后更新为"finished｜Result: error: the confirmation request expired"。
  - 过了运行期限再确认（发布件 `runTimeoutSeconds` 30、`confirmationTimeoutSeconds` 120，所有者在第 35 秒确认）：修复前事项始终是 finished；修复后暂停时为"awaiting confirmation"，确认后更新为"failed｜RUN_TIMEOUT the run exceeded its time limit"。

**验证**：Go 单元（runs 新增 5 项：中断解析；等待后正常结束；确认后以超时失败；到"过期时间 + 上限"而不是到上限才放弃；事件流没有 id 时不猜测）、集成（新增 run-awaiting-confirmation：真实 PostgreSQL，假 HOST 先给中断、再在续读的流上给结果，核对事项的标题、摘要、`dueAt`、续读的 `after` 与最终结果）、`make test`、`make test-integration`（PostgreSQL 17）全部通过；本机复现环境的两个场景如上。

**局限（不标 PASS）**
- 跟随仍在进程内：等待期间 Workspace 重启，事项会停在"awaiting confirmation"；等待中连接断开时不重连，直接记为 result unavailable（重连所需的令牌可能已经过期）。持久化跟随与令牌续期留待后续。
- Workspace 里还不能确认；static 凭据下运行的所有者是 Workspace 的服务身份，成员本人在 HOST 上也确认不了（403），只能等过期。
- 最终事项只反映运行的结局。idp-host 的期限问题——过了 `runTimeoutSeconds` 再确认，工具照常执行，运行随后以 `RUN_TIMEOUT` 失败——在事项上只显示为 failed，看不出工具已经执行。
- 每个等待中的运行占一个 goroutine 和一条到 HOST 的长连接，最长为确认超时加 10 分钟（HOST 允许确认超时到 86400 秒）。
- `outcome` 与 `interrupts` 的形状来自 idp-host 的实现与 AG-UI，不在 `idp.telos.host` 契约里，消费方契约测试钉不住；中断里的 `expiresAt` 精确到秒，`dueAt` 可能比 HOST 记录的过期时间早不到一秒。
- 没有跑 `make test-web` 与浏览器冒烟：这次没有改界面，复现环境用的是不带网页壳的镜像。

## 13. 从界面发起（2026-11-06）

| # | 决策 | 理由 |
|---|---|---|
| D19 | **壳里的发起入口**：导航里的"发起"列出已安装应用中不依附事项的动作——目标不是 `eer.decision`，契约未限定角色或与成员在本工作空间的角色相交——并按动作的输入 JSON Schema 生成表单（字符串、数字、整数、布尔、枚举各成控件，其余按 JSON 填写；必填与类型在提交前检查），以 `POST /v1/workspaces/{ws}/actions/invoke` 发起；`host.run` 的结果照 D11 回到收件箱 | 只声明动作、没有页面的应用此前只能经 API 发起；服务端照常按 D4 校验成员资格、角色与输入 Schema，壳只收集输入。`eer.decision` 决定的是一个打开的事项，留在事项上 |
| D20 | **应用读到自己发起的运行结果**：桥的 `inbox.list` 对每个事项仍只给标题、种类、优先级、状态与实体；对该应用为本成员经 `host.run` 发起的运行（以成员的调用记录为准）所对应的 run 事项，另给 `externalRef` 与 `summary`。契约的 `result` 不约束字段，不改契约版本 | 聊天式的应用页要显示 agent 的回答；放开所有事项的内容会让第三方页面读到别的系统的审批与结果，只放开它自己替成员请求的那一份 |

**验证要求**：前端单元（表单生成与输入转换、按角色与目标筛选、D20 只放开自己的运行）；开发模式浏览器冒烟里从"发起"提交一个动作并看到服务端的应答；平台 kind 部署中成员从"发起"与应用页各问一次 agent、在界面里看到回答（idp-telos-go `tests/kind/full.sh`）。

## 14. D19、D20 实施结果（2026-11-06，WSL 实测）

- D19：`apps/web/src/Launcher.tsx`（`launchable`、`fieldsOf`、`buildInput` 为纯函数）；导航在有可发起的动作时显示"发起"；成员的工作空间角色取自 `GET /v1/workspaces/{ws}`，取到之前不列任何动作。
- D20：`bridgeHandlers.ts` 的 `inbox.list` 并行读取收件箱与成员的调用记录（`GET /v1/workspaces/{ws}/invocations`），只给本应用以 `host.run` 发起的运行的 run 事项附上 `externalRef` 与 `summary`；读调用记录失败时按"没有自己的运行"处理。
- 前端单元：bridge 13 项、web 22 项（新增发起入口 4 项、D20 1 项）；`BROWSER=1 bash deploy/e2e.sh` 的冒烟从"发起"提交 Demo 的 Ping，壳显示服务端的 `TARGET_UNAVAILABLE`。
- 平台 kind（idp-telos-go `MODEL=live` 的 `tests/kind/full.sh` 与浏览器）：bob 从"发起"问订单助手，回答（本机模型、引用 CMI 中的规定）约 2 秒后进收件箱；在平台的"助手"应用页（`idp.bridge/v1` 手写客户端）中文提问得到引用规定的回答、让代码助手算 2 的 100 次方得到沙箱的输出——应用页只读到它自己发起的运行的内容。

## 15. 我的记忆（2026-11-06）

| # | 决策 | 理由 |
|---|---|---|
| D21 | **成员查看并删除助手记下的对话记忆**：配置新增可选的 `targets.cmi`（形状与校验同 `targets.host`：`endpoint`、`auth: forward\|static\|exchange`、`exchange{tokenUrl, clientId, clientSecretFile, audience}`）。`GET /v1/me/memory` 读 idp-cmi 的 `GET /v1/memory/records?limit=100`（默认范围即调用者本人），返回 `{memories:[{id, content, createdAt, validUntil, source, ref, tags}]}`，新的在前，内容原样；`DELETE /v1/me/memory/{id}` 调 `POST /v1/memory/revoke {recordId, reason}`，返回 `{revoked:1}`；`DELETE /v1/me/memory` 先列出（100 条）再逐条撤销，返回 `{revoked:N}`。记忆属于人而不是工作空间，所以路径不在 `/v1/workspaces/{ws}` 之下，要求 `ws-member`。始终用**成员本人的令牌**（`exchange` 时换成 CMI 受众的令牌）调用 CMI，绝不用服务凭据，所以"哪些是他的"完全由 CMI 判定：别人的 id 只会得到 CMI 的 404，Workspace 不重试、不泄露。`id` 必须是 UUID，否则 422。CMI 的 404、403 原样以 404 NOT_FOUND、403 FORBIDDEN 透传；其余 4xx 为 502 `TARGET_REJECTED`，5xx、超时、交换失败为 502 `TARGET_UNAVAILABLE`（与 D4 的分发一致）。每次删除写审计 `memory.delete`（资源为记忆 id，"全部"时为空；详情只有 `count`，从不含内容）。未配置 `targets.cmi` 时三个端点答 404 `NOT_CONFIGURED`，壳据此隐藏"记忆"页 | 助手（idp-host）会把对话记进 CMI；成员应当看得见、删得掉。由 CMI 判定归属，Workspace 就不会成为越权删除的入口；用成员自己的令牌，则删除在 CMI 的审计里也是成员本人所为 |

**界面**：导航里的"记忆"（仅当 `GET /v1/me/memory` 不是 404 NOT_CONFIGURED 时显示）列出记忆：内容超过 300 字符时截断并可"显示更多"，显示记录日期、到期日、来自哪次运行；每条有"删除"，另有"删除全部"（走壳已有的确认对话框）；没有记忆时显示"还没有记忆。和助手聊几句，它会记住。"；隐私说明不写死天数。

**依赖**：idp-cmi 允许 `cmi-reader` 撤销自己范围内的记录（idp-cmi 的变更，不在本仓库）；在那之前，删除会得到 CMI 的 403 并原样透传。

**验证**：Go 单元（配置校验：新增 `targets.cmi` 的 6 种非法与 3 种合法写法）；集成（真实 PostgreSQL，假 CMI 与假令牌端点：列出、删一条、删全部的路径、`Authorization` 为交换后的令牌而非成员自己的令牌、撤销请求体；别的成员猜 id 得 404 且只请求一次、对方记录不变、不写审计；CMI 不可达与交换被拒为 502；未配置为 404 NOT_CONFIGURED；审计不含内容）；前端单元（列出与截断、删一条后刷新、删全部先确认、空状态、失败提示、未配置时隐藏）。

**补充（联调发现）**：`DELETE /v1/me/memory/{id}` 先用成员自己的令牌列出"我的记忆"，id 不在其中就直接 404、不向 CMI 发撤销。原因：同时持有 CMI 管理员角色的人（联调里的 alice）经这个端点会撤销到别人的记录，因为 CMI 允许管理员撤销任何记录（CMI D26 只给读者放开自己作用域）。"我的记忆"以成员自己的列表为准。测试：`TestMyMemory/another-member-cannot-delete-by-guessing` 断言没有任何撤销调用；平台验收 PATH F 里 alice 删 bob 的 id 得 404。

## 16. 与 agent 对话（D22，2026-11-06）

起因：成员此前只能"发起一个动作，结果回到收件箱"，看不到 agent 怎么答、用了哪些工具、等了谁确认，也回不到上一次对话。idp-host 已经有线程、运行事件日志（AG-UI）、工具确认与取消，缺的是壳能经 Workspace 触达它们。

| # | 决策 | 理由 |
|---|---|---|
| D22 | **壳里的"对话"页，Workspace 做中继**：成员选一个已安装应用提供的 `host.run` 动作（输入是 `message`）所对应的 agent，发一条消息就是一次 `host.run` 调用（与 §13 同一条路径，带线程 id 时接着上一次对话）；页面实时显示这次运行（文字逐字流出、每个工具调用的参数与结果、用到的企业上下文），替成员处理等待确认的工具调用，可取消，可回到列表里的旧对话。后端新增只读与决定类中继端点：`GET /v1/workspaces/{ws}/conversations`、`.../runs/{run}`（含 `/events` 的 SSE 与 JSON、`/tool-calls`、`/tool-calls/{call}/decide`、`/cancel`）、`.../threads/{thread}/messages` 与 `/runs`。每次都用**成员自己**的令牌（换成 idp-host 的受众，D16），从不用 Workspace 自己的凭据；idp-host 仍是运行、消息、决定的所有者，Workspace 不存副本 | 对话是 idp-host 的事实，不该在 Workspace 再存一份；用成员的令牌，idp-host 的"只有运行的所有者能确认"（HOST D7）自然成立 |
| D22a | **只触达成员从这个工作空间发起的运行和线程**：`invocation` 记录里 `result.id`（运行）和 `result.threadId`（线程）是唯一的来源（迁移 `0002_conversations.sql` 加了这两个字段的索引）；路径里的 id 先按 `^[A-Za-z0-9_.:-]{1,128}$` 校验，再查成员自己的记录，不在其中一律 404（不泄露存在性），也不会为别人的 id 向 idp-host 发请求 | 中继不能变成"读任何运行"的后门；成员在别的工作空间或由别的途径发起的运行不是这个工作空间能展示的 |
| D22b | **事件流**：SSE 逐行转发并立即刷新，支持 `after` 与 `Last-Event-ID` 续传，客户端离开即取消上游请求；浏览器不接受事件流时返回 JSON。idp-host 返回的 401 说明的是 Workspace 的凭据被拒，不是成员的会话，不会让壳把成员登出；idp-host 的拒绝保留它的错误码与消息（描述运行），不暴露它的地址 | 流式体验与可恢复；错误归属清楚 |
| D22c | **渲染**：回答按受限的 Markdown 渲染（代码块、引用、列表、粗体、行内代码），全部从文本节点构造，不执行 HTML；壳增加浅色 / 深色（成员的选择，存在本浏览器，"跟随系统"为默认）；错误按码翻成人话（`errors.ts`） | 回答来自模型，不可信：不渲染 HTML；可用性 |

**与 §13、§15 的关系**：动作表单（§13）和应用桥（`actions.invoke` 的 `threadId`）仍是"发起一个动作"，对话页是为聊天做的界面；两者共用同一个 `host.run` 与线程。"我的记忆"（§15）读 idp-cmi，对话页读 idp-host，互不替代。

**验证**：Go 单元（`internal/api/runs_test.go`：中继 JSON 带上答案与成员的凭据、idp-host 的拒绝只带码与消息不带地址、没有凭据时的处理、事件流边到边转发、事件流的拒绝、id 的字符集校验）；集成（真实 PostgreSQL，假 idp-host，`conversations-relay`：跟随运行、决定工具调用、取消、读线程消息，对话列表只含成员自己的 `host.run` 调用、别的成员看到空列表）；web 单元（对话页、事件归并、Markdown、错误翻译）；真实 Chromium（`BROWSER=1 HOST=1 bash deploy/e2e.sh`）：登录后打开"对话"，发一条消息，看到回答流出、运行结束、对话出现在列表里。联调发现：壳此前用一次探测请求判断有没有"记忆"页，没配置时那次 404 被浏览器当成控制台错误，冒烟测试失败；改为 `/v1/me` 带 `memory` 标志，壳据此决定。

**限制**：对话页只列 `host.run` 且输入是单个 `message` 的动作；没有"重新生成"之外的编辑历史；列表只有这个工作空间里成员自己的对话。

**补充（2026-11-12，工具确认的端到端验证）**：此前"替成员处理等待确认"只有单元与集成（假 idp-host）两层证据。现已在真实平台上验证：注册一个 `careful-coder`（与沙箱助手同样的两个工具，但 `confirmation.tools` 把 `sandbox__sandbox_run` 设为 `confirm`），bob 在"对话"页让它跑代码——idp-host 停下运行，页面显示等待中的调用与它将要执行的参数（代码）、过期时间与倒计时，输入框在运行等待期间不可用；bob 点"允许"后决定经 Workspace 到达 idp-host，运行从中断处继续、工具卡片变为"完成"、回答里是沙箱的输出（`answer 42`，3 步 / 2 次工具调用）；同一对话里第二次提问点"拒绝"，该调用显示"已拒绝"且代码没有执行；刷新页面后历史从 idp-host 读回。平台验收固化为 `tests/kind/full.sh` 的 PATH I（同一条路径的 API 断言，外加：运行处于 `AWAITING_INPUT`、事件流里有 `idp.host/confirmation.required`、管理员 alice 对别人的运行与决定都得到 404、未知的 decision 得到 422）。

同时修掉一处隐患：浏览器冒烟里判断"运行已结束"用的是 `page.waitForFunction`，而壳的 CSP 是 `script-src 'self'`，Playwright 为这个谓词注入的求值会被拒绝（`unsafe-eval`）——只有在装了可对话的 agent 时才会走到那一行，所以此前没有暴露。改为等待终态选择器（`.turn.assistant.status-done|failed|canceled`）。

## 补充：导出我的记忆（2026-11-09）

`GET /v1/me/memory/export`（`ws-member`，要求 `targets.cmi`，否则 404 `NOT_CONFIGURED`）用成员本人的令牌调 idp-cmi 的 `GET /v1/memory/export`（CMI ADR D36），原样返回 `idp-cmi.memory-export.v1` 文档；审计 `memory.export`，详情只有 `count`。"记忆"页的"导出"按钮把它存成 `my-memory.json`。

## 17. D23：成员的文件（2026-11-13 决策与实施，平台 ADR-0002 阶段 1）

起因：office 档位（平台 ADR-0002 阶段 0）让智能体能在沙箱里做 Word / Excel / PowerPoint，但成员交不出自己的文件，也拿不到做好的文件。平台 ADR-0002 决定文件归 Workspace（F1），字节凭票据在沙箱与 Workspace 之间带外传递（F3、F4），交付物在对话页下载（F7）。

| # | 决策 | 理由 |
|---|---|---|
| D23 | **成员文件与运行的票据**。新表 `member_files`（迁移 0003，按租户强制 RLS）：所有者、所在工作空间、名称、媒体类型、大小、SHA-256、来源（`upload` 或 `run:<runId>`）、敏感级别（缺省 internal）、内容（PostgreSQL 的 bytea）、过期时间（缺省 30 天，每小时清除过期的）。接口只给所有者：`POST /v1/workspaces/{ws}/files?name=`（请求体就是文件）、`GET …/files`（`?source=`）、`GET …/files/{id}`、`GET …/files/{id}/content`（`Content-Disposition: attachment`、`nosniff`、CSP `sandbox`、`no-store`，壳里不渲染）、`DELETE …/files/{id}`；管理员也看不到别人的文件。收与交都检查文件：扩展名白名单（.docx .xlsx .pptx .pdf .png .jpg .jpeg .csv .txt .md），带宏的格式与包里的 `vbaProject.bin` 拒绝，内容须与扩展名相符，Office 包限部件数、解压后大小与压缩比；单个文件 25 MB，每个成员在租户内 200 MB / 500 个，名称只留一个路径元素。动作输入 Schema 用 `format: idp-file`（字符串或其数组，值为文件 id）声明收文件的字段；调用时这些 id 必须是调用者本人在这个工作空间的文件（否则 422），至多 10 个。`host.run` 的调用为这次运行（`ws_<invocation>`）签票据：每个附件一张读票据（租户、主体、文件、运行），再加一张写票据（租户、主体、运行、工作空间、至多 10 个文件、每个 25 MB），以 idp-host 契约 1.8.0 的 `idp.host/attachments` 与 `idp.host/delivery` 传过去。票据用应用令牌的 ES256 密钥签（D15），受众 `idp-workspace-files`，有效 60 分钟；`sub` 是成员令牌自己的 `sub`（网关转发给沙箱的调用者就是它，沙箱据此拒绝别人的票据），`owner` 是 Workspace 认的成员（`IDP_WORKSPACE_SUBJECT_CLAIM`，文件的所有者）——两者在 compose 里不同（`preferred_username` 与 Keycloak 的 UUID），实测前写成同一个会让每张票据在沙箱被拒。沙箱在**内部监听**（`IDP_WORKSPACE_INTERNAL_LISTEN`，只该让 idp-sandbox 到达）上凭票据取文件（`GET /internal/v1/files/content`）与交付（`POST /internal/v1/files`，名称在 `Content-Disposition`），交付的文件成为所有者的文件，来源 `run:<runId>`。对话页：动作收文件时输入框旁有附件按钮（上传后随问题发出），`idp.host/file.delivered` 事件与历史运行的交付物显示为可下载的文件；收件箱的运行事项列出交付的文件名 | 文件归成员：成员在壳里看、管、删；沙箱工作区是临时的，不适合当交付物的家。票据把权限缩到"这次运行、这个文件、这一小时"，沙箱不必拿未签名的转发身份（idp-mcp D43）向 Workspace 证明身份。内部接口分开监听，公开端口上没有凭票据就能取字节的入口。不渲染 Office 文件，壳就不解析不可信的文档 |

**配置**：`IDP_WORKSPACE_INTERNAL_LISTEN`（设了且有应用令牌密钥，`host.run` 才带票据；没有则附件被拒绝、运行不能交付，上传照常）、`IDP_WORKSPACE_FILE_MB`（25）、`IDP_WORKSPACE_FILE_QUOTA_MB`（200）、`IDP_WORKSPACE_FILE_QUOTA_FILES`（500）、`IDP_WORKSPACE_FILE_RETENTION_DAYS`（30）、`IDP_WORKSPACE_FILE_TICKET_MIN`（60）、`IDP_WORKSPACE_RUN_FILES`（10）。注意：开了票据，所有 `host.run` 都带交付许可，idp-host 因此需要 `IDP_HOST_CREDENTIAL_KEY`（否则 409）。

**验证**：单元（`internal/files`：票据往返，别的密钥、应用令牌、缺 run、篡改、过期都打不开；类型检查与名称；`internal/schema`：`idp-file` 格式与字段顺序；`internal/dispatch`：带文件的 `host.run` 送出的选项按钉住的 idp-host 1.8.0 RunOptions 校验通过，读写票据各自绑定成员、文件、运行与工作空间，没有签票据的配置时附件被拒；`internal/runs`：交付的文件名进入事项）；集成（`TestWorkspaceEndToEnd/member-files`，真实 PostgreSQL：上传与 7 种拒绝、非成员、只有所有者能列、读、下载，管理员与别的租户不行；carol 不能附 bob 的文件；运行拿到的读票据取回原字节，写票据、成员令牌、无票据都不行，公开端口上没有内部接口；凭写票据交付、宏与超大文件被拒、一次运行至多 2 个；bob 能下载交付物，carol 不能；过期票据失效；配额；删除后票据取不到）；壳（vitest：附件随问题发出、交付物可下载、历史运行的交付物归到对应的回合）。

**局限**：内容存在 PostgreSQL（单文件 25 MB 以内合适；更大或更多时换对象存储，接口不变）；敏感级别只有字段，外部模型的上限（平台 ADR-0002 F8）未实施；票据在有效期内可重复使用（没有一次性消费），由运行与文件范围、短有效期限制；历史对话里看不到当时附了哪些文件（运行的消息只有文字）。

## 18. D24：文件内容放进对象存储（2026-11-13 决策与实施）

起因：D23 把文件内容放在 PostgreSQL（单文件 25 MB 以内够用，RLS 与数据库备份直接覆盖）。平台已经在用 RustFS（kind 上的数据库备份目标），文件多了、大了，数据库就不该再背着它们：备份变大、表膨胀、连接被大对象占住。

| # | 决策 | 理由 |
|---|---|---|
| D24 | **文件内容可放在 S3 兼容的对象存储**（`IDP_WORKSPACE_FILE_STORE=s3`；缺省 `postgres` 不变）。数据库只留元数据，内容在 `files/<租户>/<文件 id>`（迁移 0004：`object_key`，`content` 可空，约束要求两者恰有其一且键名与行一致）。写：在持有所有者锁的事务里先传对象、再插行，事务失败就删掉对象，所以没有指向不存在对象的行。读：事务外取对象，**按数据库里的 SHA-256 与大小核对**，不符就拒绝（500，日志记下），对象存储里被改过的文件不会交给成员或沙箱。删与过期：先删行、再删对象。清扫：每小时随过期清理一起，列出每个租户前缀下超过 1 小时、没有行指向的对象并删除（上传进行中的对象有行之前不到 1 小时）。客户端是自己写的（路径风格请求、AWS Signature V4，约 300 行，以 AWS 文档的签名示例为单元测试），不引入 SDK。每个对象都要求服务端加密（SSE-S3，`IDP_WORKSPACE_S3_SSE`，缺省开）。密钥只从文件读（`_S3_ACCESS_KEY_FILE`、`_S3_SECRET_KEY_FILE`），启动时列一次桶，不通就拒绝启动。之前写在数据库里的文件照常可读，不迁移 | 对象存储没有行级安全，隔离靠"只有 Workspace 拿得到这组密钥"：所以**文件用自己的存储实例和桶，不与数据库备份共用**（共用 RustFS 的根密钥，Workspace 就能读写删除备份）。SHA-256 本来就在行里，核对它把对象存储从"可信"降为"只负责保管" |

**配置**：`IDP_WORKSPACE_FILE_STORE`（postgres | s3）、`IDP_WORKSPACE_S3_ENDPOINT`（http(s)://host:port）、`_S3_BUCKET`、`_S3_REGION`（us-east-1）、`_S3_ACCESS_KEY_FILE`、`_S3_SECRET_KEY_FILE`、`_S3_CA_FILE`（可选）、`_S3_SSE`（true）。桶由运维创建，Workspace 不建桶。

**验证**：单元（`adapters/blob`：AWS 文档的两个 SigV4 示例逐字节一致；对假服务的请求形状、路径编码、SSE 头、列表分页、404 / 超限 / 403 的处理且不把存储的错误详情外泄；配置拒绝）；集成（`make test-integration` 起一个按摘要固定的 RustFS 与 PostgreSQL；`TestFilesInTheObjectStore`：D23 的全部成员文件流程跑在对象存储上；行里没有内容、对象键与行一致；存储报告对象已加密；对象被改后下载得到 500 且不含改后的内容；删除与过期后对象消失；年轻的无主对象不清、过了时限的清掉、有行的不动）；平台 PATH J 走对象存储（compose 的 `files-s3`）。

**局限**：桶的备份与版本由运维负责（数据库备份不再包含这些文件）；对象存储只有一组密钥，没有按租户分开的凭据；kind 上的 Workspace 仍用 `postgres`（那里的 RustFS 是数据库备份目标，不共用；kind 上也没有文件进出沙箱）。

## 19. D25：交给 idp-host 的令牌绑定成员的会话（2026-11-13，配合 idp-host D38）

| # | 决策 | 理由 |
|---|---|---|
| D25 | `targets.*.exchange` 增加 `sessionBound`：交换时请求 `requested_token_type=refresh_token`，**只用返回的访问令牌，refresh token 丢弃**（不缓存、不转发、不记录）。平台的 compose 对 `targets.host` 打开它 | idp-host 要在运行中续期它的网关凭据（D38），而 Keycloak 只允许对"在用户会话里有客户端会话"的令牌再交换出 refresh token；Workspace 这一次请求就是为了留下这个客户端会话。Workspace 自己不需要续期，所以不保留 refresh token，少一个要保护的凭据 |

**验证**：`internal/dispatch` 的 `TestSessionBoundExchange`（请求的类型、下游只收到访问令牌、refresh token 不出现在任何请求里、关掉后恢复普通交换）；平台 PATH I、PATH K。

## 20. D26：跟随运行直到它自己的时限（2026-11-17）

起因：office-assistant 的运行时限是 30 分钟，跟随上限是固定的 10 分钟（D11）。2026-11-17 一次真实运行用了 12.5 分钟、以 `STEP_LIMIT` 结束，bob 的收件箱里却是"result unavailable … context deadline exceeded"：跟随者到 10 分钟主动断开，没有读到结局。

| # | 决策 | 理由 |
|---|---|---|
| D26 | 跟随者开始时读 `GET /v1/runs/{id}` 的 `deadlineAt`，跟到那个时刻再加 3 分钟；读不到（请求失败、没有这个字段）时仍用 `MaxWait`（默认 10 分钟）。确认等待的延长不变（D18：请求的过期时刻加 `MaxWait`）。3 分钟是给已经开始的那一步的：期限在模型调用或工具调用期间到点，运行要等这一步结束才失败，终态事件晚于 `deadlineAt` | 一次运行该被跟多久，由它自己的时限决定，而不是由一个对所有运行都一样的常数决定。断开后收件箱写的是跟随者自己的错误，用户看不到运行真正的结局 |

**验证**：`internal/runs` 的 `TestFollowLastsAsLongAsTheRun`（运行的期限晚于 `MaxWait`，跟随者在 `MaxWait` 之后仍在，结束后收件箱是 `STEP_LIMIT` 而不是 result unavailable；读不到期限的用例仍在 `MaxWait` 放弃）。2026-11-17 一次真实运行跟了 34 分钟，收件箱是"failed / STEP_LIMIT the run reached its limit of 120 steps"。

**局限**：令牌在打开事件流时校验一次，所以一次长运行靠这一条连接就够；流中断后重连（确认等待之后，D18）用的仍是开始时的令牌，而 D25 不保留 refresh token，令牌过期后的重连读不到后续事件，和以前一样。

## 21. D27：读票据只用一次，运行结束票据作废（2026-11-17，平台 ADR-0002 阶段 2 的一项）

D23 的票据在 60 分钟内可以反复出示：读票据每次都交出文件，写票据在运行的文件数上限内每次都收下一个文件。票据经过 idp-host 的数据库（加密）、网关与沙箱的内存和它们之间的连接；任何一处漏出去，拿到的人在这一小时里可以悄悄读那个文件，谁也不会发现。

| # | 决策 | 理由 |
|---|---|---|
| D27 | **读票据出示一次就用掉；一次运行结束，它的票据全部作废。** 迁移 0005 加两张按租户强制行级安全的表：`file_ticket_uses`（票据的 `jti`、运行、已用次数、票据自己的过期时刻）与 `file_runs_ended`（运行、何时结束）。内部文件端点在验过签名之后、交出或收下任何字节之前调用 `SpendTicket`：运行已结束则 401；读票据的使用次数先记账（`INSERT … ON CONFLICT … WHERE uses < 上限`，一条语句），超过上限则 409 `TICKET_SPENT`，消息写给 agent 看："这个附件已经拷进过工作区，一个请求里一个附件只能导入一次，接着用已经导入的那份"。上限是 `IDP_WORKSPACE_FILE_TICKET_READS`，默认 1。写票据不计次数：它的额度本来就是这次运行能交付的文件数（`IDP_WORKSPACE_RUN_FILES`，`PutFile` 在所有者锁下数）。运行跟随（D11、D26）看到运行真正结束（`RUN_FINISHED` 且不是等待确认，或 `RUN_ERROR`）时调用 `EndRun`；事件流跟丢了、放弃了则不调用，因为运行可能还在做事。两张表的行只留到票据本来也会过期的时刻，随每小时的过期清理删除。没有 `jti` 的票据不再能打开 | 用掉即作废，漏出去的读票据要么已经没用，要么被人抢先用了而让 agent 的导入失败，这个失败看得见（沙箱的工具结果、Workspace 的日志 `file ticket refused`）。先记账后交字节，两个同时到达的出示只有一个得到文件。写票据做不成"一次"：一次运行可以交付几个文件，兜底交付（idp-host D41）还会再用它；能做的是把它的寿命从一小时缩到运行本身那么长。运行结束由跟随者报告而不是由 idp-host 回调：跟随者本来就在读这条事件流，不需要新的接口和新的凭据 |

**代价与局限**：
- agent 把导入的原件改坏了，不能再导入一次。平台的 officecli skill 加了一句：附件只能导入一次，原件留着别动，在副本上改。沙箱在出示读票据之前先确认工作区是调用者的、路径合法（idp-sandbox `CanPut`），所以工作区 id 或路径写错不会白白用掉票据；但取回字节之后才出的错（文件检查不通过、写进工作区失败）仍然会。真遇到这种部署，把 `IDP_WORKSPACE_FILE_TICKET_READS` 调大。
- 跟随在进程内（D11）：Workspace 重启后未完成的跟随丢失，那些运行的票据就只按 60 分钟过期，和以前一样。
- 运行在等待成员确认时没有结束，票据仍然有效，直到确认请求过期后运行真正结束。
- 幂等重放同一个 `host.run` 时 Workspace 会重新签一套票据（新的 `jti`），idp-host 用的是第一次存下的那套，新签的没人用。

**验证**：`internal/files` 的 `TestTicketsSayWhatTheyAllow`（票据有自己的 id 与过期时刻，两张票据 id 不同，没有 id 的打不开）；`internal/runs` 的 `TestFollowEndsTheRunsFileTicketsWhenTheRunEnds`（结束与失败都作废；跟丢了不作废；等待确认时不作废，确认后真正结束才作废）；集成测试（真实 PostgreSQL，`make test-integration`）的 member-files：读票据第二次出示得到 409 与那句话，8 个同时出示的只有 1 个得到文件，另一个租户的同名记录互不相干，假 HOST 的运行结束后未用过的读票据与写票据都是 401，别的运行的票据照常可用，过期的记录被清理。idp-sandbox 的 `TestAnImportSpendsTheTicketOnlyWhenTheFileCanLand`（三种落不了地的导入一次都没有出示票据；第二次导入把 Workspace 的话转给 agent）。平台 PATH J 没有重跑（那会重建正在运行的平台）。

## 22. D28：文件的敏感级别跟着文件走（2026-11-17，配合 idp-host D43）

`member_files.sensitivity` 从 D23 起就有，一直是缺省的 `internal`，没有地方能设，也没有人读。

| # | 决策 | 理由 |
|---|---|---|
| D28 | ① 上传接口收 `?sensitivity=`（`public`、`internal`、`confidential`、`restricted`，缺省 `internal`，别的值 422）。② `host.run` 的附件里带上级别，但只在它不是 `internal` 时带。③ 写票据带上这次运行所读文件里最高的级别（不低于 `internal`），运行交付的文件按这个级别入库 | idp-host 据此决定一个可能用外部模型的 agent 拿不拿得到这个文件（idp-host D43）。`internal` 不发送：它就是"没写"的含义，只认识契约 1.8.0 的 HOST 仍然收得下这样的文件；级别更高的文件遇到旧 HOST 是整个请求被拒绝（多余字段），而不是级别被悄悄忽略。交付物继承级别：否则读了机密文件写出来的摘要是 `internal`，下一次就能交给外部模型，上限等于没有 |

**没有做的**：壳里上传文件的界面没有定级的控件，已有文件不能改级别（要重新上传）；管理员不能按工作空间设缺省级别。钉住的 idp-host 契约副本换成 1.9.0（`tests/contracts/idp.telos.host/1.9.0/`）。

**验证**：`internal/files` 的 `TestTicketsSayWhatTheyAllow`（写票据的级别、未知级别签不出来、两个级别取高且不低于 internal）；`internal/dispatch` 的 `TestHostRunSaysHowTheFilesAreClassified`（internal 不发送，其余照发；写票据是最高的那个；发出去的选项满足钉住的 1.9.0 契约）；集成测试 member-files（带级别上传、未知级别 422、附件里的级别、读了 confidential 文件的运行交付的文件是 confidential）。

## 23. D29：运行结束的事项列出它用过的网页来源（2026-11-19，平台 ADR-0003 附录 D 任务 WS1，配合 idp-host D49）

起因：联网检索让 agent 的回答带 `[n]` 引用，成员却只能相信模型写的编号。idp-host 的 D49 在运行里记下工具真正返回过的来源，运行完成时把回答里的编号和来源对一遍，并在事件流的 `idp.host/run.sources` 里一次给出。Workspace 通过 D11 已有的"跟随运行"取得它，不另发请求，放进成员的收件箱事项，让成员能看到答案从哪里来、哪个编号没有对应的来源。

| # | 决策 | 理由 |
|---|---|---|
| D29-1 | **采集：跟随者读 `CUSTOM idp.host/run.sources`，不加请求。** 事件的值里只取 `sources` 与 `unverifiedCitations`（`droppedInvalid` 等计数不存）。跟随到 `RUN_FINISHED`（不是等待确认）且运行没有失败时，随最后一次 `PutItem` 写入事项；更早的"running"、"awaiting confirmation"事项不带来源。同一事件出现多次以最后一次为准；没有这个事件的运行没有来源 | 事件只在运行完成时发一次（idp-host D49-9），在最终事项上一并写入，事项只多一次更新，不多一次请求。D18 的重连不需要带着来源走：来源只在最后一段事件流里 |
| D29-2 | **Workspace 一侧把每个字段再校验一遍，不信任 HOST 的流。** `workspace.ParseRunSources`：①`n` 是 JSON 整数，1 到 1 000 000（`1.5`、`"3"`、`1e2`、前导零都不是）；②`url` 是字符串，不超过 2048 字节，**按解析后的结果**判断 scheme 为 http 或 https（不分大小写），有主机，没有用户信息，没有不透明部分，不含空白、控制字符、格式字符（含双向文字符）；③`title` 必须是字符串（可空）：无效 UTF-8 换成 U+FFFD，换行、制表符与各种空白折成一个空格，控制字符与格式字符（双向覆盖与隔离、零宽字符、字节序标记）去掉，空白折叠，超过 300 个字符截成 299 个加省略号；**标记不去掉**，`<script>` 以文字保存，在展示处转义；④`fetchedAt` 有则须是 RFC 3339，存为 UTC；⑤`sha256` 有则须是 64 位小写十六进制；`null` 与空串等于没有；⑥`cited` 缺省为假，不是布尔值则该条无效；⑦契约以外的字段（摘录）一律不存；⑧同一个 `n` 只留第一条，其余丢弃；按 `n` 升序，**至多 100 条**，多出的丢弃；一次事件最多读前 1000 条；⑨`unverifiedCitations` 只留 1 到 999 的整数，去重、升序、至多 100 个。**无效的条目整条丢弃，不修补**（标题的清理除外）。没有一条有效来源又没有无法核对的编号时，事项没有这两个字段 | 标题与地址来自网页，HOST 的清理是它自己的承诺，Workspace 要展示、要生成链接，所以自己再查一遍；把范围取成与 idp.telos.host 1.11.0 的 `RunSource` 一致（`TestWhatIdpHostsContractAllowsOfASourceTheFollowerKeeps` 逐项比对，并要求契约自己的示例原样通过），不比 HOST 更严也不更松，HOST 允许发布的来源不会被默默丢掉。scheme 看解析结果而不是文字前缀，`JaVaScRiPt:`、前导空白、`data:` 都因此挡住 |
| D29-3 | **事项上的形状：`InboxItem` 增可选的 `sources` 与 `unverifiedCitations`。** 存在 `inbox_items` 的两列（迁移 0006）：`sources jsonb`（数组，1 到 100 项）与 `unverified_citations integer[]`（1 到 100 个），都可为 NULL，没有来源的事项两列都是 NULL，接口里字段不出现（不是空列表）。表、行级安全与授权不变：能看到事项的人（被指派的成员、角色、或无人指派时的所有成员）看到来源，别人看不到；`GetItem`、`ListInbox`、`ChangedItems`、事项的 SSE 都经同一组列。**不进摘要文字**：摘要仍是模型的回答。只有 `run` 类事项能带来源 | 跟"已交付的文件"的先例不同：文件只是名字列在摘要前面（D23），来源要有结构（编号、标题、地址、时间、是否引用），摘要文字做不到安全地展示。结构化字段随事项走，权限就不用另外实现。字段只在 `ItemInput` 里以 `json:"-"` 出现：生产者接口（`POST /inbox/items`）收到 `sources` 是 400，来源只能由校验过 HOST 事件的跟随者写入；存储层 `PutItem` 再按 `ValidRunSources` 检查一遍（数量、编号唯一升序、scheme、标题已清理），是最后一道 |
| D29-4 | **契约 `idp.telos.workspace` 升到 1.3.0（只做新增）：** `InboxItem` 增可选的 `sources`（`RunSource` 数组，至多 100）与 `unverifiedCitations`（1 到 999 的整数，唯一，至多 100）；新定义 `RunSource`（`n`、`title`、`url`，可选 `fetchedAt`、`sha256`，`cited`；封闭）。示例增一个读过网页的运行事项。测试：`TestContractExamplesAreGeneratedAndValid`（示例由 Go 值生成）、`TestContractFieldsMatchGo`（`RunSource` 的字段与 Go 结构一致）、`TestContractAndGoAgreeOnWhatARunSourceIs`（schema 拒绝的、`ValidRunSources` 也拒绝，12 种） | 旧的消费者忽略新字段，旧的生产者不写它们 |
| D29-5 | **展示（内置页，`apps/web/src/Sources.tsx`）。** 运行事项在摘要下列出来源：编号 `[n]`、标题、**主机名**、"回答引用了它 / 回答没有引用"、"读取于 …"（没有 `fetchedAt` 写"只搜索到，没有读取"），下面一行是**完整地址**；引用过的来源排在前面，各组按编号。五个以内默认展开，更多时折叠，标题行写条数。**标题是文字节点**：React 把它当文字转义，不经 Markdown，不用 `dangerouslySetInnerHTML`；页面再清一遍控制字符与双向文字符。地址只有在**页面一侧也**通过检查时才是链接：文字必须以 `http(s)://` 加一个主机字符开头（浏览器的解析会把 `https:///x` 补成 `https://x/`，所以不能只信 `new URL`），解析后协议是 http 或 https，没有用户名密码，不含空白与格式字符；链接 `target="_blank" rel="noopener noreferrer"`，**链接文字就是完整地址**（浏览器将要打开的 `href`），并带 `title`。不是链接的地址只显示为文字。主机名取解析后的 `hostname`，国际化域名显示为 punycode，字形相近的域名就不会冒充别的站。有无法核对的编号时，来源列表上方一条 `role="note"` 的提示："回答引用了 [7]、[9]，它们不在这次运行的来源里：请先核对，再据此行事。"（单个编号用"它不在"）；没有无法核对的编号就没有这条提示。页面的 CSP 是 `script-src 'self'`、`style-src 'self' 'unsafe-inline'`，这次没有增加脚本，样式写进已有的样式表 | 地址与标题是网页的话，不是我们的话：标题可能含 HTML、Markdown 链接语法、双向覆盖（把 `invoice‮fdp.exe` 显示成别的扩展名）、`javascript:` 地址。完整地址放在文字里而不是只放在悬停提示里，是因为成员点之前需要看见它去哪里（触屏没有悬停） |
| D29-6 | **失败、取消、到时限、跟随丢失的运行不显示来源。** HOST 只为完成的运行发这个事件（idp-host D49-8），Workspace 不为此另发请求去读 `GET /v1/runs/{id}`。所以事项是 "failed"（`RUN_ERROR`，含 `CANCELED`、`RUN_TIMEOUT`）或 "result unavailable"（跟随到 D11 的上限、D26 的期限后断开，或流提前结束）时没有来源，即使运行读过网页。跟随断开时事件即使已经读到也不用：运行的结局不明，"回答引用了它"这类说法没有依据 | 失败的运行没有最终回答，"是否被引用"没有意义，列表里全是"没有引用"会误导；少一次请求、少一条要保护的路径（那次请求要用成员的令牌，而令牌在长运行里可能已过期，D20、D26）。代价写在"没有做的" |

**升级顺序：先 HOST（idp-host D49，契约 1.11.0），后 Workspace。** 先升 Workspace 没有害处，但在 HOST 发这个事件之前它什么也不做；先升 HOST 时旧 Workspace 忽略不认识的自定义事件（`read` 对其他 CUSTOM 事件本来就不处理），事项没有来源。迁移 0006 只加两列，旧版本的服务照常读写。

**拿掉一个控制、看测试失败**（每一项在实现里拿掉一次，跑下列测试，失败后恢复）

| 控制 | 拿掉的东西 | 失败的测试 |
|---|---|---|
| Workspace 一侧的再校验（D29-2） | 跟随者不经 `ParseRunSources`，把事件的值直接解码进 `RunSource` | `TestTheFollowerReadsTheSourcesOfTheHostsEventAndWithoutOneThereAreNone`、`TestAHostileEventIsKeptCleanedOrNotAtAll`、`TestAnEventOfAHundredAndFiftySourcesKeepsAHundred` |
| scheme 检查（Go） | `ValidSourceURL` 不看 scheme | `TestEachKindOfInvalidSourceIsDroppedAndTheOthersStay`、`TestContractAndGoAgreeOnWhatARunSourceIs` |
| 用户信息（Go） | 不拒绝 `user:pw@` | `TestEachKindOfInvalidSourceIsDroppedAndTheOthersStay`、`TestAHostileEventIsKeptCleanedOrNotAtAll` |
| 标题清理（Go） | 标题原样保存 | `TestAHostileEventIsKeptCleanedOrNotAtAll`、`TestAValidSourceIsKeptAndTheListIsOrderedByNumber` |
| 100 条上限 | 不截断 | `TestNoMoreThanAHundredSourcesAreKeptWhateverTheHostSends`、`TestAnEventOfAHundredAndFiftySourcesKeepsAHundred` |
| 无法核对的编号的范围 | 去掉 999 的上限 | `TestUnverifiedNumbersAreIntegersFromOneToNineHundredNinetyNine` |
| 存储层的检查 | `PutItem` 不调 `ValidRunSources` | 集成 `run-sources`（直接写入 `javascript:` 来源被存下） |
| 只有 run 事项有来源 | 去掉 kind 判断 | 集成 `run-sources` |
| 生产者接口不收来源 | `ItemInput.Sources` 的 tag 去掉 `json:"-"` | 集成 `run-sources`（`POST /inbox/items` 带 `sources` 应为 400） |
| 失败的运行不带来源（D29-6） | 失败也写入 | `TestAFailedOrCutFollowShowsNoSources` |
| 来源按工作空间隔离 | `GetItem` 不按工作空间过滤 | 集成 `run-sources`（被指派的 bob 在 eu-ops 里按 id 取这个事项应为 404）；`inbox-is-a-view` 也失败 |
| 事项的可见规则 | 对 run 事项不套 `visibleItemSQL` | 集成 `run-sources`（同工作空间的 carol 按 id 取应为 404）；整体去掉时 `inbox-is-a-view` 先失败 |
| 转义（页面） | 标题改用 `dangerouslySetInnerHTML` | `Sources.test.tsx` 的 hostile title 用例（页面字节里出现 `<script`、`<img`） |
| scheme 检查（页面） | 去掉协议判断与"以 `http(s)://` 开头"两层 | `Sources.test.tsx` 的两个用例（有 `javascript:` 的链接）；只去掉其中一层时不失败：两层互为冗余，这是有意的 |
| 主机字符检查（页面） | 去掉 `EXPLICIT_HOST` | `Sources.test.tsx` 的地址用例（`https:///x`） |
| 用户信息（页面） | 不拒绝用户名密码 | `Sources.test.tsx` 两个用例 |
| 标题清理（页面） | 不去控制字符与双向字符 | `Sources.test.tsx` 两个用例 |
| `rel="noopener noreferrer"` | 去掉 | `Sources.test.tsx` 的列表用例 |
| 提示 | 永远不显示；永远显示 | `Sources.test.tsx` 的提示用例（两个方向） |

**验证**：`internal/workspace`（`TestAValidSourceIsKeptAndTheListIsOrderedByNumber`、`TestEachKindOfInvalidSourceIsDroppedAndTheOthersStay`：26 种无效条目、`TestNoMoreThanAHundredSourcesAreKeptWhateverTheHostSends`、`TestUnverifiedNumbersAreIntegersFromOneToNineHundredNinetyNine`、`TestATitleIsCleanedOfControlAndBidiCharactersAndCut`、`TestTheStoreCheckRefusesWhatTheFollowerWouldNotKeep` 与三个契约测试）；`internal/runs`（跟随者对 HOST 替身流的采集：有事件、没有事件、一条有效的都没有、含 `<script>`、`<img onerror>`、Markdown 链接、双向覆盖与 `javascript:` / `data:` / 用户信息地址的事件、150 条的事件、最终事项带来源且摘要不动、没有事件的运行没有来源、失败 / 取消 / 流提前结束没有来源，以及与 idp.telos.host 1.11.0 的消费方比对）；集成（`TestWorkspaceEndToEnd/run-sources`，真实 PostgreSQL：150 条含恶意标题的事件存成 100 条且已清理、`javascript:` 的那条不在；bob 看得到，同工作空间的 carol、另一个工作空间的成员 dan、被指派者在错的工作空间、另一个租户的 erin 都是 404；生产者接口带 `sources` 为 400；存储层拒绝 `javascript:` 来源与非 run 事项上的来源；没有事件的运行与失败的运行没有这两个字段）；壳（vitest `Sources.test.tsx`：列表各项、完整地址与 `rel`、两个方向的提示、没有来源时什么也不显示、恶意标题与地址的页面字节断言、`safeLink` 的 13 种地址）。契约测试钉住的 HOST 契约从 1.9.0 换成 1.11.0（`tests/contracts/idp.telos.host/1.11.0/`，与 idp-contracts 里的副本逐字节相同）。

**没有做的**：
- **失败的运行不显示它读过的来源。** 需要时是一次 `GET /v1/runs/{id}`（来源在运行的表示里，idp-host 对失败、取消的运行保留已收集的来源），代价见 D29-6。
- **事件之前没有增量**：运行进行中事项上看不到来源（idp-host D49-9 也没有增量事件）。
- **对话页（D22）不显示来源**：它按运行的事件流自己渲染；这次只做收件箱。
- **不检查地址是否可达，不取标题所指页面，不显示页面正文或摘录**（HOST 不存，这里也不收）。
- **`sha256` 不显示**：它只是第一段读到的正文的摘要（idp-host D49-4），展示出来会让人以为能校验整页。字段随事项返回，供以后的界面或审计用。
- **来源不进记忆、不进审计**；管理员也看不到别人的运行事项（可见规则没变）。
- **没有跑真实浏览器冒烟**（`tests/browser/shell.smoke.mjs`）：这次的检查在 jsdom 里做，页面字节是 React 的渲染结果。
- **idp-web 重定向造成的"冲突"计数、`cited` 的标记语法的盲区**是 HOST 的事（idp-host D49-5、D49-8），Workspace 只展示 HOST 给的 `cited`。
