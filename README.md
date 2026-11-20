# ytpl

`ytpl` 使用官方 YouTube Data API v3 管理 YouTube 和 YouTube Music 中可通过该 API 访问的歌单。它支持本地缓存、最少操作次数的重排，以及从网易云音乐迁移计划继续导入。凭据采用 Google Cloud Desktop OAuth 2.0 或 YouTube Data API Key。程序只使用 Go 标准库。

## 安装与构建

支持 macOS 和 Linux。程序使用 `syscall.Flock` 锁定本地状态文件。需要 Go 1.26 或以上版本。

使用 Go 安装。

```sh
go install github.com/WyattJia/youtube-playlist-cli/cmd/ytpl@latest
```

也可以在项目根目录构建并检查本地二进制。

```sh
go build -o ytpl ./cmd/ytpl
./ytpl --version
./ytpl --help
./ytpl --json --help
```

`--state-dir PATH` 优先于环境变量 `YTPL_STATE_DIR`，默认是当前工作目录的 `.state`。状态目录保存本地凭据、歌单缓存、配额账本和迁移队列。全局选项可以放在命令或子命令前后。

`--json` 将结果写为单个 JSON 对象。成功时读取 `ok` 和 `data`，失败时读取 `error.code` 和 `error.message`。帮助返回 `{"ok":true,"help":"..."}`。交互登录地址写到标准错误。

## 配置 Google 凭据

在 Google Cloud 项目中启用 YouTube Data API v3，再按所需操作配置凭据。

| 凭据 | 用途 |
| --- | --- |
| API Key | 按 ID 读取 API 可公开访问的歌单及条目 |
| Desktop OAuth | 查看本人频道、枚举自己的歌单、读取已授权内容，以及创建、更新、删除、重排和迁移 |

公开读取优先使用可用的 OAuth。API Key 按 `--api-key`、`YTPL_API_KEY`、本地 `auth/api-key.txt` 的顺序取值。可以从文件或标准输入保存 Key。

```sh
./ytpl auth set-api-key --stdin < /path/to/api-key.txt
./ytpl sync --playlist 'https://music.youtube.com/playlist?list=PLExample'
```

程序通过 Google OAuth 授权和刷新，无需浏览器 Cookie。已有 `auth-ytm` 配置可从 Google Desktop 客户端 JSON 和对应的令牌 JSON 导入。将示例路径替换为实际文件路径。客户端使用 `installed` 格式。令牌需要含 `refresh_token` 或 `access_token`。只有 `access_token` 时，还需要有效的 `expires_at`（Unix 秒数）。

```sh
./ytpl auth import --client /path/to/auth-ytm/client.json --token /path/to/auth-ytm/token.json
./ytpl auth status
./ytpl whoami
```

如果只有客户端文件，省略 `--token`，再运行 `ytpl auth login`。在浏览器中打开终端显示的 `127.0.0.1` 地址，选择对应的 Google 账号和 YouTube 频道并授权。存在多个账号时可用 `--authuser N` 提示 Google 选择账号，`--timeout SECONDS` 可调整默认 300 秒的等待时间。授权回调使用 OAuth state 和 PKCE S256。

`auth status` 报告客户端、令牌和 API Key 是否配置。OAuth 令牌存在时，它和 `whoami` 都会核对频道 ID、标题及 API 返回的 handle。迁移前用 `whoami` 核对目标频道。遇到 `CHANNEL_NOT_FOUND` 时，检查该 Google 账号下的 YouTube 频道或品牌账号，再登录。

如果令牌出现 `refresh_token_expires_in`，Google Cloud OAuth 同意屏幕可能处于 **Testing**。外部应用在该模式下的刷新令牌通常 7 天过期。可以在 **Google Auth Platform → Audience** 将应用发布为 **In Production**，然后重新运行 `ytpl auth login`。收到 `AUTH_EXPIRED` 时也重新登录。已有导入文件需要覆盖时使用 `--force`。更换 OAuth `client_id` 会移除旧令牌。可以同时导入对应的新令牌，也可以重新登录。

