// 经典故事集：随包内置一批民间故事、寓言、童话和历史人物故事，按分类放在子目录里。
// 首次启动就落到数据目录（stories/<分类>/<名字>.txt），之后直接读盘——不再调大模型、
// 也不套生成的 600 字裁切；用户自己往目录里丢 .txt（可建新分类目录）也会被列出来。
package main

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed stories
var classicFS embed.FS

// storiesDir 指向数据目录下 stories/（跟着配置一起保留，卸载重装不丢）。
func (a *App) storiesDir() string {
	return filepath.Join(dataDir(), "stories")
}

// seedStories 把内置故事写到磁盘；已存在的文件不覆盖（用户改过就按用户的来）。
func (a *App) seedStories() {
	dir := a.storiesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.store.AddLog("系统", "", false, "故事目录建不了："+err.Error())
		return
	}
	added := 0
	_ = fs.WalkDir(classicFS, "stories", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(p), ".txt") {
			return nil
		}
		rel := strings.TrimPrefix(p, "stories/")
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil
		}
		data, err := fs.ReadFile(classicFS, p)
		if err != nil {
			return nil
		}
		if err := os.WriteFile(dst, data, 0o644); err == nil {
			added++
		}
		return nil
	})
	if added > 0 {
		a.store.AddLog("系统", "", true, "经典故事已落盘 "+itoa(added)+" 篇")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

type classicEntry struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Chars    int    `json:"chars"`
}

// listClassics 列数据目录里的故事（分类 + 名字 + 字数）。
func (a *App) listClassics() []classicEntry {
	root := a.storiesDir()
	out := make([]classicEntry, 0, 64)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".txt") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		cat := path.Dir(rel)
		if cat == "." {
			cat = "其他"
		}
		chars := 0
		if data, err := os.ReadFile(p); err == nil {
			chars = len([]rune(strings.TrimSpace(string(data))))
		}
		if chars == 0 {
			return nil
		}
		out = append(out, classicEntry{
			Category: cat,
			Name:     strings.TrimSuffix(d.Name(), path.Ext(d.Name())),
			Chars:    chars,
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// readClassic 读一篇；分类和名字里都不许带路径分隔符。
func (a *App) readClassic(category, name string) (string, error) {
	category = strings.TrimSpace(category)
	name = strings.TrimSpace(name)
	if name == "" {
		return "", os.ErrNotExist
	}
	for _, s := range []string{category, name} {
		if strings.ContainsAny(s, `/\`) || strings.Contains(s, "..") {
			return "", os.ErrPermission
		}
	}
	p := filepath.Join(a.storiesDir(), category, name+".txt")
	if category == "" || category == "其他" {
		p = filepath.Join(a.storiesDir(), name+".txt")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (a *App) handleClassics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "items": a.listClassics(), "dir": a.storiesDir()})
}

func (a *App) handleClassicGet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Category string `json:"category"`
		Name     string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	text, err := a.readClassic(in.Category, in.Name)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "读不到这篇故事：" + in.Name})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "name": in.Name, "text": text, "chars": len([]rune(text)),
	})
}
