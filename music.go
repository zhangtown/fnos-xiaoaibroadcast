// 曲库：列 NAS 上的音乐目录，并起一个只读的 HTTP 音源口给音箱取。
//
// 为什么不直接借 xiaomusic 的 /music：它的路由在各版本里不稳定，而且路径规则要看它配置。
// 自己起一个端口更可控：路径里带一段随机 token，只读、只暴露音乐目录。
package main

import (
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type MusicLib struct {
	Dirs   []string `json:"dirs"`   // 音乐根目录（NAS 上的绝对路径）
	Folder string   `json:"folder"` // 当前选的文件夹（绝对路径）
	Port   int      `json:"port"`   // 音源端口，0 = 关闭
	Token  string   `json:"token"`  // 路径里的随机段，防止被随便扫
	Base   string   `json:"base"`   // 可选：手工指定对外地址，例如 http://<NAS局域网IP>:18090
}

func defaultMusicLib() MusicLib {
	return MusicLib{
		Dirs:  []string{"/vol3/1000/music", "/vol2/1000/music"},
		Port:  18090,
		Token: randomToken(10),
	}
}

func (m MusicLib) dirs() []string {
	out := make([]string, 0, len(m.Dirs))
	for _, d := range m.Dirs {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, strings.TrimSuffix(d, "/"))
		}
	}
	return out
}

var audioExt = map[string]bool{
	".mp3": true, ".m4a": true, ".aac": true, ".flac": true,
	".wav": true, ".ogg": true, ".wma": true,
}

var (
	lanIPOnce sync.Once
	lanIPVal  string
)

// localLANIP 拿到本机在局域网里的地址（拨一次 UDP，不发包）。
func localLANIP() string {
	lanIPOnce.Do(func() {
		for _, target := range []string{"223.5.5.5:53", "192.168.1.1:80"} {
			conn, err := net.DialTimeout("udp", target, 2*time.Second)
			if err != nil {
				continue
			}
			if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
				lanIPVal = addr.IP.String()
			}
			conn.Close()
			if lanIPVal != "" {
				return
			}
		}
	})
	return lanIPVal
}

// publicBase 是音箱能访问到的音源前缀。
func (m MusicLib) publicBase() string {
	if strings.TrimSpace(m.Base) != "" {
		return strings.TrimSuffix(strings.TrimSpace(m.Base), "/")
	}
	ip := localLANIP()
	if ip == "" || m.Port == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", ip, m.Port)
}

// rootOf 找出某个文件夹属于哪个根目录，以及它到根目录的相对路径。
func rootOf(dirs []string, folder string) (int, string, bool) {
	for i, d := range dirs {
		if folder == d || strings.HasPrefix(folder, d+string(os.PathSeparator)) {
			rel, err := filepath.Rel(d, folder)
			if err != nil {
				return i, "", true
			}
			return i, filepath.ToSlash(rel), true
		}
	}
	return 0, "", false
}

// musicHandler 只读地把 /m/<token>/<根目录序号>/<相对路径> 映射到音乐目录。
func (a *App) musicHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.store.Get()
		prefix := "/m/" + cfg.Music.Token + "/"
		if cfg.Music.Token == "" || !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		i := strings.IndexByte(rest, '/')
		if i <= 0 {
			http.NotFound(w, r)
			return
		}
		idx, err := strconv.Atoi(rest[:i])
		rel := rest[i+1:]
		dirs := cfg.Music.dirs()
		if err != nil || idx < 0 || idx >= len(dirs) || rel == "" {
			http.NotFound(w, r)
			return
		}
		root := dirs[idx]
		full := filepath.Join(root, filepath.FromSlash(rel))
		if !strings.HasPrefix(full, root+string(os.PathSeparator)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if st, err := os.Stat(full); err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, full) // ServeFile 自带 Range 支持，播放器拖进度靠它
	})
}

func (a *App) startMusicServer() {
	cfg := a.store.Get()
	if cfg.Music.Port == 0 {
		return
	}
	addr := fmt.Sprintf(":%d", cfg.Music.Port)
	srv := &http.Server{Addr: addr, Handler: a.musicHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			a.store.AddLog("系统", "", false, fmt.Sprintf("音源端口 %s 启动失败：%v", addr, err))
		}
	}()
}

