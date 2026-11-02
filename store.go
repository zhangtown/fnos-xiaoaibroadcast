package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Account struct {
	User          string `json:"user"`          // 小米账号（手机号/邮箱），仅用于显示
	UserID        string `json:"userId"`        // 小米云 userId
	PassToken     string `json:"passToken"`     // 续期用，等于一张长期票
	DeviceID      string `json:"deviceId"`      // 小米要求的设备指纹（随机生成后固定）
	SSecurity     string `json:"ssecurity"`     // 请求签名密钥
	ServiceToken  string `json:"serviceToken"`  // 短期票
	MicoapiSec   string `json:"micoapiSec,omitempty"`  // MiNA（小爱语音）票
	MicoapiToken string `json:"micoapiToken,omitempty"`
	TokenAt       int64  `json:"tokenAt"`       // 上次换票时间
	LastAuthError string `json:"lastAuthError"` // 上次授权失败原因（给界面提示）
}

func (a Account) LoggedIn() bool { return a.ServiceToken != "" && a.SSecurity != "" }

type Device struct {
	DID   string `json:"did"`
	Name  string `json:"name"`
	Model string `json:"model"`
	MinaID string `json:"minaId,omitempty"`  // MiNA 那边的 deviceID
}

type Schedule struct {
	ID      string `json:"id"`
	Time    string `json:"time"` // "07:00"
	Days    []int  `json:"days"` // 1=周一 … 7=周日
	Text    string `json:"text"` // 说话：文案；音乐：音频地址
	Kind    string `json:"kind"` // say（默认）/ play
	Enabled bool   `json:"enabled"`
}

// Speech 是「故事语音」：默认走小爱自带 TTS，选了 edge 才用微软的合成。
type Speech struct {
	Engine    string `json:"engine"`    // 空 = 小爱自带；openai / edge / tencent
	Voice     string `json:"voice"`     // openai=音色名，tencent=VoiceType，edge=音色 id
	Base      string `json:"base"`      // 语音接口地址，留空就用大模型的
	Key       string `json:"key"`       // 语音接口 Key，留空就用大模型的
	Model     string `json:"model"`     // 模型名，默认 tts-1
	SecretID  string `json:"secretId"`  // 腾讯云 SecretId
	SecretKey string `json:"secretKey"` // 腾讯云 SecretKey
	Region    string `json:"region"`    // 腾讯云地域，默认 ap-shanghai
	ExtBase   string `json:"extBase"`   // 可选：TTS 外链前缀，例如 http://192.168.31.68:8090/music/xiaoai-tts
	ExtDir    string `json:"extDir"`    // 可选：TTS 落盘目录，例如 /vol3/1000/music/xiaoai-tts
	Stream    bool   `json:"stream"`    // 实验：边合成边吐给音箱（依赖音箱支持渐进播放）
}

type LLM struct {
	Base      string `json:"base"`      // OpenAI 兼容地址，如 https://api.deepseek.com/v1
	Key       string `json:"key"`       // API Key
	Model     string `json:"model"`     // 模型名，如 deepseek-chat
	MaxTokens int    `json:"maxTokens"` // 输出上限（0 = 不发送）
	Effort    string `json:"effort"`    // 思考强度："" 不发送 / low / medium / high / none
}

type Options struct {
	Volume int `json:"volume"` // 0 = 不改音量；1-100 = 播报前先调到该值
	Retry  int `json:"retry"`  // 失败重试次数
}

type LogEntry struct {
	At   int64  `json:"at"`
	Kind string `json:"kind"` // 定时 / 试播 / 系统
	Text string `json:"text"`
	OK   bool   `json:"ok"`
	Msg  string `json:"msg"`
}

type Config struct {
	Version   int        `json:"version"`
	Account   Account    `json:"account"`
	Device    Device     `json:"device"`
	Devices   []Device   `json:"devices"` // 上次拉到的设备列表（缓存，页面刷新后下拉框还能用）
	Schedules []Schedule `json:"schedules"`
	Options   Options    `json:"options"`
	Music     MusicLib   `json:"music"`
	LLM       LLM        `json:"llm"`
	Speech    Speech     `json:"speech"`
}

