// 小爱播报 —— 飞牛 fnOS 内嵌应用。
//
// 进程跑在 unix socket 上（$TRIM_APPDEST/app.sock），由飞牛统一网关
// 以 /app/xiaoai-broadcast 前缀反代；不额外占用端口。
//
// 出声链路：MiNA（小爱语音云）优先，失败退回 MIoT 的 5-3。
// 凭据来自 NAS 上 xiaomusic 的 .mi.token（点「重新同步票据」或首次试播时自动读）。
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

var version = "dev"

type App struct {
	store *Store
	xm    *XM
}

func main() {
	var httpAddr string
	flag.StringVar(&httpAddr, "http", "", "调试用：直接监听 TCP 地址（生产走 unix socket）")
	flag.Parse()

	store, err := NewStore(dataDir())
	if err != nil {
		log.Fatalf("初始化数据目录 %s 失败：%v", dataDir(), err)
	}
	app := &App{store: store, xm: NewXM(store)}
	store.AddLog("系统", "", true, "应用启动，版本 "+version)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.scheduleLoop(ctx)
	app.startMusicServer()
	app.seedStories()

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/api/health", app.handleHealth)
	mux.HandleFunc("/api/state", app.handleState)
	mux.HandleFunc("/api/sync", app.handleSync)
	mux.HandleFunc("/api/device", app.handleDevice)
	mux.HandleFunc("/api/devices", app.handleDevices)
	mux.HandleFunc("/api/schedules", app.handleSchedules)
	mux.HandleFunc("/api/options", app.handleOptions)
	mux.HandleFunc("/api/test", app.handleTest)
	mux.HandleFunc("/api/play", app.handlePlay)
	mux.HandleFunc("/api/stop", app.handleStop)
	mux.HandleFunc("/api/volume", app.handleVolume)
	mux.HandleFunc("/api/music/list", app.handleMusicList)
	mux.HandleFunc("/api/music/folders", app.handleMusicFolders)
	mux.HandleFunc("/api/music/folder", app.handleMusicFolder)
	mux.HandleFunc("/api/music/settings", app.handleMusicSettings)
	mux.HandleFunc("/api/story/generate", app.handleStoryGenerate)
	mux.HandleFunc("/api/story/speak", app.handleStorySpeak)
	mux.HandleFunc("/api/classics", app.handleClassics)
	mux.HandleFunc("/api/classic", app.handleClassicGet)
	mux.HandleFunc("/api/llm", app.handleLLM)
	mux.HandleFunc("/api/llm/test", app.handleLLMTest)

	srv := &http.Server{Handler: withPrefix(mux), ReadHeaderTimeout: 10 * time.Second}

	if httpAddr != "" {
		log.Printf("调试模式：监听 %s", httpAddr)
		log.Fatal(srv.ListenAndServe())
	}

	socketPath := os.Getenv("SOCKET_PATH")
	if socketPath == "" {
		log.Fatal("缺少 SOCKET_PATH（应由 fpk 生命周期脚本注入）")
	}
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("监听 unix socket %s 失败：%v", socketPath, err)
	}
	_ = os.Chmod(socketPath, 0o666)
	log.Printf("小爱播报 %s 已就绪，socket=%s prefix=%s", version, socketPath, os.Getenv("GATEWAY_PREFIX"))

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		cancel()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		srv.Shutdown(sctx)
		_ = os.Remove(socketPath)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("服务退出：%v", err)
	}
}

// withPrefix 容错：有的网关会把 /app/<app> 原样转过来。
func withPrefix(next http.Handler) http.Handler {
	prefix := strings.TrimSuffix(os.Getenv("GATEWAY_PREFIX"), "/")
	if prefix == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, prefix) {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- 页面 ----------

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(indexHTML)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "version": version, "app": "xiaoai-broadcast"})
}