凭据保存在状态目录的 `auth/` 中。目录权限为 `0700`，凭据文件为 `0600`。状态和错误输出隐藏凭据值。运行 `ytpl auth logout` 删除本地令牌，加 `--all` 也删除已保存的 OAuth 客户端和 API Key。

## 歌单命令

`PLAYLIST` 可以是歌单 ID 或带 `list` 参数的 YouTube / YouTube Music 链接。`VIDEO` 可以是 11 位视频 ID，或 watch、youtu.be、Shorts、embed 链接。

| 命令 | 行为 |
| --- | --- |
| `sync [--playlist PLAYLIST] [--with-items]` | 默认同步本人歌单的元数据。指定歌单时读取该歌单和条目，可只使用 API Key。 |
| `list [--remote]` | 默认列出本地缓存。`--remote` 列出当前 OAuth 用户的歌单并更新元数据缓存。 |
| `show PLAYLIST [--remote]` | 默认读取本地缓存。使用 `--remote` 同步并显示元数据和条目。 |
| `create --title TITLE [--description TEXT] [--privacy private\|unlisted\|public] [--dry-run]` | 创建歌单，默认 `private`。 |
| `update PLAYLIST [--title TITLE] [--description TEXT] [--privacy VALUE] [--dry-run]` | 先读取远端，保留未改的字段和 `podcastStatus`，只提交变化的 API 字段组。可用 `--description=""` 清空简介。 |
| `delete PLAYLIST --yes [--dry-run]` | 删除歌单。`--dry-run` 可单独用来预演。 |
| `add PLAYLIST [VIDEO...] [--file PATH] [--allow-duplicates] [--position N] [--dry-run]` | 文件每行一个视频。默认跳过远端或本次输入中重复的视频，按输入顺序追加。`--position` 从 0 开始。 |
| `remove PLAYLIST [--item-id ID]... [--video VIDEO]... [--all-occurrences] [--dry-run]` | 在指定歌单中解析条目。按视频默认只移除第一次出现的条目。 |
| `dedupe PLAYLIST [--keep first\|last] [--dry-run]` | 清理重复视频，默认保留第一次出现的条目。 |
| `reorder PLAYLIST (--move FROM TO \| --by title\|channel\|added\|reverse) [--dry-run]` | `--move` 从 1 开始。按标题、频道或添加时间升序排列，或反转歌单。批量排序保留最长公共子序列，只移动必要的条目。 |
| `export (PLAYLIST \| --all) --out DIR [--format json\|csv\|txt] [--remote]` | 默认 JSON。CSV 包含条目字段，TXT 每行一个可导入的 YouTube 视频链接。导出需要条目快照，或者使用 `--remote` 实时读取。 |

已同步的内容是本地快照。条目增删、去重和重排后，使用 `sync --playlist PLAYLIST` 或 `show PLAYLIST --remote` 刷新。带 `--dry-run` 的命令只预演远端写入，可以为生成计划读取远端并消耗配额。创建或插入的响应中断时，先读回远端核对结果再决定是否重跑。

歌单导出和迁移报告的 CSV 会为可能被电子表格当成公式的单元格加上单引号。触发字符包括 `=`、`+`、`-`、`@`、制表符和回车，也会检查前导空白后的内容。JSON 保存原始值。

## 配额

`ytpl quota` 显示太平洋时间配额日、累计用量、按 API 操作统计的请求次数、是否收到远端配额耗尽，以及下次太平洋午夜重置时间。默认日上限为 10,000 单位。每页读取或每 50 个视频的详情查询计 1 单位，每次写入尝试计 50 单位。请求失败后重试也会计费。本地账本只计入该状态目录中的请求。Google Cloud 的实际用量还可能包含其他客户端的请求。

| 命令 | 行为 |
| --- | --- |
| `quota` | 查看账本。 |
| `quota --limit N` | 设置本地日上限。按 Google Cloud 项目实际配额设置。 |
| `quota --reset-exhausted` | 清除当天的远端耗尽标记，保留已记录的用量。 |
| `migrate run` | 默认保留 500 单位，迁移在保留线前暂停。 |
| `migrate run --budget N` | 显式限制本次运行的总花费。可以使用预留额度，仍受日上限约束。 |
| `migrate run --max-writes N` | 额外限制本次写入尝试次数，包括重试。 |