type musicFolder struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// handleMusicFolders 列出可选的音频文件夹（根目录及其下 3 层）。
func (a *App) handleMusicFolders(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	out := make([]musicFolder, 0, 16)
	for _, root := range cfg.Music.dirs() {
		base := filepath.Base(root)
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			depth := 0
			if rel != "." {
				depth = strings.Count(filepath.ToSlash(rel), "/") + 1
			}
			if depth > 3 {
				return filepath.SkipDir
			}
			ents, _ := os.ReadDir(p)
			n := 0
			for _, e := range ents {
				if !e.IsDir() && audioExt[strings.ToLower(filepath.Ext(e.Name()))] {
					n++
				}
			}
			label := base
			if rel != "." {
				label = base + "/" + filepath.ToSlash(rel)
			}
			out = append(out, musicFolder{Path: p, Label: label, Count: n})
			return nil
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "folders": out, "current": cfg.Music.Folder})
}

type musicEntry struct {
	Name string `json:"name"`
	Rel  string `json:"rel"`
	URL  string `json:"url"`
}

// handleMusicList 列出当前文件夹里的音频（递归，最多 800 条）。
func (a *App) handleMusicList(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	dirs := cfg.Music.dirs()
	if len(dirs) == 0 {
		writeJSON(w, 200, map[string]any{"ok": true, "items": []musicEntry{}, "folder": "", "base": ""})
		return
	}
	folder := cfg.Music.Folder
	if folder == "" {
		folder = dirs[0]
	}
	rootIdx, _, ok := rootOf(dirs, folder)
	if !ok {
		rootIdx = 0
		folder = dirs[0]
	}
	if st, err := os.Stat(folder); err != nil || !st.IsDir() {
		folder = dirs[0]
		rootIdx = 0
	}
	root := dirs[rootIdx]
	base := cfg.Music.publicBase()

	out := make([]musicEntry, 0, 64)
	truncated := false
	_ = filepath.WalkDir(folder, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && p != folder {
				return filepath.SkipDir
			}
			return nil
		}
		if !audioExt[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		if len(out) >= 800 {
			truncated = true
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		entry := musicEntry{Name: d.Name(), Rel: relSlash}
		if base != "" {
			entry.URL = fmt.Sprintf("%s/m/%s/%d/%s", base, cfg.Music.Token, rootIdx,
				(&url.URL{Path: relSlash}).EscapedPath())
		}
		out = append(out, entry)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })

	writeJSON(w, 200, map[string]any{
		"ok": true, "folder": folder, "base": base,
		"dirs": dirs, "items": out, "truncated": truncated,
	})
}

// handleMusicFolder 记住用户选的文件夹。
func (a *App) handleMusicFolder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Folder string `json:"folder"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	folder := strings.TrimSpace(in.Folder)
	dirs := a.store.Get().Music.dirs()
	if folder != "" {
		if _, _, ok := rootOf(dirs, folder); !ok {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "这个文件夹不在音乐根目录里"})
			return
		}
		if st, err := os.Stat(folder); err != nil || !st.IsDir() {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "文件夹不存在"})
			return
		}
	}
	_ = a.store.Update(func(c *Config) error {
		c.Music.Folder = folder
		return nil
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func randomToken(n int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "xiaoai"
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// handleMusicSettings 保存曲库根目录 / 音源端口 / 对外地址覆盖。
func (a *App) handleMusicSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Dirs []string `json:"dirs"`
		Port int      `json:"port"`
		Base string   `json:"base"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	clean := make([]string, 0, len(in.Dirs))
	for _, d := range in.Dirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		clean = append(clean, strings.TrimSuffix(d, "/"))
	}
	if len(clean) == 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "至少填一个音乐目录"})
		return
	}
	if in.Port < 0 || in.Port > 65535 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "端口要在 0-65535 之间"})
		return
	}
	base := strings.TrimSpace(in.Base)
	oldPort := a.store.Get().Music.Port
	_ = a.store.Update(func(c *Config) error {
		c.Music.Dirs = clean
		c.Music.Port = in.Port
		c.Music.Base = base
		if c.Music.Folder != "" {
			ok := false
			for _, d := range clean {
				if strings.HasPrefix(c.Music.Folder, d+string(os.PathSeparator)) || c.Music.Folder == d {
					ok = true
					break
				}
			}
			if !ok {
				c.Music.Folder = ""
			}
		}
		return nil
	})
	note := ""
	if oldPort != in.Port {
		note = "音源端口改了，重启应用后生效"
	}
	writeJSON(w, 200, map[string]any{"ok": true, "note": note})
}