type Store struct {
	mu       sync.Mutex
	dir      string
	cfgPath  string
	logPath  string
	cfg      *Config
	logs     []LogEntry
	firedAt  map[string]bool // 幂等：防止同一分钟重复播报
	maxLogs  int
	dirtyLog bool
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		dir:     dir,
		cfgPath: filepath.Join(dir, "config.json"),
		logPath: filepath.Join(dir, "logs.jsonl"),
		firedAt: map[string]bool{},
		maxLogs: 200,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func defaults() *Config {
	return &Config{
		Version: 1,
		Options: Options{Volume: 0, Retry: 1},
		Music:   defaultMusicLib(),
		LLM:      LLM{Base: "https://api.deepseek.com/v1", Model: "deepseek-chat", MaxTokens: 800},
		Speech:  Speech{Engine: "", Voice: "zh-CN-XiaoxiaoNeural"},
		Schedules: []Schedule{{
			ID:      "morning",
			Time:    "07:00",
			Days:    []int{1, 2, 3, 4, 5},
			Text:    "早上好！出门前记得三件事：带汗巾、吃维生素、抹油油。",
			Enabled: true,
		}},
	}
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.cfgPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		s.cfg = defaults()
		if err := s.saveLocked(); err != nil {
			return err
		}
	} else {
		var c Config
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("config.json 解析失败：%w", err)
		}
		if c.Schedules == nil {
			c.Schedules = []Schedule{}
		}
		// 2.6 起播报表只做播报：历史里「放音乐 / 讲故事」的条目先停用，避免到点念出地址或提示词
		for i := range c.Schedules {
			if c.Schedules[i].Kind != "" && c.Schedules[i].Kind != "say" {
				c.Schedules[i].Kind = "say"
				c.Schedules[i].Enabled = false
			}
		}
		if c.Music.Port == 0 && len(c.Music.Dirs) == 0 {
			c.Music = defaultMusicLib()
		}
		if c.Music.Token == "" {
			c.Music.Token = randomToken(10)
		}
		if len(c.Music.Dirs) == 0 {
			c.Music.Dirs = defaultMusicLib().Dirs
		}
		// 18090 在用户 NAS 上被 1Panel 占了，老配置自动迁到 18095
		if c.Music.Port == 18090 {
			c.Music.Port = 18095
		}
		s.cfg = &c
	}
	s.loadLogs()
	return nil
}

func (s *Store) loadLogs() {
	f, err := os.Open(s.logPath)
	if err != nil {
		return
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var e LogEntry
		if err := dec.Decode(&e); err != nil {
			break
		}
		s.logs = append(s.logs, e)
	}
	if n := len(s.logs); n > s.maxLogs {
		s.logs = s.logs[n-s.maxLogs:]
	}
}

func (s *Store) Cfg() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Get 返回配置副本，供并发读取（调度器与 HTTP 处理同时跑）。
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *s.cfg
	cp.Schedules = append([]Schedule(nil), s.cfg.Schedules...)
	cp.Devices = append([]Device(nil), s.cfg.Devices...)
	return cp
}

func (s *Store) Snapshot() (Config, []LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *s.cfg
	cp.Schedules = append([]Schedule(nil), s.cfg.Schedules...)
	cp.Devices = append([]Device(nil), s.cfg.Devices...)
	logs := append([]LogEntry(nil), s.logs...)
	sort.Slice(logs, func(i, j int) bool { return logs[i].At > logs[j].At })
	return cp, logs
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.cfgPath); err != nil {
		return err
	}
	return os.Chmod(s.cfgPath, 0o600)
}

// Update 在一把锁里改配置并落盘。
func (s *Store) Update(fn func(c *Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(s.cfg); err != nil {
		return err
	}
	return s.saveLocked()
}

func (s *Store) AddLog(kind, text string, ok bool, msg string) {
	e := LogEntry{At: time.Now().Unix(), Kind: kind, Text: text, OK: ok, Msg: msg}
	s.mu.Lock()
	s.logs = append(s.logs, e)
	if len(s.logs) > s.maxLogs {
		s.logs = s.logs[len(s.logs)-s.maxLogs:]
	}
	s.mu.Unlock()

	if f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		if b, err := json.Marshal(e); err == nil {
			f.Write(append(b, '\n'))
		}
		f.Close()
	}
}

