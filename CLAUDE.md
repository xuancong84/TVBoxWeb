# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

夜帧影视 (TVBoxWeb): a Go backend + Vue3 frontend re-implementation of the HarmonyOS TVBox client. It is both a standalone binary (`cmd/tvbox`) and an importable Go library (root package `tvbox`, `tvbox.go`) that serves an embedded frontend. It ships no media content; sources come from user-imported TVBox JSON configs. Docs/comments are mostly in Chinese. See `README.md`, `PWA.md`, `WEBSOCKET.md`, `UPGRADE_GUIDE.md` for feature details.

## Commands

```bash
./run.sh                          # npm install + build frontend, go build -> ./yezhen, run on :8090 (data in ./data)
./build.sh [version]              # frontend build + cross-compile all platforms into release/<version>/

cd web && npm run dev             # Vite dev server on :5173, proxies /api -> localhost:8090 (run Go backend separately)
cd web && npm run build           # vue-tsc --noEmit (type-check) && vite build -> ../dist

go build -o yezhen ./cmd/tvbox && ./yezhen -addr :8090 -data ./data
go test ./...                     # backend tests
go test ./internal/api -run TestName   # single test

cd web && node scripts/check-seek.cjs  # frontend regression checks (jsdom, compile real .vue files);
                                       # also npm run check:panel | check:wd-auth | check:seek | check:mobile
```

There is no linter configured.

## Architecture

**Embedding / build coupling.** Vite outputs to `/dist` (repo root, `emptyOutDir`), and `tvbox.go` embeds it via `//go:embed all:dist`. So frontend changes only reach the Go binary after `npm run build`. `dist/` is **committed** (hashed asset filenames churn in git status — expected when rebuilding), and `web/public/sw.js` is copied into `dist/sw.js`. The Dockerfile rebuilds `dist` in a Node stage and overwrites it before `go build`. Version is injected via `-ldflags -X .../internal/api.AppVersion` / `BuildTime`.

**Backend (`internal/`)** — `tvbox.go` wires everything (`New`, `Options`, `ListenAndServe`, `Handler`) and `internal/api/api.go` (large) registers all HTTP routes. Standard `net/http` mux; deps are only gorilla/websocket and google/uuid. Packages:
- `store` — JSON persistence (`data/store.json`: config, history, favorites) plus site/live/line-score caches
- `cms` / `crawler` / `resolve` — TVBox site protocols (CMS APIs, spider/crawler sources), play-URL resolution
- `live`, `liveprobe` — live channels, EPG, availability probing; `api/livescan.go`
- `api/remux.go` — media remuxing (ffmpeg-style process mgmt; `procgroup_*.go`, `priority_*.go` are OS-specific)
- `httpc`, `netguard` — outbound HTTP client and SSRF/private-network guarding (the `/api/proxy` video proxy rewrites m3u8 segment URLs to bypass CORS)
- `pyspider` — Python 站源（type=3 且 api 指向 `.py`）：Go 常驻一个 Python worker（`pyfiles/worker.py`、`pyfiles/base/spider.py` 内嵌在二进制里，启动时释放到 `data/pyspider/`），stdin/stdout 行 JSON 协议调用站源的 home/category/detail/search/play/localProxy。`cms.Cms` 对 `site.Kind=="py"` 走 `cms/pyadapter.go`；播放用令牌 `pyplay:`（`/api/resolve` → `api/pyresolve.go` 调 playerContent）和 `pyproxy:`（`/api/proxy` 调 localProxy，m3u8 里的本地代理地址会被换成令牌）。需要 Python 3 + `requests pycryptodome lxml beautifulsoup4 pyquery`（`PYSPIDER_PYTHON` 指定解释器，默认依次找 `~/miniconda3/bin/python`、`python3`）；缺依赖则这些站源保持不可用。站源默认不启用，需在设置里手动打开。
- `webdav` — WebDAV/网盘 aggregation with auth; `ws` — WebSocket hub; `metrics`, `imgcache`, `pinyin` (search), `quality`, `upgrade` (self-update)
- Platform-specific files use build-tag suffixes (`_linux`, `_other`, `_unix`, `_windows`).

**Frontend (`web/src`)** — Vue3 + TS + Element Plus + Pinia + Vue Router. `api.ts` is the HTTP client, `ws.ts` the WebSocket client, `idb.ts`/`vodCache.ts` IndexedDB caching, `pwa.ts` + `public/sw.js` the service worker. `views/PlayerView.vue` is the central/complex piece (hls.js for HLS, mpegts.js for FLV/M2TS live; player UI in `components/player/`). Heavy libs are split into separate Vite chunks (`vite.config.ts`) and loaded on demand.

**Frontend tests** (`web/scripts/check-*.cjs`) are not a test framework: each script compiles the real `.vue` file with jsdom and asserts behavior. Update them when changing the player UI they cover.

## Misc

- `example/` holds sample library usages (`basic`, `custom-mux`); `yezhen` is a built binary (gitignored); `data/` is runtime state.
- When bumping the version, keep `build.sh` default, `internal/api.AppVersion`, and release workflow (`.github/workflows/build-release.yml`) consistent.
