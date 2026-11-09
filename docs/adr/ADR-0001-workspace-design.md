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
