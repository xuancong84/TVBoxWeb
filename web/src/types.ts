// 与 Go 后端 model 包一一对应的前端类型。

/** 站源健康度（GET /api/sites/health） */
export interface SiteHealthStatus {
  key: string
  status: 'healthy' | 'slow' | 'dead' | 'unknown' | string
  lastSuccess: number
  errorRate: number
  avgMs: number
}

/** 一条「单仓源」（展开后的叶子订阅）引用，标识是谁带来了某个站源/直播源 */
export interface SubRef {
  name: string
  source: string
}

export interface Site {
  key: string
  alts?: string[]
  dupOf?: string
  name: string
  type: number
  api: string
  searchable: boolean
  quickSearch: boolean
  ext: string
  usable: boolean
  reason: string
  enabled: boolean
  /** 最近一次内容更新时该站源返回了 0 条（域名过期/空列表等），不等于不可用 */
  dead: boolean
  /** 最近一次内容更新时该站源报告的影片总数，没抓取过/失败时是 0 */
  count: number
  /** count=0 时的严重程度细分：1=连上了确实空 2=暂时不可用 3=过期/损坏/失联 */
  severity: number
  /** 贡献了这个站源的单仓源列表（GET /api/sites 现查现填） */
  /** 贡献该站源的单仓源：下标，指向 GET /api/sites/subrefs 的表（体积小） */
  subIdx?: number[]
  /** 前端按 subIdx 解析出来的单仓源（siteInfo.ts 填；服务端不返回） */
  subs?: SubRef[]
}

export interface Category {
  id: string
  name: string
  pid: string
}

export interface Vod {
  id: string
  name: string
  pic: string
  remark: string
  year: string
  area: string
  actor: string
  director: string
  content: string
  typeName: string
  from: string
  playUrl: string
  siteKey: string
  siteName: string
  // 总集数快照：首页「最近更新」这类卡片没有 from/playUrl（落盘前就被
  // 砍掉了，省存储），后端会顺手存一个总集数算好的整数；有 playUrl 的
  // 地方（点播/搜索）不用这个字段，前端直接现算更准。
  epCount?: number
  // 排行指标（站源没给/填 0 时缺省）：ts = vod_time 的 Unix 秒
  ts?: number
  score?: number
  douban?: number
  hits?: number
  hitsWeek?: number
  hitsMonth?: number
  up?: number
}

export interface Episode {
  name: string
  url: string
  urls: string[]
}

export interface Line {
  name: string
  direct: boolean
  eps: Episode[]
}

export interface VodPage {
  items: Vod[]
  page: number
  pageCount: number
  total: number
}

export interface FilterValue {
  n: string
  v: string
}

export interface FilterGroup {
  key: string
  name: string
  values: FilterValue[]
}

export interface Channel {
  name: string
  url: string
  logo: string
  group: string
  urls: string[]
  /** 最近一次测试结果（真实播放 或 后台可播性扫描）：'ok'=能播，'fail'=播不通
   *（24 小时后过期需重测），不带=没测过/已过期 */
  status?: 'ok' | 'fail'
}

/** 「可播频道」分类的后台扫描进度（GET /api/live/scan/status） */
export interface LiveScanStatus {
  total: number
  scanned: number
  playable: number
  scanning: boolean
}

export interface ChannelGroup {
  name: string
  channels: Channel[]
}

export interface LiveDir {
  name: string
  count: number
  children: LiveDir[]
  channels: Channel[]
}

/** 单条播放线路的轻量测速结果（GET /api/live/lines/probe），只取开头 16KB */
export interface LineQuality {
  url: string
  ok: boolean
  ms: number
  kbps: number
}

export interface LiveScore {
  name: string
  url: string
  ok: boolean
  ms: number
  count: number
  alive: number
  sampled: number
  score: number
  state: string
}

export interface HistItem {
  siteKey: string
  siteName: string
  vodId: string
  name: string
  pic: string
  epName: string
  epIndex: number
  // 这部剧总集数：播放页现查现填（player.eps.length）；0/未知时不显示。
  epCount?: number
  from: string
  pos: number
  dur: number
  time: number
  watchedEps?: number[]  // 已看过的集数列表（分集管理）
}

export interface FavItem {
  siteKey: string
  siteName: string
  vodId: string
  name: string
  pic: string
  groupId?: string  // 所属分组 ID，空字符串表示默认分组
}

export interface FavGroup {
  id: string
  name: string
  time: number
  count?: number  // 该分组内收藏数量（由后端聚合计算）
}

/** 一条订阅源（GET /api/subscriptions）。可以同时启用多条，站源/直播源会合并去重。 */
export interface Subscription {
  id: string
  name: string
  source: string
  enabled: boolean
  sites: number
  lives: number
  parses: number
  updatedAt: number
  err?: string
}

export interface WebdavSource {
  id: string
  name: string
  url: string
  user: string
  pass: string
  root: string
}

export interface WebdavEntry {
  href: string
  name: string
  isDir: boolean
  size: number
  mtime: string
}

export interface WebdavEp {
  name: string
  url: string
  href: string
  season: number
  episode: number
}

export interface WebdavVod {
  name: string
  kind: string
  year: string
  /** 所在绝对目录（URL 路径） */
  dir: string
  /**
   * 相对挂载根的目录路径（已解码），如「凡人/01 风起天南」。
   * 用于还原目录层级分组；直接位于挂载根下时为空串。
   * 老版本后端不返回该字段，前端会退化为按 dir 推断。
   */
  relDir?: string
  eps: WebdavEp[]
}

export interface WdHistory {
  srcId: string
  srcName: string
  vodName: string
  dir: string
  epIndex: number
  epName: string
  url: string
  pos: number
  dur: number
  time: number
}

export interface RawLive {
  name: string
  url: string
  /** 是否参与直播频道合并（GET /api/live/sources 现查现填，空启停列表=全部启用） */
  enabled?: boolean
  /** 贡献了这条直播源的单仓源下标，指向 GET /api/sites/subrefs */
  subIdx?: number[]
}

export interface EpgProgram {
  start: number
  stop: number
  title: string
  desc: string
}

export interface EpgResult {
  channel: string
  programs: EpgProgram[]
  now: EpgProgram | null
  next: EpgProgram | null
}

export interface QLevel {
  height: number
  width: number
  bps: number
  url: string
  name: string
}

export interface CrawlResult {
  at: number
  sites: number
  alive: number
  dead: number
  items: number
  cfgUpdated: boolean
  ok: boolean
  msg: string
}