// ---------- 状态 ----------

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	cfg, logs := a.store.Snapshot()
	if len(logs) > 60 {
		logs = logs[:60]
	}
	writeJSON(w, 200, map[string]any{
		"version":   version,
		"now":       time.Now().Format("2006-01-02 15:04:05"),
		"ready":     cfg.Account.MicoapiToken != "" || cfg.Account.LoggedIn(),
		"micoapi":   cfg.Account.MicoapiToken != "",
		"device":    cfg.Device,
		"devices":   cfg.Devices,
		"schedules": cfg.Schedules,
		"options":   cfg.Options,
		"llm": map[string]any{"ready": cfg.LLM.Key != "", "base": cfg.LLM.Base, "model": cfg.LLM.Model, "maxTokens": cfg.LLM.MaxTokens, "effort": cfg.LLM.Effort, "keyMask": maskKey(cfg.LLM.Key)},
		"storyPrompt": defaultStoryPrompt,
		"musicCfg": map[string]any{"dirs": cfg.Music.Dirs, "port": cfg.Music.Port, "base": cfg.Music.Base},
		"next":      NextRuns(cfg, 4),
		"logs":      logs,
	})
}

// ---------- 票据与设备 ----------

// handleSync 从 xiaomusic 的 .mi.token 重新同步票据，并刷新设备列表。
func (a *App) handleSync(w http.ResponseWriter, r *http.Request) {
	if !readJSON(w, r, &struct{}{}) {
		return
	}
	devs, path, err := a.ImportXiaomusicToken()
	if err != nil {
		a.store.AddLog("系统", "", false, "同步票据失败："+err.Error())
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	a.store.AddLog("系统", "", true, fmt.Sprintf("已同步票据（%s），%d 台设备", path, len(devs)))
	writeJSON(w, 200, map[string]any{"ok": true, "devices": devs})
}

func (a *App) handleDevice(w http.ResponseWriter, r *http.Request) {
	var in Device
	if !readJSON(w, r, &in) {
		return
	}
	_ = a.store.Update(func(c *Config) error {
		c.Device = Device{DID: in.DID, Name: in.Name, Model: in.Model}
		return nil
	})
	a.store.AddLog("系统", "", true, "选择音箱："+in.Name)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := a.xm.Devices()
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "devices": a.store.Get().Devices})
		return
	}
	_ = a.store.Update(func(c *Config) error {
		c.Devices = devs
		return nil
	})
	writeJSON(w, 200, map[string]any{"ok": true, "devices": devs})
}

// ---------- 播报表 ----------

func (a *App) handleSchedules(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Schedules []Schedule `json:"schedules"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	clean := make([]Schedule, 0, len(in.Schedules))
	for _, s := range in.Schedules {
		s.Time = strings.TrimSpace(s.Time)
		if _, _, ok := parseHM(s.Time); !ok {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "时间格式不对：" + s.Time})
			return
		}
		s.Text = strings.TrimSpace(s.Text)
		if s.Text == "" {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "有一条的文案是空的"})
			return
		}
		s.Kind = "say" // 播报表只做播报；音乐和故事走各自的卡片
		if len(s.Days) == 0 {
			s.Days = []int{1, 2, 3, 4, 5, 6, 7}
		}
		if s.ID == "" {
			s.ID = fmt.Sprintf("s%d", time.Now().UnixNano())
		}
		clean = append(clean, s)
	}
	_ = a.store.Update(func(c *Config) error {
		c.Schedules = clean
		return nil
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleOptions(w http.ResponseWriter, r *http.Request) {
	var in Options
	if !readJSON(w, r, &in) {
		return
	}
	if in.Volume < 0 || in.Volume > 100 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "音量要在 0-100 之间"})
		return
	}
	if in.Retry < 0 || in.Retry > 3 {
		in.Retry = 1
	}
	_ = a.store.Update(func(c *Config) error {
		c.Options = in
		return nil
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 播报 / 播放 ----------

func (a *App) handleTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	cfg := a.store.Get()
	if strings.TrimSpace(in.Text) == "" {
		in.Text = "测试一下，我是小爱播报。"
	}
	err := a.speak(cfg, in.Text)
	if err != nil {
		a.store.AddLog("试播", in.Text, false, err.Error())
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	a.store.AddLog("试播", in.Text, true, "已发送给「"+cfg.Device.Name+"」")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handlePlay 手动放一首（音频地址）。
func (a *App) handlePlay(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := a.xm.PlayURL(in.URL, 0); err != nil {
		a.store.AddLog("音乐", in.URL, false, err.Error())
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	a.store.AddLog("音乐", scheduleText(Schedule{Kind: "play", Text: in.URL}), true, "正在播放")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleVolume 立刻把音箱音量设成指定值。
func (a *App) handleVolume(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		v, err := a.xm.CurrentVolume()
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "volume": v})
		return
	}
	var in struct {
		Volume int `json:"volume"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := a.xm.SetVolume(in.Volume); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	a.store.AddLog("系统", "", true, fmt.Sprintf("音量已调到 %d", in.Volume))
	writeJSON(w, 200, map[string]any{"ok": true})
}
// handleStop 停掉当前播放。
func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	if !readJSON(w, r, &struct{}{}) {
		return
	}
	if err := a.xm.StopPlaying(); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	a.store.AddLog("音乐", "", true, "已停止播放")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// speak 把一句话送出去：MiNA 优先（api2.mina.mi.com），只在「根本没送到」时才退回 MIoT。
