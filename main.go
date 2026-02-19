package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

const (
	ListenAddr      = ":8081"
	GatewayURL      = "https://ai-gateway.qqq-7fd.workers.dev/v1/chat/completions"
	GatewayAPIKey   = "xlt2026"
	SummarizeModel  = "gpt-4o-mini"
	MaxRecentMsgs   = 10
	MinMsgsToSum    = 11 // 超过此数量才摘要
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string     `json:"model"`
	Messages []Message  `json:"messages"`
}

type Choice struct {
	Message Message `json:"message"`
}

type ChatResponse struct {
	Choices []Choice `json:"choices"`
}

func summarizeEarly(messages []Message) (string, error) {
	// 取最早的 1/3 消息内容
	n := len(messages) / 3
	if n < 1 {
		n = 1
	}
	texts := make([]string, 0, n)
	for i := 0; i < n && i < len(messages); i++ {
		texts = append(texts, messages[i].Content)
	}
	joint := strings.Join(texts, "\n---\n")
	if len(joint) > 12000 {
		joint = joint[:12000] + "..."
	}

	// 调用网关生成摘要
	req := ChatRequest{
		Model: SummarizeModel,
		Messages: []Message{
			{Role: "system", Content: "用一句话总结以下对话的核心内容，不超过100字，保留关键事实和数字。"},
			{Role: "user", Content: joint},
		},
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest("POST", GatewayURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+GatewayAPIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gateway status %d", resp.StatusCode)
	}
	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", err
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("empty choices")
	}
	sum := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	if len(sum) > 150 {
		sum = sum[:150] + "..."
	}
	return sum, nil
}

func compressMessages(msgs []Message) []Message {
	if len(msgs) < MinMsgsToSum {
		return msgs
	}
	sum, err := summarizeEarly(msgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "摘要失败: %v\n", err)
		return msgs // 失败则返回原消息
	}
	// 保留最近 MaxRecentMsgs 条
	start := len(msgs) - MaxRecentMsgs
	if start < 0 {
		start = 0
	}
	recent := msgs[start:]
	// 前置 system 摘要
	compressed := []Message{
		{Role: "system", Content: "[历史摘要] " + sum},
	}
	compressed = append(compressed, recent...)
	return compressed
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Messages) > MinMsgsToSum {
		req.Messages = compressMessages(req.Messages)
	}
	newBody, _ := json.Marshal(req)
	httpReq, err := http.NewRequest("POST", GatewayURL, bytes.NewReader(newBody))
	if err != nil {
		http.Error(w, "Upstream error", http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+GatewayAPIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		http.Error(w, "Upstream error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	io.Copy(w, resp.Body)
}

func main() {
	log.Println("Zepto Proxy started on", ListenAddr, "→", GatewayURL)
	if err := http.ListenAndServe(ListenAddr, http.HandlerFunc(proxyHandler)); err != nil {
		log.Fatal("Listen error:", err)
	}
}
