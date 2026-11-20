package cli

const globalHelp = `ytpl 管理 YouTube / YouTube Music 歌单

用法  ytpl [全局选项] <命令> [参数]

命令
  auth       导入、登录、配置 API Key、验证频道或登出
  whoami     验证当前 OAuth 凭据对应的 YouTube 频道
  quota      查看和设置太平洋时间配额账本
  sync       将远端歌单同步到本地缓存
  list       列出歌单
  show       显示歌单和条目
  create     创建歌单，默认为 private
  update     修改歌单标题、简介或可见性
  delete     删除歌单，需要 --yes
  add        批量添加视频
  remove     按条目 ID 或视频删除
  dedupe     清理重复视频
  reorder    移动条目或排序
  export     导出 JSON、CSV 或 TXT
  migrate    导入、运行及查看网易云歌单迁移队列

全局选项可放在命令前后
  --state-dir PATH  状态目录，默认取 YTPL_STATE_DIR 或当前目录下的 .state
  --api-key KEY     公开读取凭据，默认取 YTPL_API_KEY 或已保存的 API Key
  --json            输出结构化 JSON
  --help, -h        帮助，也可运行 ytpl <命令> --help
  --version, -v     版本
`

var commandHelp = map[string]string{
	"auth": `ytpl auth <子命令> [参数]
  import --client PATH [--token PATH] [--force]
  set-api-key [KEY] [--stdin]
  login [--authuser N] [--timeout SECONDS]
  status
  logout [--all]
`,
	"auth import":      "ytpl auth import --client PATH [--token PATH] [--force]\n导入 Google Cloud Desktop OAuth JSON，可同时导入已有令牌。\n",
	"auth set-api-key": "ytpl auth set-api-key [KEY] [--stdin]\n保存仅用于公开读取的 YouTube Data API Key。\n",
	"auth login":       "ytpl auth login [--authuser N] [--timeout SECONDS]\n在浏览器中打开终端给出的回环地址。默认等待 300 秒。\n",
	"auth status":      "ytpl auth status\n显示凭据配置状态，存在 OAuth 令牌时核对当前频道。\n",
	"auth logout":      "ytpl auth logout [--all]\n删除本地令牌。--all 同时删除 OAuth 客户端和已保存的 API Key。\n",
	"whoami":           "ytpl whoami\n验证 OAuth 用户的频道 ID、标题和可用的 handle。\n",
	"quota":            "ytpl quota [--limit N] [--reset-exhausted]\n查看配额使用量。--reset-exhausted 清除当天的远端耗尽标记。\n",
	"sync":             "ytpl sync [--playlist ID_OR_URL] [--with-items]\n指定歌单时同步该歌单及其条目，否则同步本人歌单。\n",
	"list":             "ytpl list [--remote]\n列出本地缓存或当前 OAuth 用户的远端歌单。\n",
	"show":             "ytpl show PLAYLIST [--remote]\n显示指定歌单，本地未同步的条目会标明。\n",
	"create":           "ytpl create --title TITLE [--description TEXT] [--privacy private|unlisted|public] [--dry-run]\n",
	"update":           "ytpl update PLAYLIST [--title TITLE] [--description TEXT] [--privacy private|unlisted|public] [--dry-run]\n",
	"delete":           "ytpl delete PLAYLIST --yes [--dry-run]\n单用 --dry-run 可查看计划。\n",
	"add":              "ytpl add PLAYLIST [VIDEO_OR_URL...] [--file PATH] [--allow-duplicates] [--position N] [--dry-run]\n文件每行一个视频。--position 从 0 开始，默认追加。\n",
	"remove":           "ytpl remove PLAYLIST [--item-id ID]... [--video VIDEO_OR_URL]... [--all-occurrences] [--dry-run]\n按视频匹配时默认仅删除第一次出现的条目。\n",
	"dedupe":           "ytpl dedupe PLAYLIST [--keep first|last] [--dry-run]\n默认保留第一次出现的条目。\n",
	"reorder":          "ytpl reorder PLAYLIST (--move FROM TO | --by title|channel|added|reverse) [--dry-run]\n--move 的两个输入均从 1 开始。\n",
	"export":           "ytpl export (PLAYLIST | --all) --out DIR [--format json|csv|txt] [--remote]\n默认 JSON。\n",
	"migrate": `ytpl migrate <子命令> [参数]
  init --plan PATH [--record PATH] [--queue-file PATH] [--include IDS] [--exclude IDS] [--force]
  status [--queue-file PATH]
  run [--queue-file PATH] [--budget N] [--max-writes N] [--playlist NETEASE_ID] [--confirm-recreate] [--confirm-reinsert] [--dry-run]
  retry-failed [--queue-file PATH] [--include-permanent]
  export-report [--queue-file PATH] --out PATH [--format csv|json]
`,
	"migrate init":   "ytpl migrate init --plan PATH [--record PATH] [--queue-file PATH] [--include IDS] [--exclude IDS] [--force]\n筛选项使用逗号分隔的网易云歌单 ID。\n",
	"migrate status": "ytpl migrate status [--queue-file PATH]\n",
	"migrate run": `ytpl migrate run [--queue-file PATH] [--budget N] [--max-writes N] [--playlist NETEASE_ID] [--confirm-recreate] [--confirm-reinsert] [--dry-run]
默认保留 500 单位日配额。--budget 指定本次配额，受每日上限约束。
--confirm-recreate 和 --confirm-reinsert 都需要 --playlist NETEASE_ID。
`,
	"migrate retry-failed":  "ytpl migrate retry-failed [--queue-file PATH] [--include-permanent]\n",
	"migrate export-report": "ytpl migrate export-report [--queue-file PATH] --out PATH [--format csv|json]\n默认 JSON。\n",
}

func help(command string) string {
	if command == "" {
		return globalHelp
	}
	return commandHelp[command] + "\n全局选项请运行 ytpl --help。\n"
}