//
// 这里刻意不做无脑重试：MiNA 有些时候回了非 0 code 但其实已经说完了，
// 再换 MIoT 或再重试就会重复出声——之前「成功了还触发重试」就是这个原因。
func (a *App) speak(cfg Config, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("文案是空的")
	}
	if cfg.Device.DID == "" {
		return fmt.Errorf("还没选音箱：先点「同步票据」拉设备，再选一台")
	}

	err := a.xm.SayViaMina(text)
	if err == nil {
		return nil
	}
	if !deliveryFailed(err) {
		// 音箱已经收到（只是回报了个 code），不再换路、不再重试
		a.store.AddLog("系统", "", true, "MiNA 回报异常但已下发，不再重试："+err.Error())
		return nil
	}
	a.store.AddLog("系统", "", false, "MiNA 没送出去，改用 MIoT："+err.Error())

	// 只换一条路再试一次，不再按「失败重试」次数循环：定时播报早几秒晚几秒没意义，
	// 而重复下发会真的重复出声。
	if err2 := a.xm.Say(cfg.Device.DID, text, 0); err2 != nil {
		return fmt.Errorf("MiNA：%v；MIoT：%v", err, err2)
	}
	return nil
}

// deliveryFailed 判断错误是不是「请求没到/被拒在门口」，这种才值得换条路重发。
func deliveryFailed(err error) bool {
	s := err.Error()
	for _, k := range []string{
		"HTTP 401", "HTTP 403", "HTTP 5", "no such host", "connection refused",
		"connection reset", "timeout", "EOF", "没送出去", "拉设备失败",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// ---------- 调度 ----------

func (a *App) scheduleLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	a.tick(time.Now()) // 启动时先对齐一次
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			a.tick(now)
		}
	}
}

func (a *App) tick(now time.Time) {
	cfg := a.store.Get()
	hm := now.Format("15:04")
	iso := int(now.Weekday())
	if iso == 0 {
		iso = 7
	}
	dayKey := now.Format("2006-01-02")
	for _, sc := range cfg.Schedules {
		if !sc.Enabled || sc.Time != hm || !hasDay(sc.Days, iso) {
			continue
		}
		if !a.store.markFired(sc.ID + "|" + dayKey + "|" + hm) {
			continue
		}
		go a.fire(sc)
	}
}

func (a *App) fire(sc Schedule) {
	cfg := a.store.Get()
	label := scheduleText(sc)
	if label == "" {
		return
	}
	if err := a.speak(cfg, sc.Text); err != nil {
		a.store.AddLog("定时", sc.Text, false, sc.Time+"："+err.Error())
		return
	}
	a.store.AddLog("定时", sc.Text, true, sc.Time+" 已播报")
}

// ---------- 小工具 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"ok": false, "error": "只接受 POST"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "请求体不是合法 JSON：" + err.Error()})
		return false
	}
	return true
}

// dataDir 选数据目录。优先放在用户卷（/vol1/1000/docker）上：
// 飞牛重装同名应用会清掉 $TRIM_PKGVAR，放这里配置和票据才不会被一次升级抹掉。
func dataDir() string {
	if v := os.Getenv("DATA_DIR"); v != "" {
		return v
	}
	for _, p := range []string{"/vol1/1000/docker"} {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return filepath.Join(p, "xiaoai-broadcast")
		}
	}
	if v := os.Getenv("TRIM_PKGVAR"); v != "" {
		return v
	}
	return "data"
}
