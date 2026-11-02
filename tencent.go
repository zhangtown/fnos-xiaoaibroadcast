// 腾讯云语音合成：一次 TC3-HMAC-SHA256 签名的 POST，返回 base64 音频。
//
// 为什么单独写：腾讯云不是 OpenAI 兼容接口，签名规则也不同（TC3），但它的合成延迟在几百毫秒级，
// 比目前这条云端 CosyVoice2 快一个数量级，适合「点一下就要出声」的场景。
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	tencentHost    = "tts.tencentcloudapi.com"
	tencentService = "tts"
	tencentVersion = "2019-08-23"
	tencentAction  = "TextToVoice"
)

// 腾讯云的预置音色（VoiceType）。名字取自控制台，具体可用性以账号开通的套餐为准。
var tencentVoices = []edgeVoice{
	{"101001", "智瑜 · 女声 · 通用"},
	{"101002", "智聆 · 女声 · 通用"},
	{"101003", "智美 · 女声 · 客服"},
	{"101004", "智云 · 男声 · 通用"},
	{"101005", "智莉 · 女声 · 通用"},
	{"101008", "智甜 · 女声 · 童声感"},
	{"101009", "智爽 · 男声 · 活泼"},
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// tencentSynth 合成一段语音，返回 mp3 字节。
func tencentSynth(cfg Config, text, voice string) ([]byte, error) {
	id := strings.TrimSpace(cfg.Speech.SecretID)
	key := strings.TrimSpace(cfg.Speech.SecretKey)
	if id == "" || key == "" {
		return nil, fmt.Errorf("还没填腾讯云的 SecretId / SecretKey（设置 → 故事语音）")
	}
	region := strings.TrimSpace(cfg.Speech.Region)
	if region == "" {
		region = "ap-shanghai"
	}
	if voice == "" {
		voice = tencentVoices[0].ID
	}

	var audio bytes.Buffer
	for _, piece := range splitForTTS(text, 140) {
		payload, _ := json.Marshal(map[string]any{
			"Text":      piece,
			"SessionId": requestID(),
			"VoiceType": atoiSafe(voice, 101001),
			"Codec":     "mp3",
			"SampleRate": 16000,
			"ModelType": 1,
		})
		now := time.Now().UTC()
		date := now.Format("2006-01-02")
		ts := fmt.Sprint(now.Unix())

		// 1. 拼接规范请求串
		canonicalHeaders := "content-type:application/json; charset=utf-8\nhost:" + tencentHost + "\n"
		signedHeaders := "content-type;host"
		canonicalRequest := "POST\n/\n\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + sha256Hex(payload)

		// 2. 拼接待签字符串
		scope := date + "/" + tencentService + "/tc3_request"
		stringToSign := "TC3-HMAC-SHA256\n" + ts + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))

		// 3. 逐级派生签名密钥
		secretDate := hmacSHA256([]byte("TC3"+key), []byte(date))
		secretService := hmacSHA256(secretDate, []byte(tencentService))
		secretSigning := hmacSHA256(secretService, []byte("tc3_request"))
		signature := hex.EncodeToString(hmacSHA256(secretSigning, []byte(stringToSign)))

		auth := "TC3-HMAC-SHA256 Credential=" + id + "/" + scope +
			", SignedHeaders=" + signedHeaders + ", Signature=" + signature

		req, err := http.NewRequest(http.MethodPost, "https://"+tencentHost+"/", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Host", tencentHost)
		req.Header.Set("X-TC-Action", tencentAction)
		req.Header.Set("X-TC-Version", tencentVersion)
		req.Header.Set("X-TC-Timestamp", ts)
		req.Header.Set("X-TC-Region", region)

		resp, err := ttsHTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("连不上腾讯云 TTS：%v", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("腾讯云返回 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 240))
		}

		var out struct {
			Response struct {
				Audio     string `json:"Audio"`
				Error     *struct {
					Code    string `json:"Code"`
					Message string `json:"Message"`
				} `json:"Error"`
				RequestId string `json:"RequestId"`
			} `json:"Response"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("腾讯云响应解析失败：%s", truncate(string(raw), 240))
		}
		if e := out.Response.Error; e != nil && e.Code != "" {
			return nil, fmt.Errorf("腾讯云拒绝（%s）：%s", e.Code, e.Message)
		}
		if out.Response.Audio == "" {
			return nil, fmt.Errorf("腾讯云没给音频：%s", truncate(string(raw), 200))
		}
		chunk, err := base64.StdEncoding.DecodeString(out.Response.Audio)
		if err != nil {
			return nil, fmt.Errorf("音频解码失败：%v", err)
		}
		audio.Write(chunk)
	}
	return audio.Bytes(), nil
}

func atoiSafe(s string, def int) int {
	n := 0
	ok := false
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
		ok = true
	}
	if !ok {
		return def
	}
	return n
}
