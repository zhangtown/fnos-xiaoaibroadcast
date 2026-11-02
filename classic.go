// 经典故事集：随包内置一份民间故事，首次启动就落到数据目录里，
// 之后直接读盘（不再调大模型、也不套 600 字裁切），用户自己往目录里丢 .txt 也能被列出来。
package main

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed stories/*.txt
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
	entries, err := classicFS.ReadDir("stories")
	if err != nil {
		return
	}
	added := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		dst := filepath.Join(dir, e.Name())
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		data, err := fs.ReadFile(classicFS, "stories/"+e.Name())
		if err != nil {
			continue
		}
		if err := os.WriteFile(dst, data, 0o644); err == nil {
			added++
		}
	}
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
	Name  string `json:"name"`
	Chars int    `json:"chars"`
}

// listClassics 列数据目录里的故事（名字 + 字数）。
func (a *App) listClassics() []classicEntry {
	dir := a.storiesDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]classicEntry, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".txt") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".txt")
		chars := 0
		if data, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
			chars = len([]rune(strings.TrimSpace(string(data))))
		}
		out = append(out, classicEntry{Name: name, Chars: chars})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// readClassic 读一篇，名字里不许带路径。
func (a *App) readClassic(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", os.ErrNotExist
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", os.ErrPermission
	}
	data, err := os.ReadFile(filepath.Join(a.storiesDir(), name+".txt"))
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
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	text, err := a.readClassic(in.Name)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "读不到这篇故事：" + in.Name})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "name": in.Name, "text": text, "chars": len([]rune(text)),
	})
}