当服务返回 `QUOTA_EXCEEDED` 时，批次保存进度并停在当天，输出重置时间。不同进程共用状态目录时，配额检查和扣减在同一文件锁内完成。

## 网易云歌单迁移

先确认 OAuth 对应的目标频道。将源迁移计划和可选的旧迁移记录传给 `migrate init`。导入只读取这两个文件，在状态目录中生成独立的 `migration-queue.json`。

```sh
./ytpl whoami
./ytpl migrate init --plan 'YouTube Music迁移计划.json' --record '迁移记录.json'
./ytpl migrate status
./ytpl migrate run --playlist 12345 --dry-run --json
./ytpl migrate run --playlist 12345 --max-writes 10 --json
./ytpl migrate run --json
./ytpl migrate export-report --out out/migration.csv --format csv
```

- `init` 支持 `--include IDS` 与 `--exclude IDS`，用逗号分隔网易云歌单 ID。`--record` 文件必须包含值为 JSON 对象的 `playlists` 字段，允许空对象 `{}`。计划和旧记录均提供备份提交号时，两者必须一致。两边给出不同的 YouTube 目标 ID 时，该歌单标记为 `conflict` 并跳过。保留队列时直接继续运行，重建已有队列需要 `--force`。
- `run` 每次读取远端条目，按视频 ID 的出现次数核对。计划增补或重排后，旧记录逐次验证的等价视频仍可用于核对。迁移保留远端已有条目和顺序，按计划顺序追加缺少的条目。额外条目计为 `extraRemoteTracksCount`。没有匹配视频的曲目记为 `unmatched`。
- 创建前先保存 `creationPending`。中断后的下一次运行按歌单名称和简介里的网易云来源标记搜索目标。一个匹配即恢复绑定，多个匹配标记冲突。没有匹配时保留待确认状态，可稍后重试。确认远端确实未创建后，可执行 `ytpl migrate run --playlist NETEASE_ID --confirm-recreate`。`--confirm-recreate` 必须指定单个 `--playlist`。
- 插入前先保存 `pendingInsert`。响应中断或出现 5xx 错误时暂停该歌单，等下一次运行从远端确认曲目出现次数增加。如果暂时读不到结果，可以稍后重试。确认远端确实未插入后，可执行 `ytpl migrate run --playlist NETEASE_ID --confirm-reinsert`。`--confirm-reinsert` 必须指定单个 `--playlist`。
- 视频不存在或与另一个系列歌单冲突时，该曲目记为 `failed_permanent`，批次继续处理后续曲目。歌单不存在或已满时，批次继续处理其他歌单。临时失败记为 `failed_transient`，连续 3 次时暂停批次。服务端要求的等待时间超出请求内重试上限时，批次暂停，JSON 的 `data.stopped.retryAfter` 提供最早重试时间。恢复运行同一队列时会遵守这个时间。
- `retry-failed` 可把已确认失败的临时错误设为待重试。修复永久错误或腾出歌单空间后，用 `--include-permanent` 重新启用这些记录。`export-report` 支持 CSV 和 JSON，默认 JSON，包含逐歌单及逐曲目的状态。
- 所有迁移子命令接受 `--queue-file PATH`。访问同一队列时使用同一 `--state-dir`，以共用进程锁。批次受预算、认证、配额或限流影响而正常暂停时，JSON 中的 `data.stopped` 提供原因。自动化可结合 `migrate status --json` 判断是否已处理完。

## 离线验证

```sh
go vet ./...
go test -race -count=1 ./...
```

测试使用本地 mock HTTP 服务、模拟凭据、可注入时钟和临时目录。Google 文档提供 OAuth 配置与配额明细。 [Desktop OAuth](https://developers.google.com/identity/protocols/oauth2/native-app) · [YouTube Data API 配额](https://developers.google.com/youtube/v3/determine_quota_cost)

## 许可证

项目采用 MIT 许可证。 [查看 LICENSE](LICENSE)
