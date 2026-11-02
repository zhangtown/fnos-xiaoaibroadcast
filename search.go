// 曲库搜索：把所有配置目录里的音频建一份索引（带缓存），按关键字匹配。
// 索引只在内存里，5 分钟内复用；文件增删多的话在页面上点「刷新」即可重建。
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type searchEntry struct {
	Name   string `json:"name"`
	Rel    string `json:"rel"`
	Root   int    `json:"root"`
	Folder string `json:"folder"` // 相对音源根的目录，便于看归属
	URL    string `json:"url"`
}

type searchIndex struct {
	mu      sync.Mutex
	built   time.Time
	entries []searchEntry
	token   string // 音源 token 变了要重建（URL 里带 token）
	base    string
}

const searchIndexTTL = 5 * time.Minute

// buildIndex 遍历所有曲库目录，收集音频文件。
func (a *App) buildIndex(cfg Config) []searchEntry {
	dirs := cfg.Music.dirs()
	if len(dirs) == 0 {
		return nil
	}
	base := cfg.Music.publicBase()
	out := make([]searchEntry, 0, 2048)
	for rootIdx, root := range dirs {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") && p != root {
					return filepath.SkipDir
				}
				return nil
			}
			if !audioExt[strings.ToLower(filepath.Ext(d.Name()))] {
				return nil
			}
			if len(out) >= 50000 {
				return filepath.SkipAll
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			entry := searchEntry{Name: d.Name(), Rel: relSlash, Root: rootIdx, Folder: filepath.ToSlash(filepath.Dir(rel))}
			if entry.Folder == "." {
				entry.Folder = ""
			}
			if base != "" {
				entry.URL = musicURL(base, cfg.Music.Token, rootIdx, relSlash)
			}
			out = append(out, entry)
			return nil
		})
	}
	return out
}

// index 取索引（必要时重建）。
func (a *App) searchIdx(cfg Config) []searchEntry {
	a.idx.mu.Lock()
	defer a.idx.mu.Unlock()
	base := cfg.Music.publicBase()
	if a.idx.entries != nil && time.Since(a.idx.built) < searchIndexTTL &&
		a.idx.token == cfg.Music.Token && a.idx.base == base {
		return a.idx.entries
	}
	entries := a.buildIndex(cfg)
	a.idx.entries, a.idx.built, a.idx.token, a.idx.base = entries, time.Now(), cfg.Music.Token, base
	return entries
}

// handleMusicSearch 按关键字搜歌：GET /api/music/search?q=xxx&limit=80
func (a *App) handleMusicSearch(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	limit := 80
	if q == "" {
		writeJSON(w, 200, map[string]any{"ok": true, "items": []searchEntry{}, "total": 0})
		return
	}
	all := a.searchIdx(cfg)
	// 先按「名字命中」还是「路径命中」排个序：名字命中的更可能是想要的
	var nameHit, pathHit []searchEntry
	for _, e := range all {
		lower := strings.ToLower(e.Name)
		switch {
		case strings.Contains(lower, q):
			nameHit = append(nameHit, e)
		case strings.Contains(strings.ToLower(e.Folder), q):
			pathHit = append(pathHit, e)
		}
	}
	sort.Slice(nameHit, func(i, j int) bool { return len(nameHit[i].Name) < len(nameHit[j].Name) })
	items := append(nameHit, pathHit...)
	total := len(items)
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, 200, map[string]any{"ok": true, "items": items, "total": total, "indexed": len(all)})
}