// markFired 返回 true 表示这一次是首次触发（同一分钟内重复 tick 会被挡住）。
func (s *Store) markFired(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firedAt[key] {
		return false
	}
	s.firedAt[key] = true
	// 只留当天的键，避免无限增长
	today := time.Now().Format("2006-01-02")
	for k := range s.firedAt {
		if !strings.Contains(k, today) {
			delete(s.firedAt, k)
		}
	}
	return true
}

// NextRuns 每条播报只出一行：它的下一次时间 + 规律描述，避免同一句文案刷一屏。
func NextRuns(cfg Config, n int) []map[string]any {
	if n <= 0 {
		n = 12
	}
	now := time.Now()
	type hit struct {
		at time.Time
		sc Schedule
	}
	var hits []hit
	for _, sc := range cfg.Schedules {
		if !sc.Enabled {
			continue
		}
		if at, ok := nextAt(sc, now); ok {
			hits = append(hits, hit{at: at, sc: sc})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].at.Before(hits[j].at) })
	out := make([]map[string]any, 0, n)
	for i := 0; i < len(hits) && i < n; i++ {
		sc := hits[i].sc
		out = append(out, map[string]any{
			"id":    sc.ID,
			"at":    hits[i].at.Unix(),
			"label": relLabel(hits[i].at, now),
			"time":  sc.Time,
			"days":  daysText(sc.Days),
			"kind":  kindOf(sc),
			"text":  scheduleText(sc),
		})
	}
	return out
}

func kindOf(sc Schedule) string {
	switch sc.Kind {
	case "play", "story":
		return sc.Kind
	}
	return "say"
}

// scheduleText 是给界面看的一句话：说话就是文案，音乐是文件名，故事是提示词。
func scheduleText(sc Schedule) string {
	switch kindOf(sc) {
	case "story":
		if p := strings.TrimSpace(sc.Text); p != "" {
			return "📖 " + p
		}
		return "📖 随便讲一个童话"
	case "play":
		u := sc.Text
		if i := strings.LastIndexAny(u, "/\\"); i >= 0 && i < len(u)-1 {
			u = u[i+1:]
		}
		if i := strings.IndexByte(u, '?'); i > 0 {
			u = u[:i]
		}
		return "🎵 " + u
	}
	return sc.Text
}
// nextAt 找这条播报的下一次触发时间。
func nextAt(sc Schedule, now time.Time) (time.Time, bool) {
	hh, mm, ok := parseHM(sc.Time)
	if !ok {
		return time.Time{}, false
	}
	for d := 0; d < 8; d++ {
		day := now.AddDate(0, 0, d)
		iso := int(day.Weekday())
		if iso == 0 {
			iso = 7
		}
		if !hasDay(sc.Days, iso) {
			continue
		}
		at := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, time.Local)
		if at.Before(now.Add(20 * time.Second)) {
			continue
		}
		return at, true
	}
	return time.Time{}, false
}

// relLabel 「今天 07:40」/「明天 07:40」/「周三 07:40」/「11-08 07:40」
func relLabel(at, now time.Time) string {
	hm := at.Format("15:04")
	d0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	days := int(at.Sub(d0).Hours() / 24)
	switch {
	case days <= 0:
		return "今天 " + hm
	case days == 1:
		return "明天 " + hm
	case days < 7:
		return []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[int(at.Weekday())] + " " + hm
	default:
		return at.Format("01-02") + " " + hm
	}
}

// daysText 把星期集合讲成「每天 / 工作日 / 周末 / 周一、周三」。
func daysText(days []int) string {
	set := map[int]bool{}
	for _, d := range days {
		if d >= 1 && d <= 7 {
			set[d] = true
		}
	}
	if len(set) == 7 {
		return "每天"
	}
	if len(set) == 5 && set[1] && set[2] && set[3] && set[4] && set[5] {
		return "工作日"
	}
	if len(set) == 2 && set[6] && set[7] {
		return "周末"
	}
	names := []string{"", "周一", "周二", "周三", "周四", "周五", "周六", "周日"}
	var parts []string
	for d := 1; d <= 7; d++ {
		if set[d] {
			parts = append(parts, names[d])
		}
	}
	if len(parts) == 0 {
		return "未选"
	}
	return strings.Join(parts, "、")
}

func hasDay(days []int, d int) bool {
	for _, v := range days {
		if v == d {
			return true
		}
	}
	return false
}

func parseHM(s string) (int, int, bool) {
	var hh, mm int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &hh, &mm); err != nil {
		return 0, 0, false
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}
